package handlers

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"panrouter/proxy/internal/db"
	"panrouter/proxy/internal/handlers/chat"
	"panrouter/proxy/internal/handlers/media"
)

// engineRoute is one registration mountEngineRoutes makes.
type engineRoute struct {
	method  string
	pattern string
}

// engineRouteRecorder implements routeRegistrar and records what
// mountEngineRoutes (the engine surface SetupRoutes mounts) registers. It is
// the test-side half of the coverage guard below: the route set is enumerated
// from production code, so a route added to the engine surface without an
// /api/v1 twin fails the walk instead of passing a hand-written list nobody
// updated.
type engineRouteRecorder struct{ routes []engineRoute }

func (rec *engineRouteRecorder) add(method, pattern string) {
	rec.routes = append(rec.routes, engineRoute{method: method, pattern: pattern})
}

func (rec *engineRouteRecorder) Get(pattern string, _ http.HandlerFunc) {
	rec.add(http.MethodGet, pattern)
}

func (rec *engineRouteRecorder) Post(pattern string, _ http.HandlerFunc) {
	rec.add(http.MethodPost, pattern)
}

func (rec *engineRouteRecorder) Put(pattern string, _ http.HandlerFunc) {
	rec.add(http.MethodPut, pattern)
}

func (rec *engineRouteRecorder) Patch(pattern string, _ http.HandlerFunc) {
	rec.add(http.MethodPatch, pattern)
}

func (rec *engineRouteRecorder) Delete(pattern string, _ http.HandlerFunc) {
	rec.add(http.MethodDelete, pattern)
}

func (rec *engineRouteRecorder) HandleFunc(pattern string, _ http.HandlerFunc) {
	rec.add("ANY", pattern)
}

// TestAPIv1AliasCoversEveryEngineRoute is the coverage guard for the second
// spelling upstream publishes. next.config.mjs rewrites /v1/:path* onto
// /api/v1/:path*, so every engine endpoint answers at both; this port served
// /v1 only (middleware.RequestLogger strips the prefix and chi matches the bare
// route), and a Node-style client calling /api/v1/chat/completions got a 404.
//
// Both sides of the assertion are derived. The expected set is what
// mountEngineRoutes registers — the very function SetupRoutes mounts — and the
// observed side is a chi.Walk of the router SetupServerRouter builds. A route
// the engine adds without the alias, an alias wired to a different handler, or
// an alias mounted outside the API-key group all fail here.

// concretePath turns a chi wildcard pattern into a path a probe can request.
// "/v1beta/models/*" is matched by the router, but a request literally for "*"
// is not, so the probe substitutes a suffix and reads the gate's status instead
// of the router's 404.
func concretePath(pattern string) string {
	if strings.HasSuffix(pattern, "/*") {
		return strings.TrimSuffix(pattern, "*") + "gemini-2.5-flash:generateContent"
	}
	return pattern
}

func TestAPIv1AliasCoversEveryEngineRoute(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()
	repo := db.NewRepo(database)

	r := chi.NewRouter()
	SetupServerRouter(r, repo, nil)

	registered := map[string]http.Handler{}
	if err := chi.Walk(r, func(method, route string, handler http.Handler, _ ...func(http.Handler) http.Handler) error {
		registered[method+" "+route] = handler
		return nil
	}); err != nil {
		t.Fatalf("walk routes: %v", err)
	}

	rec := &engineRouteRecorder{}
	mountEngineRoutes(rec, chat.NewChatHandler(repo), media.NewMediaHandler(repo, nil, nil))
	if len(rec.routes) == 0 {
		t.Fatal("mountEngineRoutes registered nothing; the coverage guard would pass vacuously")
	}

	// A key that is not in the table: the engine group's RequireApiKey rejects
	// it before any handler runs, so probing every route is side-effect free
	// while the status still separates "matched this group" (401) from "no such
	// route" (404).
	probe := func(method, path string) int {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer sk-not-a-real-key")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}

	posts := 0
	for _, rt := range rec.routes {
		bare := rt.method + " " + rt.pattern
		alias := rt.method + " " + apiV1Alias(rt.pattern)

		bareHandler, ok := registered[bare]
		if !ok {
			t.Errorf("%s is not registered on the server router", bare)
			continue
		}
		aliasHandler, ok := registered[alias]
		if !ok {
			t.Errorf("%s has no alias: %s is served but %s is not registered",
				bare, rt.pattern, apiV1Alias(rt.pattern))
			continue
		}
		if reflect.ValueOf(bareHandler).Pointer() != reflect.ValueOf(aliasHandler).Pointer() {
			t.Errorf("%s and %s point at different handlers", bare, alias)
		}

		if rt.method != http.MethodPost {
			continue
		}
		// Every POST engine route answers behind the same gate at both
		// spellings — the API-key group, never the dashboard group.
		posts++
		if got := probe(rt.method, concretePath(rt.pattern)); got != http.StatusUnauthorized {
			t.Errorf("POST %s with a bogus key = %d, want 401 (engine group gate)", rt.pattern, got)
		}
		if got := probe(rt.method, apiV1Alias(rt.pattern)); got != http.StatusUnauthorized {
			t.Errorf("POST %s with a bogus key = %d, want 401 (same gate as %s)",
				apiV1Alias(rt.pattern), got, rt.pattern)
		}
	}
	if posts == 0 {
		t.Fatal("the engine surface registered no POST route; the /api/v1 contract is unguarded")
	}

	// Nothing else may live under /api/v1: the router's alias subtree has to be
	// exactly the engine mount, so a hand-written /api/v1 route cannot appear
	// without a twin in the surface above.
	want := make(map[string]bool, len(rec.routes))
	for _, rt := range rec.routes {
		want[rt.method+" "+apiV1Alias(rt.pattern)] = true
	}
	for key := range registered {
		_, path, ok := strings.Cut(key, " ")
		if !ok || !strings.HasPrefix(path, apiV1Prefix+"/") {
			continue
		}
		if !want[key] {
			t.Errorf("%s is registered under %s but is not part of mountEngineRoutes", key, apiV1Prefix)
		}
	}

	// The probe must not be trivially 404-shaped, or every check above would
	// pass on a router that answered nothing.
	if got := probe(http.MethodPost, apiV1Prefix+"/definitely-not-an-engine-route"); got != http.StatusNotFound {
		t.Errorf("POST %s/definitely-not-an-engine-route = %d, want 404", apiV1Prefix, got)
	}
}
