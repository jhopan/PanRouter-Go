package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The SSO flows put identity claims in the session cookie, and /api/auth/status
// reads them back through SessionClaimSet. A claim that SignWith drops would
// leave the dashboard showing every SSO user as "Password user".
func TestSignWithCarriesSSOClaims(t *testing.T) {
	t.Setenv("JWT_SECRET", "sso-claims-secret")
	t.Setenv("DATA_DIR", t.TempDir())

	now := time.Now()
	cases := []struct {
		name   string
		claims SessionClaims
		check  func(*testing.T, *SessionClaims)
	}{
		{
			name: "oidc",
			claims: SessionClaims{
				Authenticated: true,
				Oidc:          true,
				OidcSub:       "sub-1",
				OidcName:      "Ada",
				OidcEmail:     "ada@example.com",
			},
			check: func(t *testing.T, got *SessionClaims) {
				t.Helper()
				if !got.Oidc || got.OidcSub != "sub-1" || got.OidcName != "Ada" || got.OidcEmail != "ada@example.com" {
					t.Errorf("oidc claims = %+v", got)
				}
				if got.Saml {
					t.Error("an OIDC session reports itself as SAML")
				}
			},
		},
		{
			name: "saml",
			claims: SessionClaims{
				Authenticated: true,
				Saml:          true,
				SamlName:      "Test User",
				SamlEmail:     "user@example.com",
			},
			check: func(t *testing.T, got *SessionClaims) {
				t.Helper()
				if !got.Saml || got.SamlName != "Test User" || got.SamlEmail != "user@example.com" {
					t.Errorf("saml claims = %+v", got)
				}
				if got.Oidc {
					t.Error("a SAML session reports itself as OIDC")
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			token, err := SignWith(Secret(), now, tc.claims)
			if err != nil {
				t.Fatalf("SignWith: %v", err)
			}
			if !Verify(token, Secret()) {
				t.Fatal("the SSO session token does not verify")
			}
			req := httptest.NewRequest(http.MethodGet, "/api/auth/status", nil)
			req.AddCookie(&http.Cookie{Name: CookieName, Value: token})
			got := SessionClaimSet(req)
			if got == nil {
				t.Fatal("SessionClaimSet returned nil for a valid cookie")
			}
			tc.check(t, got)
		})
	}
}
