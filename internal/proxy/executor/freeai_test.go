package executor

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"panrouter/proxy/internal/providers"
)

// freeAIRecordingTransport captures the URL the forwarder dialled and answers
// with a canned body, so the endpoint choice can be asserted without a network.
type freeAIRecordingTransport struct {
	url string
}

func (rt *freeAIRecordingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rt.url = r.URL.String()
	body := `{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"ok"}}]}`
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    r,
	}, nil
}

type freeAIFailingTransport struct{}

// RoundTrip stands in for "no network in a unit test": the dial is refused
// before anything leaves the process.
func (freeAIFailingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("dial refused by the test transport")
}

type freeAINoopWriter struct{}

func (freeAINoopWriter) Header() http.Header         { return http.Header{} }
func (freeAINoopWriter) Write(b []byte) (int, error) { return len(b), nil }
func (freeAINoopWriter) WriteHeader(int)             {}

// TestForwardFreeAIPicksTheEndpointFamilyPerModel pins the port of
// open-sse/executors/freeai.js: chat/code ids dial the native /v1/chat/ path,
// the three vision ids dial the OpenAI-compatible /v1/chat/completions path,
// and both do it off the one provider entry and one key.
func TestForwardFreeAIPicksTheEndpointFamilyPerModel(t *testing.T) {
	tests := []struct {
		name    string
		model   string
		wantURL string
	}{
		{name: "chat model keeps the registry base", model: "deepseek-r1", wantURL: freeAIChatBase},
		{name: "code model keeps the registry base", model: "qwen3-coder", wantURL: freeAIChatBase},
		{name: "vision model moves to the OpenAI base", model: "qwen-vl", wantURL: freeAIVisionBase},
		{name: "second vision id", model: "qwen25-vl", wantURL: freeAIVisionBase},
		{name: "third vision id", model: "moondream2", wantURL: freeAIVisionBase},
		{name: "provider prefix is stripped before the lookup", model: "fai/qwen-vl", wantURL: freeAIVisionBase},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := &freeAIRecordingTransport{}
			cfg := providers.KnownProviders["freeai"]
			req := &Request{
				Ctx:    t.Context(),
				Client: &http.Client{Transport: rt},
				Config: &cfg,
				APIKey: "sk-freeai",
				Body:   []byte(`{"model":"` + tt.model + `"}`),
			}
			if err := ForwardFreeAI(freeAINoopWriter{}, req); err != nil {
				t.Fatalf("ForwardFreeAI: %v", err)
			}
			if rt.url != tt.wantURL {
				t.Errorf("dialled %q, want %q", rt.url, tt.wantURL)
			}
		})
	}
}

// TestForwardFreeAIHonoursAConnectionBaseURL pins the override upstream gives
// priority to: when the connection carries its own base URL
// (providerSpecificData.baseUrl), the per-model split must stand aside rather
// than overwrite the operator's host for every model.
func TestForwardFreeAIHonoursAConnectionBaseURL(t *testing.T) {
	const override = "https://self-hosted.example/v1/chat/completions"
	cfg := providers.KnownProviders["freeai"]
	cfg.BaseURL = override
	req := &Request{
		Ctx:    t.Context(),
		Client: &http.Client{Transport: freeAIFailingTransport{}},
		Config: &cfg,
		APIKey: "sk-freeai",
		Body:   []byte(`{"model":"qwen-vl"}`),
	}
	// The dial is refused on purpose; what matters is that the config still
	// points at the operator's host after the call.
	_ = ForwardFreeAI(freeAINoopWriter{}, req)
	if cfg.BaseURL != override {
		t.Errorf("BaseURL = %q, want the connection override untouched", cfg.BaseURL)
	}
}
