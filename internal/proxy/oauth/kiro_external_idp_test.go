// Kiro enterprise (external IdP) auth — parity with
// src/lib/oauth/kiroExternalIdp.js and tests/unit/kiro-external-idp.test.js.
//
// Two rules matter more than the exchange itself:
//   - the token endpoint may only be a Microsoft login host, because that is the
//     URL the refresh token is sent to, and it is whatever the import stored;
//   - the refresh is a form-encoded OAuth refresh_token grant against that
//     endpoint, not Kiro's own token URL. Sending an Entra refresh token to the
//     Kiro endpoint returns an opaque 401 and the account never rotates.
package oauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestValidateMicrosoftTokenEndpoint(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{name: "microsoftonline", raw: "https://login.microsoftonline.com/tenant/oauth2/v2.0/token"},
		{name: "login.microsoft.com", raw: "https://login.microsoft.com/common/oauth2/token"},
		{name: "windows.net", raw: "https://login.windows.net/common/oauth2/token"},
		{name: "empty", raw: "   ", wantErr: "token_endpoint is required"},
		{name: "plain http", raw: "http://login.microsoftonline.com/token", wantErr: "must use https"},
		{name: "foreign host", raw: "https://evil.example.com/token", wantErr: "must be a Microsoft login endpoint"},
		{name: "not a url", raw: "://nope", wantErr: "must be a valid URL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ValidateMicrosoftTokenEndpoint(tt.raw)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !strings.HasPrefix(got, "https://") {
				t.Errorf("endpoint = %q, want an https URL", got)
			}
		})
	}
}

func TestNormalizeKiroIDPScope(t *testing.T) {
	if got := NormalizeKiroIDPScope([]any{"openid", " profile ", ""}); got != "openid profile" {
		t.Errorf("scope list = %q", got)
	}
	if got := NormalizeKiroIDPScope("  openid   profile "); got != "openid profile" {
		t.Errorf("scope string = %q", got)
	}
	if got := NormalizeKiroIDPScope(nil); got != "" {
		t.Errorf("nil scope = %q, want empty", got)
	}
}

func TestRefreshKiroExternalIDPSendsTheRefreshGrant(t *testing.T) {
	var gotForm url.Values
	var gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		gotForm = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600}`))
	}))
	defer srv.Close()

	// The handler validates the host, so the fake cannot be a Microsoft host: the
	// validation is exercised above, and the exchange below runs through the
	// refresh function with the endpoint already resolved.
	psd := map[string]string{
		"authMethod":    "external_idp",
		"clientId":      "client-123",
		"scope":         "openid profile",
		"tokenEndpoint": srv.URL,
	}
	// Call the grant itself (validation of the host is a separate contract).
	result, err := refreshKiroExternalIDPGrant(context.Background(), srv.Client(), "refresh-token", psd["clientId"], psd["scope"], psd["tokenEndpoint"])
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if result.AccessToken != "new-access" {
		t.Errorf("access token = %q", result.AccessToken)
	}
	if result.RefreshToken != "new-refresh" {
		t.Errorf("refresh token = %q, want the rotated one", result.RefreshToken)
	}
	if result.ExpiresIn != 3600 {
		t.Errorf("expiresIn = %d", result.ExpiresIn)
	}
	if !strings.HasPrefix(gotContentType, "application/x-www-form-urlencoded") {
		t.Errorf("Content-Type = %q, want a form post", gotContentType)
	}
	if gotForm.Get("grant_type") != "refresh_token" || gotForm.Get("client_id") != "client-123" ||
		gotForm.Get("refresh_token") != "refresh-token" || gotForm.Get("scope") != "openid profile" {
		t.Errorf("refresh grant form = %v", gotForm)
	}
}

func TestRefreshKiroExternalIDPRejectsAForeignEndpoint(t *testing.T) {
	// The stored endpoint receives the refresh token: a non-Microsoft host must be
	// refused before any request is made.
	_, err := refreshKiroExternalIDP(context.Background(), &http.Client{Timeout: time.Second}, "rt", map[string]string{
		"authMethod":    "external_idp",
		"clientId":      "cid",
		"scope":         "openid",
		"tokenEndpoint": "https://evil.example.com/token",
	})
	if err == nil || !strings.Contains(err.Error(), "Microsoft login endpoint") {
		t.Fatalf("err = %v, want the endpoint refusal", err)
	}
}

func TestRefreshKiroDispatchesExternalIDPFirst(t *testing.T) {
	// An external_idp connection carries no clientSecret, but it must still never
	// reach the social endpoint: the dispatch has to look at authMethod first.
	_, err := refreshKiro(context.Background(), &Params{
		RefreshToken:         "rt",
		ProviderSpecificData: map[string]string{"authMethod": "external_idp"},
	})
	if err == nil || !strings.Contains(err.Error(), "token_endpoint is required") {
		t.Fatalf("err = %v, want the external-IdP endpoint requirement", err)
	}
}