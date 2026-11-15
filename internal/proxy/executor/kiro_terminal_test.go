// Kiro terminal integrity — parity with open-sse/executors/kiro.js
// (messageStopEvent / metadataEvent / metricsEvent / EventStream error frames) and
// tests/unit/kiro-terminal-integrity.test.js.
//
// The failure this pins is silence: a stream that ends without a stop reason, or
// ends with an error frame, used to be reported as a clean short answer. The client
// cannot tell that from a complete one, so it never retries and the operator sees a
// healthy account.
package executor

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"panrouter/proxy/internal/providers"
)

// kiroStreamBody runs one Kiro stream made of the given frames and returns the SSE
// body the client would see.
func kiroStreamBody(t *testing.T, frames ...[]byte) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		w.WriteHeader(http.StatusOK)
		for _, frame := range frames {
			_, _ = w.Write(frame)
		}
	}))
	defer srv.Close()

	rec := httptest.NewRecorder()
	err := ForwardKiro(rec, &Request{
		Client:    srv.Client(),
		Config:    &providers.ProviderConfig{BaseURL: srv.URL, AuthHeader: "Authorization", AuthScheme: "bearer"},
		Provider:  "kiro",
		APIKey:    "token",
		Body:      []byte(`{"model":"claude-sonnet-4.5","messages":[{"role":"user","content":"hi"}],"stream":true}`),
		IsStream:  true,
		ModelName: "claude-sonnet-4.5",
	})
	if err != nil {
		t.Fatalf("ForwardKiro: %v", err)
	}
	return rec.Body.String()
}

// kiroFinishReasonOf reads the finish_reason of the closing chunk.
func kiroFinishReasonOf(t *testing.T, body string) string {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		var chunk struct {
			Choices []struct {
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) > 0 && chunk.Choices[0].FinishReason != nil {
			return *chunk.Choices[0].FinishReason
		}
	}
	return ""
}

func TestKiroStreamStopReasonDrivesFinishReason(t *testing.T) {
	tests := []struct {
		name   string
		frames []([]byte)
		want   string
	}{
		{
			name: "max_tokens is a truncated answer, not a clean stop",
			frames: []([]byte){
				kiroEventFrame("assistantResponseEvent", `{"content":"partial"}`),
				kiroEventFrame("messageStopEvent", `{"stopReason":"max_tokens"}`),
			},
			want: "length",
		},
		{
			name: "tool_use asks the client to dispatch the call",
			frames: []([]byte){
				kiroEventFrame("assistantResponseEvent", `{"content":"running"}`),
				kiroEventFrame("toolUseEvent", `{"name":"read_file","toolUseId":"tu-1","input":{"path":"a"},"stop":true}`),
				kiroEventFrame("messageStopEvent", `{"stopReason":"tool_use"}`),
			},
			want: "tool_calls",
		},
		{
			name: "a refusal is surfaced, not smoothed over",
			frames: []([]byte){
				kiroEventFrame("messageStopEvent", `{"stopReason":"refusal"}`),
			},
			want: "content_filter",
		},
		{
			name: "no stop event at all still closes the turn",
			frames: []([]byte){
				kiroEventFrame("assistantResponseEvent", `{"content":"hi"}`),
			},
			want: "stop",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := kiroFinishReasonOf(t, kiroStreamBody(t, tt.frames...)); got != tt.want {
				t.Errorf("finish_reason = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestKiroStreamErrorFrameIsReported(t *testing.T) {
	frame := providers.EncodeEventFrame(map[string]string{
		":message-type":   "exception",
		":exception-type": "throttlingException",
	}, []byte(`{"message":"Rate exceeded"}`))
	body := kiroStreamBody(t, frame)

	if !strings.Contains(body, "kiro_upstream_eventstream_error") {
		t.Fatalf("an EventStream exception must reach the client as an error:\n%s", body)
	}
	if !strings.Contains(body, "Rate exceeded") {
		t.Errorf("the upstream message must be forwarded:\n%s", body)
	}
}

func TestKiroStreamStripsInlineThinkingMarkup(t *testing.T) {
	// Kiro sometimes wraps reasoning in <thinking> tags inside the assistant event.
	body := kiroStreamBody(t,
		kiroEventFrame("assistantResponseEvent", `{"content":"<thinking>secret plan</thinking>Hello"}`),
		kiroEventFrame("messageStopEvent", `{"stopReason":"end_turn"}`),
	)
	if strings.Contains(body, "secret plan") {
		t.Errorf("the thinking markup leaked into the answer:\n%s", body)
	}
	if !strings.Contains(body, "Hello") {
		t.Errorf("the answer was dropped with the markup:\n%s", body)
	}
}

func TestKiroStreamMetricsCarryRealTokenCounts(t *testing.T) {
	frames := [][]byte{
		kiroEventFrame("assistantResponseEvent", `{"content":"hi"}`),
		kiroEventFrame("metricsEvent", `{"metricsEvent":{"inputTokens":1234,"outputTokens":56}}`),
		kiroEventFrame("messageStopEvent", `{"stopReason":"end_turn"}`),
	}
	body := kiroStreamBody(t, frames...)
	if kiroFinishReasonOf(t, body) != "stop" {
		t.Fatalf("the metrics event must not disturb the finish reason:\n%s", body)
	}

	input, output, ok := kiroMetricsTokens([]byte(`{"inputTokens":1234,"outputTokens":56}`))
	if !ok || input != 1234 || output != 56 {
		t.Errorf("top-level metrics = %d/%d ok=%v, want 1234/56", input, output, ok)
	}
	// The usage record must prefer these numbers over the len(content)/4 estimate,
	// which undercounts a tool-only turn and every non-ASCII answer.
	if input, output, ok := kiroMetricsTokens([]byte(`{"metricsEvent":{"inputTokens":7,"outputTokens":9}}`)); !ok || input != 7 || output != 9 {
		t.Errorf("nested metrics = %d/%d ok=%v, want 7/9", input, output, ok)
	}
}

func TestKiroStopReasonMergingKeepsTheSevereVerdict(t *testing.T) {
	tests := []struct {
		name     string
		current  string
		incoming string
		want     string
	}{
		{name: "truncation outranks a clean stop", current: kiroStopEndTurn, incoming: kiroStopMaxTokens, want: kiroStopMaxTokens},
		{name: "a clean stop cannot overwrite truncation", current: kiroStopMaxTokens, incoming: kiroStopEndTurn, want: kiroStopMaxTokens},
		{name: "a refusal outranks truncation", current: kiroStopMaxTokens, incoming: "refusal", want: "refusal"},
		{name: "tool use outranks a plain stop", current: kiroStopEndTurn, incoming: kiroStopToolUse, want: kiroStopToolUse},
		{name: "empty incoming is ignored", current: kiroStopToolUse, incoming: "", want: kiroStopToolUse},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := kiroMergeStopReason(tt.current, tt.incoming); got != tt.want {
				t.Errorf("merge(%q, %q) = %q, want %q", tt.current, tt.incoming, got, tt.want)
			}
		})
	}

	if got := kiroNormalizeStopReason("", true); got != kiroStopToolUse {
		t.Errorf("an unnamed stop after a tool call = %q, want tool_use", got)
	}
	if got := kiroNormalizeStopReason("MAX_TOKENS", false); got != kiroStopMaxTokens {
		t.Errorf("case-insensitive mapping failed: %q", got)
	}
}
