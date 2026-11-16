// Codex stream peek — parity with CodexExecutor._peekSseTransientError in
// open-sse/executors/codex.js and tests/unit/codex-fast-capacity.test.js.
//
// The ChatGPT Codex backend can answer HTTP 200 and put the failure in the stream:
// "Selected model is at capacity. Please try a different model." Without the peek
// that reaches the client as a successful turn that emits nothing — no rotation, no
// retry, and an operator looking at a healthy account whose answers are empty.
package executor

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func codexStreamBody(chunks ...string) io.Reader {
	return strings.NewReader(strings.Join(chunks, ""))
}

func TestCodexPeekDetectsCapacityAsAccountFallback(t *testing.T) {
	body := codexStreamBody(
		"data: {\"type\":\"response.created\"}\n\n",
		"data: {\"error\":{\"message\":\"Selected model is at capacity. Please try a different model.\"}}\n\n",
	)
	peek := peekCodexSSE(body)
	if !peek.accountFallback {
		t.Fatalf("peek = %+v, want the account fallback verdict", peek)
	}
	if peek.matched != "selected model is at capacity" {
		t.Errorf("matched = %q", peek.matched)
	}
	if peek.message != codexCapacityMessage {
		t.Errorf("message = %q, want the reference wording", peek.message)
	}
}

func TestCodexPeekDetectsTheSnakeCaseMarker(t *testing.T) {
	peek := peekCodexSSE(codexStreamBody(`{"error":{"code":"model_at_capacity"}}`))
	if !peek.accountFallback {
		t.Fatalf("peek = %+v, want the account fallback verdict for model_at_capacity", peek)
	}
}

func TestCodexPeekRetriesAnOverloadedUpstream(t *testing.T) {
	peek := peekCodexSSE(codexStreamBody(`{"type":"error","error":{"code":"server_is_overloaded"}}`))
	if peek.matched != "server_is_overloaded" {
		t.Fatalf("peek = %+v, want the overload pattern", peek)
	}
	if peek.accountFallback {
		t.Error("an overloaded upstream is retryable on the same account, not a rotation")
	}
}

func TestCodexPeekReplaysAHealthyStream(t *testing.T) {
	// A healthy stream must reach the client byte for byte: the peek reads a prefix
	// and the reader replays it.
	chunks := []string{
		"event: response.output_text.delta\ndata: {\"delta\":\"Hel\"}\n\n",
		"event: response.output_text.delta\ndata: {\"delta\":\"lo\"}\n\n",
		"data: [DONE]\n\n",
	}
	peek := peekCodexSSE(codexStreamBody(chunks...))
	if peek.matched != "" {
		t.Fatalf("a healthy stream matched %q", peek.matched)
	}
	got, err := io.ReadAll(peek.reader)
	if err != nil {
		t.Fatalf("read replayed stream: %v", err)
	}
	if want := strings.Join(chunks, ""); string(got) != want {
		t.Errorf("replayed stream differs:\n got: %q\nwant: %q", got, want)
	}
}

func TestCodexPeekStopsEarlyOnUserOutput(t *testing.T) {
	// The model is answering: the peek must stop rather than buffer the whole answer
	// to the 256 KiB cap.
	big := strings.Repeat("x", 128<<10)
	body := codexStreamBody(
		"event: response.output_text.delta\ndata: {\"delta\":\""+big+"\"}\n\n",
		"trailing",
	)
	peek := peekCodexSSE(body)
	if peek.matched != "" {
		t.Fatalf("matched %q on a stream that is answering", peek.matched)
	}
	got, err := io.ReadAll(peek.reader)
	if err != nil {
		t.Fatalf("read replayed stream: %v", err)
	}
	if !strings.Contains(string(got), "trailing") {
		t.Error("the replayed stream lost the bytes after the peek")
	}
}

func TestCodexSSELooksLikeStream(t *testing.T) {
	sse := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream; charset=utf-8"}}, Body: io.NopCloser(strings.NewReader(""))}
	jsonBody := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader("{}"))}
	errorStatus := &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(""))}

	if !codexSSELooksLikeStream(sse) {
		t.Error("an event stream must be peeked")
	}
	if codexSSELooksLikeStream(jsonBody) {
		t.Error("a JSON body carries no stream to peek")
	}
	if codexSSELooksLikeStream(errorStatus) {
		t.Error("a non-200 is already an error; the peek is for 200-with-an-error")
	}
}

func TestCodexStreamFailureIsRetryable(t *testing.T) {
	err := codexStreamFailure(codexSSEPeek{matched: "selected model is at capacity", message: codexCapacityMessage, accountFallback: true})
	if err == nil {
		t.Fatal("a capacity stream must surface as an error")
	}
	if !strings.Contains(err.Error(), codexCapacityMessage) {
		t.Errorf("error = %v, want the reference wording", err)
	}
	if !strings.Contains(err.Error(), "service_unavailable") {
		t.Errorf("error = %v, want the retryable code the router rotates on", err)
	}
}
