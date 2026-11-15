// Kiro surface headers — the captured 1.4.178 wire, ported from
// open-sse/executors/kiro.js buildHeaders and pinned by
// tests/unit/kiro-runtime-wire.test.js.
//
// Three surfaces, three shapes (captured 2026-11-15 with mitmproxy):
//
//	runtime.<region>.kiro.dev   x-amz-target: KiroRuntimeService.GenerateAssistantResponse
//	                            content-type: application/x-amz-json-1.0
//	                            x-amzn-kiro-client-attribution / -mode-identifier / -fallback / -attempt
//	codewhisperer.<region>...   x-amz-target: AmazonCodeWhispererStreamingService.GenerateAssistantResponse
//	q.<region>.amazonaws.com    no x-amz-target at all
//
// The runtime gateway refuses the legacy path-style operation with 400
// REQUEST_BODY_INVALID, and 400 is terminal, so getting this wrong takes the whole
// provider down rather than one request.
package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"panrouter/proxy/internal/providers"
)

func TestKiroRuntimeSurfaceHeaders(t *testing.T) {
	endpoint := "https://runtime.us-east-1.kiro.dev/generateAssistantResponse"
	headers := kiroSurfaceHeaders(endpoint, map[string]any{"authMethod": "builder-id", "clientId": "client-runtime"}, "sso-token", "inv-1")

	if got := headers["X-Amz-Target"]; got != "KiroRuntimeService.GenerateAssistantResponse" {
		t.Errorf("X-Amz-Target = %q, want the runtime operation", got)
	}
	if got := headers["Content-Type"]; got != "application/x-amz-json-1.0" {
		t.Errorf("Content-Type = %q, want the amazon json-1.0 wire", got)
	}
	for _, hint := range []string{"x-amzn-kiro-client-attribution", "x-amzn-kiro-mode-identifier", "x-amzn-kiro-fallback", "x-amzn-kiro-attempt"} {
		if headers[hint] == "" {
			t.Errorf("runtime surface is missing %s", hint)
		}
	}
	if got := headers["x-amzn-kiro-client-attribution"]; got != "kiro-ide" {
		t.Errorf("x-amzn-kiro-client-attribution = %q, want kiro-ide", got)
	}
	if got := headers["x-amzn-kiro-mode-identifier"]; got != "default-v2" {
		t.Errorf("x-amzn-kiro-mode-identifier = %q, want default-v2", got)
	}
	if got := headers["x-amzn-kiro-fallback"]; got != "0;max=0" {
		t.Errorf("x-amzn-kiro-fallback = %q, want 0;max=0", got)
	}
	// The 1.4.178 IDE sends none of the Amazon-only markers to this gateway.
	for _, absent := range []string{"x-amz-sso-bearer", "x-amzn-kiro-agent-mode", "x-amzn-codewhisperer-machine-id"} {
		if got := headers[absent]; got != "" {
			t.Errorf("%s must not be sent to the runtime gateway, got %q", absent, got)
		}
	}
	if got := headers["User-Agent"]; !strings.Contains(got, "api/kiroruntime#") {
		t.Errorf("User-Agent = %q, want the api/kiroruntime service token", got)
	}
}

func TestKiroCodeWhispererSurfaceHeaders(t *testing.T) {
	endpoint := "https://codewhisperer.us-east-1.amazonaws.com/generateAssistantResponse"
	headers := kiroSurfaceHeaders(endpoint, map[string]any{"authMethod": "builder-id", "clientId": "client-cw"}, "sso-token", "inv-2")

	if got := headers["X-Amz-Target"]; got != "AmazonCodeWhispererStreamingService.GenerateAssistantResponse" {
		t.Errorf("X-Amz-Target = %q, want the CodeWhisperer operation", got)
	}
	if got := headers["Content-Type"]; got != "" {
		t.Errorf("the CodeWhisperer surface takes the plain JSON body, got Content-Type %q", got)
	}
	if got := headers["x-amzn-kiro-client-attribution"]; got != "" {
		t.Errorf("runtime-only hint sent to codewhisperer: %q", got)
	}
	if got := headers["x-amz-sso-bearer"]; got != "sso-token" {
		t.Errorf("x-amz-sso-bearer = %q, want the account token", got)
	}
	if got := headers["x-amzn-kiro-agent-mode"]; got != "spec" {
		t.Errorf("x-amzn-kiro-agent-mode = %q, want spec", got)
	}
	if got := headers["User-Agent"]; !strings.Contains(got, "api/codewhispererstreaming#") {
		t.Errorf("User-Agent = %q, want the api/codewhispererstreaming service token", got)
	}
}

func TestKiroAmazonQSurfaceSendsNoTarget(t *testing.T) {
	endpoint := "https://q.us-east-1.amazonaws.com/generateAssistantResponse"
	headers := kiroSurfaceHeaders(endpoint, map[string]any{"authMethod": "api_key", "profileArn": "arn:aws:codewhisperer:us-east-1:1:profile/k"}, "api-key-value", "inv-3")

	if got := headers["X-Amz-Target"]; got != "" {
		t.Errorf("q.* must send no X-Amz-Target, got %q", got)
	}
	if got := headers["TokenType"]; got != "API_KEY" {
		t.Errorf("TokenType = %q, want API_KEY for an api_key connection", got)
	}
	if got := headers["x-amzn-codewhisperer-profile-arn"]; got != "arn:aws:codewhisperer:us-east-1:1:profile/k" {
		t.Errorf("profile arn = %q, want the connection's own", got)
	}
	if got := headers["User-Agent"]; !strings.Contains(got, "api/codewhispererruntime#") {
		t.Errorf("User-Agent = %q, want the api/codewhispererruntime service token", got)
	}
}

// TestForwardKiroSendsTheBuiltHeaders proves the request path uses the builder:
// a fake host stands in for a self-hosted mirror, which is the "other" surface.
func TestForwardKiroSendsTheBuiltHeaders(t *testing.T) {
	var received http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := &providers.ProviderConfig{BaseURL: srv.URL}
	psd := map[string]any{"authMethod": "builder-id", "clientId": "client-other"}
	resp, err := ForwardKiro(context.Background(), srv.Client(), cfg, "sso-token", []byte(`{}`), false, psd)
	if err != nil {
		t.Fatalf("ForwardKiro failed: %v", err)
	}
	resp.Body.Close()

	if target := received.Get("X-Amz-Target"); target != "" {
		t.Errorf("a non-Amazon surface must send no X-Amz-Target, got %q", target)
	}
	if got := received.Get("Amz-Sdk-Invocation-Id"); len(got) != 36 {
		t.Errorf("Amz-Sdk-Invocation-Id = %q, want a uuid", got)
	}
	if got := received.Get("x-amz-sso-bearer"); got != "sso-token" {
		t.Errorf("x-amz-sso-bearer = %q, want the account token", got)
	}
	if got := received.Get("User-Agent"); !strings.Contains(got, "api/codewhispererruntime#") {
		t.Errorf("User-Agent = %q, want the codewhispererruntime service token", got)
	}
}
