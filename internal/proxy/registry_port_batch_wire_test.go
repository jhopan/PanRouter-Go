package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"panrouter/proxy/internal/providers"
)

// TestAgentRouterWireRequest pins what actually leaves the process for the
// AgentRouter entry ported from open-sse/providers/registry/agentrouter.js.
//
// The provider is behind a WAF that answers only requests looking like the
// official Claude Code client, so the registry's declared transport has to
// survive the shared forwarder intact: the Claude CLI User-Agent, the Stainless
// fingerprint block, the raw x-api-key auth pair, and the ?beta=true suffix
// upstream declares as transport.urlSuffix. A config-only assertion cannot see
// the last one, because the query lives in the URL the forwarder dials.
func TestAgentRouterWireRequest(t *testing.T) {
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(r.Context())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	cfg := providers.KnownProviders["agentrouter"]
	// The registry URL is what the suffix belongs to; only the host is swapped
	// for the test listener so the query is the thing under test.
	cfg.BaseURL = srv.URL + "/v1/messages?beta=true"

	if _, err := ForwardOpenAI(t.Context(), srv.Client(), &cfg, "sk-agentrouter", []byte(`{"model":"claude-opus-4-6"}`), false); err != nil {
		t.Fatalf("ForwardOpenAI: %v", err)
	}
	if got == nil {
		t.Fatal("the upstream saw no request")
	}
	if p := got.URL.Path; p != "/v1/messages" {
		t.Errorf("path = %q, want /v1/messages", p)
	}
	if beta := got.URL.Query().Get("beta"); beta != "true" {
		t.Errorf("beta query = %q, want true (transport.urlSuffix)", beta)
	}
	if ua := got.Header.Get("User-Agent"); ua != "claude-cli/"+providers.ClaudeCLIVersion+" (external, sdk-cli)" {
		t.Errorf("User-Agent = %q, want the Claude CLI fingerprint", ua)
	}
	for header, want := range map[string]string{
		"x-api-key":           "sk-agentrouter",
		"X-App":               "cli",
		"X-Stainless-Lang":    "js",
		"anthropic-version":   "2023-06-01",
		"X-Stainless-Timeout": "600",
	} {
		if v := got.Header.Get(header); v != want {
			t.Errorf("%s = %q, want %q", header, v, want)
		}
	}
	if got.Header.Get("X-Stainless-Os") == "" || got.Header.Get("X-Stainless-Arch") == "" {
		t.Error("the Stainless OS/arch pair is missing; the fingerprint is incomplete")
	}
}
