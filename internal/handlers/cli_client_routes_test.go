package handlers

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"panrouter/proxy/internal/auth"
	"panrouter/proxy/internal/clientconfig"
	"panrouter/proxy/internal/db"
)

// TestSetupServerRouter_EveryClientAdapterHasARoute is the coverage guard for
// the dashboard-only client adapters: clientconfig.IDs() is the list of
// clients, and the router registers one settings route per id. Walking the ids
// rather than naming them here is the point — an adapter added to the registry
// without a route, or a route that shadows another, fails this test instead of
// 404ing in the dashboard.
func TestSetupServerRouter_EveryClientAdapterHasARoute(t *testing.T) {
	t.Setenv("JWT_SECRET", "client-adapter-router-secret")

	// Every path resolver is pointed at a temp home and an empty PATH, so no
	// developer install and no real config can change what the route reports.
	home := t.TempDir()
	for _, key := range []string{"HOME", "USERPROFILE"} {
		t.Setenv(key, home)
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("APPDATA", filepath.Join(home, "roaming"))
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "local"))
	t.Setenv("PATH", filepath.Join(home, "empty-path"))

	database, cleanup := setupTestDB(t)
	defer cleanup()
	r := chi.NewRouter()
	SetupServerRouter(r, db.NewRepo(database), nil)

	token, err := auth.Sign("client-adapter-router-secret", time.Now())
	if err != nil {
		t.Fatalf("sign session token: %v", err)
	}
	authed := func(method, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}

	ids := clientconfig.IDs()
	if len(ids) == 0 {
		t.Fatal("clientconfig.IDs() is empty: the router loop would register no client route")
	}
	for _, id := range ids {
		path := "/api/cli-tools/" + id + "-settings"
		for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
			rec := authed(method, path)
			if rec.Code == http.StatusNotFound {
				t.Errorf("%s %s is not registered on the production router: %s",
					method, path, rec.Body.String())
			}
		}
	}
	// A name outside the registry must still miss, so the loop is proven to
	// register ids rather than a catch-all.
	if rec := authed(http.MethodGet, "/api/cli-tools/not-a-client-settings"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown client answered %d, want 404", rec.Code)
	}
}
