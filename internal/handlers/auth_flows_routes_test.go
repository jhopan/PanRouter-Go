package handlers

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"panrouter/proxy/internal/auth"
	"panrouter/proxy/internal/db"
)

// TestSetupServerRouter_AuthFlowsAreLive pins the SSO browser flows and the
// password-reset route to the router the binary builds.
//
// Upstream lists /api/auth/oidc and /api/auth/saml in dashboardGuard
// PUBLIC_API_PATHS (the login page links there with no session yet), so the
// four flow routes must answer anonymously — with a redirect to /login carrying
// the reason, not a 404 and not the dashboard gate's 401.
// /api/auth/reset-password is the opposite: it is in LOCAL_ONLY_PATHS and absent
// from PUBLIC_API_PATHS, so an anonymous caller meets the dashboard gate.
func TestSetupServerRouter_AuthFlowsAreLive(t *testing.T) {
	t.Setenv("JWT_SECRET", "auth-flows-router-secret")
	t.Setenv("BASE_URL", "")
	t.Setenv("NEXT_PUBLIC_BASE_URL", "")
	database, cleanup := setupTestDB(t)
	defer cleanup()

	repo := db.NewRepo(database)
	r := chi.NewRouter()
	SetupServerRouter(r, repo, nil)

	public := []struct {
		name    string
		method  string
		path    string
		body    string
		form    url.Values
		wantErr string
	}{
		{
			name:    "oidc start unconfigured",
			method:  http.MethodGet,
			path:    "/api/auth/oidc/start",
			wantErr: "oidc_not_configured",
		},
		{
			name:    "oidc callback without cookies",
			method:  http.MethodGet,
			path:    "/api/auth/oidc/callback?code=abc&state=xyz",
			wantErr: "oidc_invalid_state",
		},
		{
			name:    "saml start unconfigured",
			method:  http.MethodGet,
			path:    "/api/auth/saml/start",
			wantErr: "saml_not_configured",
		},
		{
			name:    "saml acs without a response",
			method:  http.MethodPost,
			path:    "/api/auth/saml/acs",
			form:    url.Values{},
			wantErr: "saml_missing_response",
		},
	}
	for _, tt := range public {
		t.Run(tt.name, func(t *testing.T) {
			var body *strings.Reader
			if tt.form != nil {
				body = strings.NewReader(tt.form.Encode())
			} else {
				body = strings.NewReader("")
			}
			req := httptest.NewRequest(tt.method, tt.path, body)
			if tt.form != nil {
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			}
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			if rec.Code == http.StatusNotFound {
				t.Fatalf("%s %s is not registered (404)", tt.method, tt.path)
			}
			if rec.Code != http.StatusFound {
				t.Fatalf("%s %s status = %d, want 302", tt.method, tt.path, rec.Code)
			}
			loc, err := url.Parse(rec.Header().Get("Location"))
			if err != nil {
				t.Fatalf("parse Location: %v", err)
			}
			if loc.Path != "/login" {
				t.Errorf("Location path = %q, want /login", loc.Path)
			}
			if got := loc.Query().Get("error"); got != tt.wantErr {
				t.Errorf("error = %q, want %q", got, tt.wantErr)
			}
		})
	}

	// The reset route is dashboard-gated: anonymous is 401 from the gate, which
	// also proves the path matched instead of falling through to 404.
	anonRec := httptest.NewRecorder()
	r.ServeHTTP(anonRec, httptest.NewRequest(http.MethodPost, "/api/auth/reset-password", nil))
	if anonRec.Code == http.StatusNotFound {
		t.Fatal("POST /api/auth/reset-password is not registered (404)")
	}
	if anonRec.Code != http.StatusUnauthorized {
		t.Fatalf("POST /api/auth/reset-password anonymously = %d, want the dashboard gate's 401", anonRec.Code)
	}

	// A session plus a loopback peer reaches the handler and clears the stored
	// verifier, which is upstream's bare POST().
	token, err := auth.Sign("auth-flows-router-secret", time.Now())
	if err != nil {
		t.Fatalf("sign session token: %v", err)
	}
	if err := repo.UpdateSettingsRaw(map[string]any{"password": "$2a$10$notarealhash"}); err != nil {
		t.Fatalf("seed password: %v", err)
	}

	localReq := httptest.NewRequest(http.MethodPost, "/api/auth/reset-password", nil)
	localReq.RemoteAddr = "127.0.0.1:51234"
	localReq.Host = "localhost:20128"
	localReq.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
	localRec := httptest.NewRecorder()
	r.ServeHTTP(localRec, localReq)
	if localRec.Code != http.StatusOK {
		t.Fatalf("POST /api/auth/reset-password locally = %d, want 200: %s", localRec.Code, localRec.Body.String())
	}
	if got := localRec.Body.String(); !strings.Contains(got, `"success":true`) {
		t.Errorf("body = %s, want {success:true}", got)
	}
	raw, err := repo.GetSettingsRaw()
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if got, ok := raw["password"].(string); ok && got != "" {
		t.Error("the stored password verifier survived the reset")
	}

	// The same session from another host is refused by the local-only gate.
	remoteReq := httptest.NewRequest(http.MethodPost, "/api/auth/reset-password", nil)
	remoteReq.RemoteAddr = "203.0.113.7:51234"
	remoteReq.Host = "gateway.example.com"
	remoteReq.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
	remoteRec := httptest.NewRecorder()
	r.ServeHTTP(remoteRec, remoteReq)
	if remoteRec.Code != http.StatusForbidden {
		t.Fatalf("POST /api/auth/reset-password remotely = %d, want 403", remoteRec.Code)
	}
}
