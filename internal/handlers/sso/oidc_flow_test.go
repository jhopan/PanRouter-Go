package sso

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestHandleOIDCStart_Flow(t *testing.T) {
	repo, cleanup := newTestRepo(t)
	defer cleanup()

	h := NewHandler(repo)

	t.Run("unconfigured redirects to login", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/auth/oidc/start", nil)
		rec := httptest.NewRecorder()
		h.HandleOIDCStart(rec, req)

		if rec.Code != http.StatusFound {
			t.Fatalf("status = %d, want 302", rec.Code)
		}
		if loc := rec.Header().Get("Location"); !strings.Contains(loc, "error=oidc_not_configured") {
			t.Fatalf("location = %q, want oidc_not_configured", loc)
		}
	})

	t.Run("configured without BASE_URL or TLS refuses to start", func(t *testing.T) {
		t.Setenv("BASE_URL", "")
		t.Setenv("NEXT_PUBLIC_BASE_URL", "")
		_ = repo.UpdateSettingsRaw(map[string]any{
			"authMode":         "oidc",
			"oidcIssuerUrl":    "https://idp.example.com",
			"oidcClientId":     "client-123",
			"oidcClientSecret": "shh",
		})
		req := httptest.NewRequest(http.MethodGet, "http://gateway.internal/api/auth/oidc/start", nil)
		req.Host = "gateway.internal"
		rec := httptest.NewRecorder()
		h.HandleOIDCStart(rec, req)

		if rec.Code != http.StatusFound {
			t.Fatalf("status = %d, want 302", rec.Code)
		}
		if loc := rec.Header().Get("Location"); !strings.Contains(loc, "error=oidc_base_url_required") {
			t.Fatalf("location = %q, want oidc_base_url_required", loc)
		}
	})

	t.Run("configured with mock IdP sets cookies and redirects with PKCE", func(t *testing.T) {
		idpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/.well-known/openid-configuration" {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"issuer":                 "http://" + r.Host,
					"authorization_endpoint": "http://" + r.Host + "/auth",
					"token_endpoint":         "http://" + r.Host + "/token",
					"jwks_uri":               "http://" + r.Host + "/jwks.json",
				})
			}
		}))
		defer idpServer.Close()

		t.Setenv("BASE_URL", "https://gateway.example.com")
		_ = repo.UpdateSettingsRaw(map[string]any{
			"authMode":         "oidc",
			"oidcIssuerUrl":    idpServer.URL,
			"oidcClientId":     "client-123",
			"oidcClientSecret": "shh",
		})

		req := httptest.NewRequest(http.MethodGet, "https://gateway.example.com/api/auth/oidc/start", nil)
		rec := httptest.NewRecorder()
		h.HandleOIDCStart(rec, req)

		if rec.Code != http.StatusFound {
			t.Fatalf("status = %d, want 302", rec.Code)
		}

		loc := rec.Header().Get("Location")
		parsed, err := url.Parse(loc)
		if err != nil {
			t.Fatalf("parse location %q: %v", loc, err)
		}
		if parsed.Scheme+"://"+parsed.Host+parsed.Path != idpServer.URL+"/auth" {
			t.Fatalf("unexpected authorization endpoint: %s", loc)
		}
		q := parsed.Query()
		if got := q.Get("client_id"); got != "client-123" {
			t.Errorf("client_id = %q, want the stored id", got)
		}
		if got := q.Get("redirect_uri"); got != "https://gateway.example.com/api/auth/oidc/callback" {
			t.Errorf("redirect_uri = %q", got)
		}
		if got := q.Get("response_type"); got != "code" {
			t.Errorf("response_type = %q, want code", got)
		}
		if got := q.Get("scope"); got != defaultScopes {
			t.Errorf("scope = %q, want %q", got, defaultScopes)
		}
		if got := q.Get("code_challenge_method"); got != "S256" {
			t.Errorf("code_challenge_method = %q, want S256", got)
		}
		if q.Get("state") == "" {
			t.Error("authorization URL carries no state")
		}

		cookies := rec.Result().Cookies()
		cookieMap := map[string]*http.Cookie{}
		for _, c := range cookies {
			cookieMap[c.Name] = c
		}

		for _, required := range []string{oidcStateCookie, oidcNonceCookie, oidcVerifierCookie} {
			c, ok := cookieMap[required]
			if !ok || c.Value == "" {
				t.Errorf("missing or empty cookie: %s", required)
			}
		}
		// The flow cookies are what bind the callback to this request: the URL's
		// state must be the stored one, and the challenge must be S256 of the
		// stored verifier.
		if got := cookieMap[oidcStateCookie].Value; got != q.Get("state") {
			t.Errorf("state cookie = %q, authorization state = %q", got, q.Get("state"))
		}
		if got := cookieMap[oidcNonceCookie].Value; got != q.Get("nonce") {
			t.Errorf("nonce cookie = %q, authorization nonce = %q", got, q.Get("nonce"))
		}
		if got := cookieMap[oidcVerifierCookie].Value; pkceChallenge(got) != q.Get("code_challenge") {
			t.Errorf("code_challenge does not match S256(verifier cookie)")
		}
	})

	t.Run("SSO disabled or secret missing counts as unconfigured", func(t *testing.T) {
		t.Setenv("BASE_URL", "https://gateway.example.com")
		for _, tc := range []struct {
			name     string
			settings map[string]any
		}{
			{
				name: "authMode password",
				settings: map[string]any{
					"authMode":         "password",
					"oidcIssuerUrl":    "https://idp.example.com",
					"oidcClientId":     "client-123",
					"oidcClientSecret": "shh",
				},
			},
			{
				name: "no client secret",
				settings: map[string]any{
					"authMode":         "both",
					"oidcIssuerUrl":    "https://idp.example.com",
					"oidcClientId":     "client-123",
					"oidcClientSecret": "",
				},
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				_ = repo.UpdateSettingsRaw(tc.settings)
				req := httptest.NewRequest(http.MethodGet, "https://gateway.example.com/api/auth/oidc/start", nil)
				rec := httptest.NewRecorder()
				h.HandleOIDCStart(rec, req)

				if rec.Code != http.StatusFound {
					t.Fatalf("status = %d, want 302", rec.Code)
				}
				if loc := rec.Header().Get("Location"); !strings.Contains(loc, "error=oidc_not_configured") {
					t.Fatalf("location = %q, want oidc_not_configured", loc)
				}
			})
		}
	})
}

func TestHandleOIDCCallback_Flow(t *testing.T) {
	repo, cleanup := newTestRepo(t)
	defer cleanup()

	h := NewHandler(repo)

	t.Run("state mismatch or missing cookie redirects with error", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/auth/oidc/callback?state=xyz&code=123", nil)
		rec := httptest.NewRecorder()
		h.HandleOIDCCallback(rec, req)

		if rec.Code != http.StatusFound {
			t.Fatalf("status = %d, want 302", rec.Code)
		}
		if loc := rec.Header().Get("Location"); !strings.Contains(loc, "error=oidc_invalid_state") {
			t.Fatalf("location = %q, want oidc_invalid_state", loc)
		}
	})

	t.Run("IdP error param forwarded to login", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/auth/oidc/callback?error=access_denied", nil)
		rec := httptest.NewRecorder()
		h.HandleOIDCCallback(rec, req)

		if rec.Code != http.StatusFound {
			t.Fatalf("status = %d, want 302", rec.Code)
		}
		if loc := rec.Header().Get("Location"); !strings.Contains(loc, "error=access_denied") {
			t.Fatalf("location = %q, want access_denied", loc)
		}
	})

	t.Run("missing code redirects with oidc_missing_code", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/auth/oidc/callback?state=my-state", nil)
		req.AddCookie(&http.Cookie{Name: oidcStateCookie, Value: "my-state"})
		req.AddCookie(&http.Cookie{Name: oidcNonceCookie, Value: "my-nonce"})
		req.AddCookie(&http.Cookie{Name: oidcVerifierCookie, Value: "my-verifier"})
		rec := httptest.NewRecorder()
		h.HandleOIDCCallback(rec, req)

		if rec.Code != http.StatusFound {
			t.Fatalf("status = %d, want 302", rec.Code)
		}
		if loc := rec.Header().Get("Location"); !strings.Contains(loc, "error=oidc_missing_code") {
			t.Fatalf("location = %q, want oidc_missing_code", loc)
		}
	})
}

func TestHandleSAMLFlow_Handlers(t *testing.T) {
	repo, cleanup := newTestRepo(t)
	defer cleanup()

	h := NewHandler(repo)

	t.Run("saml start unconfigured redirects to login", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/auth/saml/start", nil)
		rec := httptest.NewRecorder()
		h.HandleSAMLStart(rec, req)

		if rec.Code != http.StatusFound {
			t.Fatalf("status = %d, want 302", rec.Code)
		}
		if loc := rec.Header().Get("Location"); !strings.Contains(loc, "error=saml_not_configured") {
			t.Fatalf("location = %q, want saml_not_configured", loc)
		}
	})

	t.Run("saml start configured without BASE_URL on http refuses", func(t *testing.T) {
		t.Setenv("BASE_URL", "")
		t.Setenv("NEXT_PUBLIC_BASE_URL", "")
		_ = repo.UpdateSettingsRaw(map[string]any{
			"samlEntryPoint": "https://idp.example.com/sso",
			"samlCert":       "cert-pem",
		})
		req := httptest.NewRequest(http.MethodGet, "http://gateway.internal/api/auth/saml/start", nil)
		req.Host = "gateway.internal"
		rec := httptest.NewRecorder()
		h.HandleSAMLStart(rec, req)

		if rec.Code != http.StatusFound {
			t.Fatalf("status = %d, want 302", rec.Code)
		}
		if loc := rec.Header().Get("Location"); !strings.Contains(loc, "error=saml_base_url_required") {
			t.Fatalf("location = %q, want saml_base_url_required", loc)
		}
	})

	t.Run("saml acs missing SAMLResponse form redirects with error", func(t *testing.T) {
		form := url.Values{}
		req := httptest.NewRequest(http.MethodPost, "/api/auth/saml/acs", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		h.HandleSAMLACS(rec, req)

		if rec.Code != http.StatusFound {
			t.Fatalf("status = %d, want 302", rec.Code)
		}
		if loc := rec.Header().Get("Location"); !strings.Contains(loc, "error=saml_missing_response") {
			t.Fatalf("location = %q, want saml_missing_response", loc)
		}
	})

	t.Run("saml acs invalid base64 redirects with error", func(t *testing.T) {
		form := url.Values{"SAMLResponse": {"not-valid-base64!!!"}}
		req := httptest.NewRequest(http.MethodPost, "/api/auth/saml/acs", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		h.HandleSAMLACS(rec, req)

		if rec.Code != http.StatusFound {
			t.Fatalf("status = %d, want 302", rec.Code)
		}
		if loc := rec.Header().Get("Location"); !strings.Contains(loc, "error=saml_bad_response") {
			t.Fatalf("location = %q, want saml_bad_response", loc)
		}
	})
}
