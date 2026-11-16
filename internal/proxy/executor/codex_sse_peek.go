// Codex 200-with-an-error peek — port of CodexExecutor._peekSseTransientError in
// open-sse/executors/codex.js.
//
// The ChatGPT Codex backend can answer HTTP 200 and then put the failure in the
// event stream: "Selected model is at capacity. Please try a different model."
// Without reading the first bytes, that arrives at the client as a successful turn
// that emits nothing — no account rotation, no retry, and the operator sees a
// healthy account whose answers are empty.
//
// Two classes, handled differently because the reference does:
//
//	account fallback  the MODEL is out of capacity for this account → the turn must
//	                  rotate, so the reply becomes a retryable 503;
//	overloaded        the upstream is briefly overloaded → the same call is retried
//	                  in place once before the 503.
//
// A stream that matches neither is handed on unchanged: the bytes already read are
// replayed through a MultiReader so the client sees the full stream.
package executor

import (
	"bytes"
	"io"
	"net/http"
	"strings"

	"panrouter/proxy/internal/proxy"
)

const (
	// codexSSEPeekBytes bounds how much of a 200 stream is inspected.
	codexSSEPeekBytes = 256 << 10
	// codexCapacityMessage is what the client and the log see when the model is out
	// of capacity (upstream CODEX_MODEL_CAPACITY_MESSAGE).
	codexCapacityMessage = "Selected model is at capacity. Please try a different model."
	// codexSSEMaxRetries bounds the same-account retries for an overloaded upstream.
	codexSSEMaxRetries = 1
)

// codexSSEAccountFallbackPatterns mean "this account cannot serve the model right
// now": the answer is to rotate, not to wait.
var codexSSEAccountFallbackPatterns = []string{
	"selected model is at capacity",
	"model_at_capacity",
}

// codexSSERetryPatterns mean "the upstream is briefly overloaded": the same call
// gets another attempt before the account is blamed.
var codexSSERetryPatterns = []string{
	"server_is_overloaded",
	"service_unavailable_error",
}

// codexSSEUserOutputPatterns prove the stream is already answering: once one of
// these is seen the peek stops, so a healthy long answer is not buffered to the cap.
var codexSSEUserOutputPatterns = []string{
	"event: response.output_text.delta",
	"event: response.function_call_arguments.delta",
	`"type":"response.output_text.delta"`,
	`"type":"response.function_call_arguments.delta"`,
}

// codexSSEPeek is what the peek found, plus the reader the caller must use from
// then on (the bytes already consumed are replayed).
type codexSSEPeek struct {
	matched string
	message string
	// accountFallback reports that the failure is the account's to rotate away from.
	accountFallback bool
	// reader replays the inspected prefix and continues with the live stream.
	reader io.Reader
}

// codexSSEFailureResponse is the SSE-shaped error the client receives instead of an
// empty 200 (upstream codexSseErrorResponse).
func codexSSEFailureResponse(status int, message string) error {
	errorType := "invalid_request_error"
	if status >= http.StatusInternalServerError {
		errorType = "server_error"
	}
	code := "upstream_error"
	if status == http.StatusServiceUnavailable {
		code = "service_unavailable"
	}
	return proxy.UpstreamFailure(status, message+" ["+errorType+"/"+code+"]")
}

// peekCodexSSE inspects the beginning of a 200 event stream.
func peekCodexSSE(body io.Reader) codexSSEPeek {
	if body == nil {
		return codexSSEPeek{}
	}
	// The raw prefix is kept for replay; a lowercased copy is grown alongside it so
	// each read is matched once instead of re-lowercasing the whole buffer.
	prefix := make([]byte, 0, 4096)
	lower := make([]byte, 0, 4096)
	buf := make([]byte, 4096)
	for len(lower) < codexSSEPeekBytes {
		read, err := body.Read(buf)
		if read > 0 {
			prefix = append(prefix, buf[:read]...)
			for _, b := range buf[:read] {
				lower = append(lower, lowerByte(b))
			}
			text := string(lower)
			for _, pattern := range codexSSEAccountFallbackPatterns {
				if strings.Contains(text, pattern) {
					return codexSSEPeek{
						matched:         pattern,
						message:         codexSSEMessage(text),
						accountFallback: true,
						reader:          io.MultiReader(bytes.NewReader(prefix), body),
					}
				}
			}
			for _, pattern := range codexSSERetryPatterns {
				if strings.Contains(text, pattern) {
					return codexSSEPeek{
						matched: pattern,
						message: codexSSEMessage(text),
						reader:  io.MultiReader(bytes.NewReader(prefix), body),
					}
				}
			}
			for _, pattern := range codexSSEUserOutputPatterns {
				if strings.Contains(text, pattern) {
					// The model is answering: stop early, the rest streams through.
					return codexSSEPeek{reader: io.MultiReader(bytes.NewReader(prefix), body)}
				}
			}
		}
		if err != nil {
			break
		}
	}
	return codexSSEPeek{reader: io.MultiReader(bytes.NewReader(prefix), body)}
}

// lowerByte lowercases one ASCII byte; the patterns are ASCII and the streams are
// UTF-8, so non-ASCII bytes pass through unchanged.
func lowerByte(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}

// codexSSEMessage digs the provider's sentence out of the buffered stream, falling
// back to the capacity wording the page uses.
func codexSSEMessage(lowerText string) string {
	for _, marker := range []string{"selected model is at capacity", "model_at_capacity"} {
		if index := strings.Index(lowerText, marker); index >= 0 {
			return codexCapacityMessage
		}
	}
	return codexCapacityMessage
}

// codexSSELooksLikeStream reports whether a response body should be peeked: only an
// event stream can carry a 200-with-an-error, and a JSON body is left untouched.
func codexSSELooksLikeStream(resp *http.Response) bool {
	if resp == nil || resp.Body == nil || resp.StatusCode != http.StatusOK {
		return false
	}
	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	return strings.Contains(contentType, "text/event-stream")
}

// codexStreamFailure turns a peek verdict into the error the caller returns.
func codexStreamFailure(peek codexSSEPeek) error {
	message := peek.message
	if message == "" {
		message = peek.matched
	}
	return codexSSEFailureResponse(http.StatusServiceUnavailable, message)
}
