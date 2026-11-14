package proxy

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"net/http"
	"strings"
)

// SSEClientFormat picks the client wire format for a terminal frame emitted
// after HTTP 200 already went out: the status code can no longer change, so
// an abort is reportable only in-band (upstream decolua/9router 93001213).
type SSEClientFormat uint8

const (
	SSEFormatOpenAI SSEClientFormat = iota
	SSEFormatClaude
)

type sseErrorPayload struct {
	Error sseErrorDetail `json:"error"`
}

type sseErrorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

func sseErrorInfo(code int) (string, string) {
	switch code {
	case 499:
		return "invalid_request_error", "request_aborted"
	case 502:
		return "server_error", "upstream_error"
	case 504:
		return "server_error", "gateway_timeout"
	default:
		if code >= 500 {
			return "server_error", "internal_server_error"
		}
		return "invalid_request_error", ""
	}
}

func BuildStreamErrorBytes(code int, message string, format SSEClientFormat) []byte {
	typ, errCode := sseErrorInfo(code)
	// A stall is not a generic gateway timeout: the watchdog names the phase and
	// tags the frame, so a client can branch on STREAM_STALL (AGENTS.md §1.4).
	if code == http.StatusGatewayTimeout && errors.Is(errStallCause(message), ErrStreamStall) {
		errCode = StreamStallCode
	}
	payload := sseErrorPayload{Error: sseErrorDetail{Message: message, Type: typ, Code: errCode}}
	encoded, err := json.Marshal(payload)
	if err != nil {
		encoded = []byte(`{"error":{"message":"upstream stream failed","type":"server_error","code":"internal_server_error"}}`)
	}
	if format == SSEFormatClaude {
		ev, _ := json.Marshal(map[string]any{"type": "error", "error": payload.Error})
		return []byte("event: error\ndata: " + string(ev) + "\n\n")
	}
	return []byte("data: " + string(encoded) + "\n\n" + sseDoneFrame)
}

// ClassifyStreamAbort turns a mid-stream read failure into the status and
// message the client sees. A stall watchdog firing and a socket dying are
// different failures and the client should be able to tell them apart.
//
// A stall carries the watchdog's own message, which names the phase and the
// budget ("stream no first chunk (waited 30ms, budget 30ms)") — upstream's
// abortMessage, so a client log can tell slow prefill from a dead stream
// without the numbers being stripped on the way out.
func ClassifyStreamAbort(cause error) (int, string) {
	var stall *StreamStallError
	if errors.As(cause, &stall) {
		return http.StatusGatewayTimeout, stall.Error()
	}
	if errors.Is(cause, ErrStreamStall) {
		return http.StatusGatewayTimeout, ErrStreamStall.Error()
	}
	if errors.Is(cause, context.Canceled) {
		return 499, "upstream request aborted"
	}
	return http.StatusBadGateway, "upstream connection lost"
}

// errStallCause reports ErrStreamStall when the abort message is the watchdog's,
// so the frame builder can tag the code without a second error value threaded
// through every call site. Two shapes exist: the sentinel prose (a bare
// ErrStreamStall, no phase information) and a StreamStallError message, which
// starts with "stream <phase> " and carries the numbers.
func errStallCause(message string) error {
	if strings.Contains(message, ErrStreamStall.Error()) || isStreamStallMessage(message) {
		return ErrStreamStall
	}
	return nil
}

// isStreamStallMessage matches the messages StreamStallError produces. The
// phase names are compile-time constants, so the prefixes fold at build time
// and the check cannot drift from the error type's own formatting.
func isStreamStallMessage(message string) bool {
	switch {
	case strings.HasPrefix(message, "stream "+stallPhaseFirstChunk+" "),
		strings.HasPrefix(message, "stream "+stallPhaseAfterFirstChunk+" "):
		return true
	}
	return false
}
