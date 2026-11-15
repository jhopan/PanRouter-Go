package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"panrouter/proxy/internal/providers"
)

// Upstream tries the Amazon surfaces before the Kiro IDE gateway
// (open-sse/executors/kiro.js getOrderedBaseUrls), regionalizing the AWS hosts.
func TestKiroEndpointsOrdering(t *testing.T) {
	t.Run("amazon surfaces first, IDE gateway last", func(t *testing.T) {
		got := kiroEndpoints(&providers.ProviderConfig{}, nil)
		if len(got) != 3 {
			t.Fatalf("expected 3 kiro endpoints, got %d (%v)", len(got), got)
		}
		if !strings.Contains(got[0], "://q.") {
			t.Errorf("expected the q surface first, got %q", got[0])
		}
		if !strings.Contains(got[1], "://codewhisperer.") {
			t.Errorf("expected the codewhisperer surface second, got %q", got[1])
		}
		if !strings.Contains(got[2], "kiro.dev") {
			t.Errorf("expected the Kiro IDE gateway last, got %q", got[2])
		}
	})

	t.Run("aws hosts follow the account region only when covered", func(t *testing.T) {
		got := kiroEndpoints(&providers.ProviderConfig{}, map[string]any{"region": "eu-central-1"})
		if !strings.Contains(got[0], "q.eu-central-1.amazonaws.com") {
			t.Errorf("expected the q host in eu-central-1, got %q", got[0])
		}
		if !strings.Contains(got[1], "codewhisperer.us-east-1.amazonaws.com") {
			t.Errorf("expected codewhisperer to stay us-east-1 (only region served), got %q", got[1])
		}
	})

	t.Run("never builds a host in a region that service does not serve", func(t *testing.T) {
		got := kiroEndpoints(&providers.ProviderConfig{}, map[string]any{"region": "ap-southeast-1"})
		if !strings.Contains(got[0], "q.us-east-1.amazonaws.com") {
			t.Errorf("expected q to stay us-east-1 for uncovered ap-southeast-1, got %q", got[0])
		}
		if !strings.Contains(got[1], "codewhisperer.us-east-1.amazonaws.com") {
			t.Errorf("expected codewhisperer to stay us-east-1, got %q", got[1])
		}
	})

	t.Run("refuses to splice an attacker-controlled region into hostname", func(t *testing.T) {
		got := kiroEndpoints(&providers.ProviderConfig{}, map[string]any{"region": "x.attacker.tld#"})
		for _, u := range got {
			if strings.Contains(u, "attacker") {
				t.Errorf("hostile region leaked into endpoint %q", u)
			}
		}
	})

	t.Run("a configured non-amazon mirror is the only endpoint", func(t *testing.T) {
		cfg := &providers.ProviderConfig{BaseURL: "https://mirror.internal/kiro"}
		got := kiroEndpoints(cfg, nil)
		if len(got) != 1 || got[0] != "https://mirror.internal/kiro" {
			t.Errorf("expected only the configured mirror, got %v", got)
		}
	})
}

func TestKiroTokenType(t *testing.T) {
	tests := []struct {
		authMethod string
		want       string
	}{
		{authMethod: "api_key", want: "API_KEY"},
		{authMethod: "external_idp", want: "EXTERNAL_IDP"},
		{authMethod: "imported", want: ""},
		{authMethod: "builder-id", want: ""},
		{authMethod: "", want: ""},
	}
	for _, tt := range tests {
		psd := map[string]any{}
		if tt.authMethod != "" {
			psd["authMethod"] = tt.authMethod
		}
		if got := kiroTokenType(psd); got != tt.want {
			t.Errorf("kiroTokenType(%q) = %q, want %q", tt.authMethod, got, tt.want)
		}
	}
}

// Upstream KIRO_ENDPOINT_FALLBACK_STATUSES: 400 is a payload problem and must
// never be retried against another surface.
func TestKiroEndpointFallbackStatus(t *testing.T) {
	fallback := []int{401, 403, 404}
	for _, status := range fallback {
		if !kiroEndpointFallbackStatus(status) {
			t.Errorf("expected %d to rotate to the next kiro surface", status)
		}
	}
	for _, status := range []int{200, 400, 429, 500, 503} {
		if kiroEndpointFallbackStatus(status) {
			t.Errorf("expected %d to be terminal for kiro", status)
		}
	}
}
func TestKiroIdentity_FingerprintAndMachineID(t *testing.T) {
	t.Run("stable machine id per clientId", func(t *testing.T) {
		psd := map[string]any{"clientId": "client-12345"}
		id1 := providers.KiroMachineID(psd, "")
		id2 := providers.KiroMachineID(psd, "different-token")
		if id1 != id2 {
			t.Errorf("machine ID must be stable across token refreshes: %q != %q", id1, id2)
		}
		if len(id1) != 64 {
			t.Errorf("expected 64-hex sha256 machine ID, got %d chars: %q", len(id1), id1)
		}
	})

	t.Run("user agent carries the captured 1.4.178 identity", func(t *testing.T) {
		psd := map[string]any{"clientId": "client-test"}
		ua, amzUA := providers.KiroUserAgents(psd, "token", "codewhispererruntime")
		machineID := providers.KiroMachineID(psd, "token")

		// The captured client ends every UA with
		//   KiroIDE-1.4.178-<machineId>-KAS/0.66.102-hotfix.1 acp-client/kiro-ide
		// and the version gate reads exactly the KiroIDE-<version>-<machineId> part.
		tail := "KiroIDE-" + providers.KiroIDEVersion + "-" + machineID +
			"-KAS/" + providers.KiroAgentServerVersion + " " + providers.KiroClientAttribution
		if !strings.HasSuffix(ua, " "+tail) {
			t.Errorf("User-Agent must end with %q, got %q", tail, ua)
		}
		if !strings.Contains(ua, "api/codewhispererruntime#"+providers.KiroRuntimeSDKVersion) {
			t.Errorf("User-Agent missing the codewhispererruntime surface token, got %q", ua)
		}
		if !strings.Contains(ua, " m/N,E ") {
			t.Errorf("User-Agent missing the m/N,E capability pair, got %q", ua)
		}
		if want := "aws-sdk-js/" + providers.KiroRuntimeSDKVersion + " " + tail; amzUA != want {
			t.Errorf("X-Amz-User-Agent = %q, want %q", amzUA, want)
		}
		// The host token must describe this host: the constants this replaces
		// claimed os/windows#10.0.26200 from every platform.
		if !strings.Contains(ua, "os/"+providers.KiroOSPlatform()+"#") {
			t.Errorf("User-Agent os token does not name this host's platform, got %q", ua)
		}
		if strings.Contains(ua, "os/windows#") {
			t.Errorf("User-Agent must not claim the Windows platform token, got %q", ua)
		}
	})

	t.Run("service id follows the surface", func(t *testing.T) {
		tests := map[string]string{
			"https://runtime.us-east-1.kiro.dev/generateAssistantResponse":            "kiroruntime",
			"https://codewhisperer.us-east-1.amazonaws.com/generateAssistantResponse": "codewhispererstreaming",
			"https://q.us-east-1.amazonaws.com/generateAssistantResponse":             "codewhispererruntime",
		}
		for url, want := range tests {
			if got := providers.KiroServiceIDForURL(url); got != want {
				t.Errorf("KiroServiceIDForURL(%q) = %q, want %q", url, got, want)
			}
		}
	})
}

func TestForwardKiro_TargetAndProfileArn(t *testing.T) {
	var receivedHeaders http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := &providers.ProviderConfig{BaseURL: srv.URL}
	psd := map[string]any{
		"authMethod": "builder-id",
		"clientId":   "test-client-id",
	}

	resp, err := ForwardKiro(context.Background(), srv.Client(), cfg, "sso-token", []byte(`{}`), false, psd)
	if err != nil {
		t.Fatalf("ForwardKiro failed: %v", err)
	}
	defer resp.Body.Close()

	// BaseURL does not contain codewhisperer -> X-Amz-Target must NOT be sent
	if target := receivedHeaders.Get("X-Amz-Target"); target != "" {
		t.Errorf("X-Amz-Target must not be sent for non-codewhisperer endpoints, got %q", target)
	}

	// Effective profileArn for builder-id must fall back to the default builder ID ARN
	expectedArn := providers.KiroDefaultProfileARNBuilderID
	if arn := receivedHeaders.Get("x-amzn-codewhisperer-profile-arn"); arn != expectedArn {
		t.Errorf("x-amzn-codewhisperer-profile-arn = %q, want %q", arn, expectedArn)
	}

	// User-Agent must carry KiroIDE version
	ua := receivedHeaders.Get("User-Agent")
	if !strings.Contains(ua, "KiroIDE-"+providers.KiroIDEVersion) {
		t.Errorf("User-Agent missing KiroIDE version, got %q", ua)
	}
}
