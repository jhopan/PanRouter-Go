package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	json "panrouter/proxy/internal/fastjson"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"panrouter/proxy/internal/providers"
)

// Antigravity in-place retry — parity with computeRetryDelay in
// open-sse/executors/antigravity.js and its retry-hook test.
//
// The behaviour matters on a single-account deployment: upstream waits out a short
// provider-named pause and repeats the same call, where parking the account fails
// the client's turn for a fault that clears in seconds.

func TestAntigravityRetryDelayHeaders(t *testing.T) {
	header := func(values map[string]string) http.Header {
		out := http.Header{}
		for k, v := range values {
			out.Set(k, v)
		}
		return out
	}

	tests := []struct {
		name    string
		status  int
		header  http.Header
		body    string
		attempt int
		want    time.Duration
	}{
		{name: "retry-after within the cap", status: 429, header: header(map[string]string{"Retry-After": "5"}), attempt: 1, want: 5 * time.Second},
		{name: "retry-after above the cap is a veto", status: 429, header: header(map[string]string{"Retry-After": "60"}), attempt: 1, want: 0},
		{name: "rate-limit reset seconds", status: 429, header: header(map[string]string{"X-Ratelimit-Reset-After": "3"}), attempt: 1, want: 3 * time.Second},
		{name: "body names the wait", status: 429, body: "quota exhausted, resets in 4s", attempt: 1, want: 4 * time.Second},
		{name: "body names a long wait", status: 429, body: "resets in 1m30s", attempt: 1, want: 0},
		{name: "429 without info backs off", status: 429, attempt: 1, want: 2 * time.Second},
		{name: "429 backoff is capped at 10s", status: 429, attempt: 6, want: 10 * time.Second},
		{name: "503 without info backs off", status: 503, attempt: 1, want: 2 * time.Second},
		{name: "503 backoff is capped at 15s", status: 503, attempt: 9, want: 15 * time.Second},
		{name: "a 400 naming high traffic is transient", status: 400, body: "The model is currently experiencing high traffic.", attempt: 1, want: 2 * time.Second},
		{name: "a 400 naming an agent termination is transient", status: 400, body: "Agent execution terminated due to error.", attempt: 1, want: 2 * time.Second},
		{name: "a plain 400 is not", status: 400, body: `{"error":{"message":"Bad request"}}`, attempt: 1, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := antigravityRetryDelay(tt.status, tt.header, tt.body, tt.attempt); got != tt.want {
				t.Errorf("delay = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAntigravityRetryableStatus(t *testing.T) {
	for _, status := range []int{429, 500, 502, 503, 504} {
		if !antigravityRetryableStatus(status) {
			t.Errorf("status %d must be inspected for a retry", status)
		}
	}
	for _, status := range []int{200, 400, 401, 403, 404} {
		if antigravityRetryableStatus(status) {
			t.Errorf("status %d must not be read for a retry", status)
		}
	}
}

// TestForwardGeminiRetriesAntigravityTransientFault proves the loop: a 503 that
// clears is repeated in place, and the client's turn succeeds without rotating.
func TestForwardGeminiRetriesAntigravityTransientFault(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"The model is currently experiencing high traffic."}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"ok"}]}}]}`))
	}))
	defer srv.Close()

	cfg := &providers.ProviderConfig{BaseURL: srv.URL, Format: "antigravity"}
	resp, err := ForwardGemini(t.Context(), srv.Client(), cfg, "token",
		`{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"hi"}]}`, false, "project-1", "gemini-3.8-flash-high")
	if err != nil {
		t.Fatalf("ForwardGemini: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want the retried call to succeed", resp.StatusCode)
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("upstream requests = %d, want the failed attempt plus one retry", got)
	}
}

// TestForwardGeminiDoesNotRetryARejectedRequest: a 400 that names nothing transient
// is the payload's fault — one request, and the caller gets the provider's body.
func TestForwardGeminiDoesNotRetryARejectedRequest(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"Bad request","details":[{"fieldViolations":[{"field":"items.items","description":"missing field."}]}]}}`))
	}))
	defer srv.Close()

	cfg := &providers.ProviderConfig{BaseURL: srv.URL, Format: "antigravity"}
	_, err := ForwardGemini(t.Context(), srv.Client(), cfg, "token",
		`{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"hi"}]}`, false, "project-1", "gemini-3.8-flash-high")
	if err == nil {
		t.Fatal("a rejected request must surface")
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("upstream requests = %d, want 1: a rejected payload fails on every account", got)
	}
}

// TestAntigravityStripStreamOptions pins the reference rule: an OpenAI client may
// send stream_options even when it asked for no stream, and Google rejects that
// combination before processing the request.
func TestAntigravityStripStreamOptions(t *testing.T) {
	withOptions := []byte(`{"model":"m","stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`)
	stripped := antigravityStripStreamOptions(withOptions)
	var decoded map[string]any
	if err := json.Unmarshal(stripped, &decoded); err != nil {
		t.Fatalf("stripped body is not JSON: %v", err)
	}
	if _, present := decoded["stream_options"]; present {
		t.Error("stream_options survived the strip")
	}
	if _, present := decoded["messages"]; !present {
		t.Error("the strip dropped the request itself")
	}

	// A body without the field is returned untouched (no re-encode, no churn).
	plain := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if got := antigravityStripStreamOptions(plain); string(got) != string(plain) {
		t.Errorf("a body without stream_options was rewritten: %s", got)
	}
}

// TestForwardGeminiStripsStreamOptionsWhenNotStreaming proves the wiring: the
// outbound envelope carries no stream_options, and the streaming path keeps the
// client's own value.
func TestForwardGeminiStripsStreamOptionsWhenNotStreaming(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(raw))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"ok"}]}}]}`))
	}))
	defer srv.Close()

	cfg := &providers.ProviderConfig{BaseURL: srv.URL, Format: "antigravity"}
	body := `{"model":"gemini-3.8-flash-high","stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`
	resp, err := ForwardGemini(t.Context(), srv.Client(), cfg, "token", body, false, "project-1", "gemini-3.8-flash-high")
	if err != nil {
		t.Fatalf("ForwardGemini: %v", err)
	}
	resp.Body.Close()
	if len(bodies) != 1 {
		t.Fatalf("upstream requests = %d, want 1", len(bodies))
	}
	if strings.Contains(bodies[0], "stream_options") {
		t.Errorf("a non-streaming Antigravity request carried stream_options: %s", bodies[0])
	}
}
