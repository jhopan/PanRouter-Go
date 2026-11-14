package oauth

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"panrouter/proxy/internal/providers"
)

// The intl refresh must look like the CLI's refresh on the wire: the same CLI
// user agent, the account id from the token's sub claim, a per-request id, and
// the still-valid access token as a bearer. PanRouter used to advertise
// "IDE/2.160.0" here while the chat path said "CLI/2.178.0" — one account, two
// identities — so the two lanes are pinned to the same values.
// (upstream tests/unit/codebuddy-intl-cli-identity.test.js, "OAuth identity
// matches the CLI")

const codebuddyTestSub = "85482e42-2454-468d-a2b0-f1f6311f0f81"

func codebuddyJWT(sub string) string {
	payload := []byte(`{"sub":"` + sub + `"}`)
	return "eyJhbGciOiJIUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

// withCodebuddyRefreshServer points the refresher at a local server and restores
// the production URL afterwards.
func withCodebuddyRefreshServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	previous := codebuddyIntlRefreshURL
	codebuddyIntlRefreshURL = server.URL
	t.Cleanup(func() {
		codebuddyIntlRefreshURL = previous
		server.Close()
	})
	return server
}

func TestRefreshCodeBuddyIntl_CLIIdentity(t *testing.T) {
	var gotHeaders http.Header
	var gotMethod, gotBody string
	server := withCodebuddyRefreshServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		gotMethod = r.Method
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		_, _ = w.Write([]byte(`{"code":0,"data":{"accessToken":"new-access","refreshToken":"new-refresh","expiresIn":86400}}`))
	})

	accessToken := codebuddyJWT(codebuddyTestSub)
	result, err := refreshCodeBuddyIntl(context.Background(), &Params{
		Client:       server.Client(),
		Provider:     "codebuddy-intl",
		RefreshToken: "old-refresh",
		AccessToken:  accessToken,
	})
	if err != nil {
		t.Fatalf("refreshCodeBuddyIntl: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotBody != "{}" {
		t.Errorf("body = %q, want {}", gotBody)
	}
	for header, want := range map[string]string{
		"User-Agent":            "CLI/2.178.0 CodeBuddy/2.178.0",
		"Accept":                "application/json, text/plain, */*",
		"Content-Type":          "application/json",
		"X-Requested-With":      "XMLHttpRequest",
		"X-Domain":              "www.codebuddy.ai",
		"X-Refresh-Token":       "old-refresh",
		"X-Auth-Refresh-Source": "plugin",
		"X-Product":             "SaaS",
		"X-User-Id":             codebuddyTestSub,
		"Authorization":         "Bearer " + accessToken,
	} {
		if got := gotHeaders.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if len(gotHeaders.Get("X-Request-ID")) != 32 {
		t.Errorf("X-Request-ID = %q, want 32 hex chars", gotHeaders.Get("X-Request-ID"))
	}

	if result.AccessToken != "new-access" {
		t.Errorf("AccessToken = %q", result.AccessToken)
	}
	if result.RefreshToken != "new-refresh" {
		t.Errorf("RefreshToken = %q", result.RefreshToken)
	}
	if result.ExpiresIn != 86400 {
		t.Errorf("ExpiresIn = %d", result.ExpiresIn)
	}
}

// The refresh token rides in the X-Refresh-Token header, never in a form body:
// the official CLI never posts a grant_type to this endpoint.
func TestRefreshCodeBuddyIntl_KeepsTheOldRefreshTokenWhenNoneIsReturned(t *testing.T) {
	server := withCodebuddyRefreshServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"accessToken":"new-access","expiresIn":3600}}`))
	})

	result, err := refreshCodeBuddyIntl(context.Background(), &Params{
		Client:       server.Client(),
		Provider:     "codebuddy-intl",
		RefreshToken: "old-refresh",
	})
	if err != nil {
		t.Fatalf("refreshCodeBuddyIntl: %v", err)
	}
	if result.RefreshToken != "old-refresh" {
		t.Errorf("RefreshToken = %q, want the spent token kept", result.RefreshToken)
	}
}

func TestRefreshCodeBuddyIntl_OmitsUserIDWithoutASubClaim(t *testing.T) {
	var gotHeaders http.Header
	server := withCodebuddyRefreshServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		_, _ = w.Write([]byte(`{"code":0,"data":{"accessToken":"new-access"}}`))
	})

	if _, err := refreshCodeBuddyIntl(context.Background(), &Params{
		Client:       server.Client(),
		Provider:     "codebuddy-intl",
		RefreshToken: "old-refresh",
		AccessToken:  "opaque-token",
	}); err != nil {
		t.Fatalf("refreshCodeBuddyIntl: %v", err)
	}
	if _, ok := gotHeaders["X-User-Id"]; ok {
		t.Errorf("X-User-Id = %q, want it omitted", gotHeaders.Get("X-User-Id"))
	}
	// An opaque token is still the caller's bearer: the CLI sends whatever
	// access token it holds.
	if got := gotHeaders.Get("Authorization"); got != "Bearer opaque-token" {
		t.Errorf("Authorization = %q", got)
	}
}

func TestRefreshCodeBuddyIntl_Errors(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		refresh    string
		wantStatus int
	}{
		{name: "rejected", status: http.StatusForbidden, body: `{"code":11140,"msg":"request illegal"}`, refresh: "rt", wantStatus: http.StatusForbidden},
		{name: "unauthorized", status: http.StatusUnauthorized, body: `{"code":1,"msg":"bad token"}`, refresh: "rt", wantStatus: http.StatusUnauthorized},
		{name: "provider error code", status: http.StatusOK, body: `{"code":11217,"msg":"authorization_pending"}`, refresh: "rt"},
		{name: "empty access token", status: http.StatusOK, body: `{"code":0,"data":{}}`, refresh: "rt"},
		{name: "no refresh token", status: http.StatusOK, body: `{"code":0,"data":{"accessToken":"x"}}`, refresh: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := withCodebuddyRefreshServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			})

			_, err := refreshCodeBuddyIntl(context.Background(), &Params{
				Client:       server.Client(),
				Provider:     "codebuddy-intl",
				RefreshToken: tt.refresh,
			})
			if err == nil {
				t.Fatal("expected an error")
			}
			if tt.wantStatus != 0 {
				var refreshErr *providers.OAuthRefreshError
				if !errors.As(err, &refreshErr) {
					t.Fatalf("error %v, want *providers.OAuthRefreshError", err)
				}
				if refreshErr.Status != tt.wantStatus {
					t.Errorf("status = %d, want %d", refreshErr.Status, tt.wantStatus)
				}
			}
		})
	}
}

// The registry entry the refresher reads at startup must point at the intl
// host: copilot.tencent.com answers CN accounts only.
func TestCodebuddyIntlRefreshURL_PointsAtTheIntlHost(t *testing.T) {
	if got := providers.KnownOAuthConfigs["codebuddy-intl"].TokenURL; got != "https://www.codebuddy.ai/v2/plugin/auth/token/refresh" {
		t.Errorf("codebuddy-intl TokenURL = %q, want the codebuddy.ai refresh endpoint", got)
	}
}
