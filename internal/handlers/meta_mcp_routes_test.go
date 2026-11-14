package handlers

import (
	json "encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"panrouter/proxy/internal/auth"
	"panrouter/proxy/internal/db"
)

// The dashboard boot surface and the MCP bridge are mounted on the router the
// binary actually builds, not only on the tests-only dashboard table:
// /api/init and /api/locale are public upstream (dashboardGuard
// PUBLIC_API_PATHS), while /api/pricing, /api/tags and /api/mcp/* sit in
// PROTECTED_API_PATHS, i.e. the dashboard auth group.
func TestSetupServerRouter_MetaAndMCPRoutesAreLive(t *testing.T) {
	t.Setenv("JWT_SECRET", "meta-mcp-router-secret")
	database, cleanup := setupTestDB(t)
	defer cleanup()

	repo := db.NewRepo(database)
	r := chi.NewRouter()
	SetupServerRouter(r, repo, nil)

	// Public: no session, no API key.
	initRec := httptest.NewRecorder()
	r.ServeHTTP(initRec, httptest.NewRequest(http.MethodGet, "/api/init", nil))
	if initRec.Code != http.StatusOK {
		t.Fatalf("GET /api/init status = %d, want 200", initRec.Code)
	}
	if got := initRec.Body.String(); got != "Initialized" {
		t.Errorf("GET /api/init body = %q, want %q", got, "Initialized")
	}

	localeRec := httptest.NewRecorder()
	r.ServeHTTP(localeRec, httptest.NewRequest(http.MethodPost, "/api/locale",
		strings.NewReader(`{"locale":"vi"}`)))
	if localeRec.Code != http.StatusOK {
		t.Fatalf("POST /api/locale status = %d, want 200: %s", localeRec.Code, localeRec.Body.String())
	}
	var locale struct {
		Success bool   `json:"success"`
		Locale  string `json:"locale"`
	}
	if err := json.Unmarshal(localeRec.Body.Bytes(), &locale); err != nil {
		t.Fatalf("POST /api/locale body: %v", err)
	}
	if !locale.Success || locale.Locale != "vi" {
		t.Errorf("POST /api/locale = %+v, want {success:true locale:vi}", locale)
	}

	// Upstream exports POST only for /api/locale: GET must reach the route and
	// be refused as a method, not fall through to chi's not-found handler.
	localeGet := httptest.NewRecorder()
	r.ServeHTTP(localeGet, httptest.NewRequest(http.MethodGet, "/api/locale", nil))
	if localeGet.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /api/locale status = %d, want 405", localeGet.Code)
	}
	if got := localeGet.Header().Get("Allow"); got != http.MethodPost {
		t.Errorf("GET /api/locale Allow = %q, want POST", got)
	}

	// Dashboard-gated: an anonymous caller must be refused by the gate (401),
	// which proves the route matched. A 404 here would mean the endpoint is
	// reachable in a unit test and missing on the real server.
	gated := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/pricing"},
		{http.MethodGet, "/api/tags"},
		{http.MethodOptions, "/api/tags"},
		{http.MethodGet, "/api/mcp/browsermcp/sse"},
		{http.MethodPost, "/api/mcp/browsermcp/message"},
	}
	for _, tt := range gated {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(tt.method, tt.path, strings.NewReader(`{}`)))
			if rec.Code == http.StatusNotFound {
				t.Fatalf("%s %s is not registered (404)", tt.method, tt.path)
			}
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s %s status = %d, want the dashboard gate's 401", tt.method, tt.path, rec.Code)
			}
		})
	}

	// A dashboard session reaches the same routes.
	token, err := auth.Sign("meta-mcp-router-secret", time.Now())
	if err != nil {
		t.Fatalf("sign session token: %v", err)
	}
	session := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}

	pricingRec := session(http.MethodGet, "/api/pricing", "")
	if pricingRec.Code != http.StatusOK {
		t.Fatalf("GET /api/pricing with a session = %d, want 200: %s", pricingRec.Code, pricingRec.Body.String())
	}
	var pricingBody map[string]any
	if err := json.Unmarshal(pricingRec.Body.Bytes(), &pricingBody); err != nil {
		t.Fatalf("GET /api/pricing body: %v", err)
	}
	if _, ok := pricingBody["gh"]; !ok {
		t.Errorf("GET /api/pricing payload has no gh provider: %v", pricingBody)
	}

	tagsRec := session(http.MethodGet, "/api/tags", "")
	if tagsRec.Code != http.StatusOK {
		t.Fatalf("GET /api/tags with a session = %d, want 200: %s", tagsRec.Code, tagsRec.Body.String())
	}
	var tagsBody struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(tagsRec.Body.Bytes(), &tagsBody); err != nil {
		t.Fatalf("GET /api/tags body: %v", err)
	}
	if len(tagsBody.Models) == 0 {
		t.Error("GET /api/tags listed no models")
	}
	if got := tagsRec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("GET /api/tags ACAO = %q, want * (upstream CORS_HEADERS)", got)
	}

	preflight := session(http.MethodOptions, "/api/tags", "")
	if preflight.Code != http.StatusOK {
		t.Fatalf("OPTIONS /api/tags = %d, want 200 (upstream's empty preflight response)", preflight.Code)
	}
	if got := preflight.Header().Get("Access-Control-Allow-Methods"); !strings.Contains(got, "GET") {
		t.Errorf("OPTIONS /api/tags Allow-Methods = %q, want GET listed", got)
	}
}
