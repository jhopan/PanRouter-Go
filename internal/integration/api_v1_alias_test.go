//go:build integration

package integration

import (
	"net/http"
	"testing"
)

// TestV1APIv1AliasReachesTheSameEngineHandler proves the /api/v1 spelling end
// to end, through the production stack (app.ProvideRouter, RequestLogger
// included). next.config.mjs rewrites /v1/:path* onto /api/v1/:path*, so a
// Node-style client may call either spelling for the same endpoint; before the
// alias existed, POST /api/v1/chat/completions fell through to chi's 404 while
// every /v1 test stayed green.
func TestV1APIv1AliasReachesTheSameEngineHandler(t *testing.T) {
	env, upstream := newProviderEnv(t)

	for _, path := range []string{"/v1/chat/completions", "/api/v1/chat/completions"} {
		res := env.Post(t, path, ChatBody("deepseek/deepseek-chat", false))
		if res.Status != http.StatusOK {
			t.Fatalf("POST %s = %d, want 200 (body: %s)", path, res.Status, truncate(res.Body))
		}
		// The same provider lane and model as the /v1 call: the alias is not a
		// second, thinner handler that skips model resolution.
		if got := upstream.Last(t).Model(t); got != "deepseek-chat" {
			t.Errorf("POST %s reached the provider with model %q, want deepseek-chat", path, got)
		}
	}
	if got := upstream.Count(); got != 2 {
		t.Errorf("upstream received %d requests, want 2: both spellings must reach the provider", got)
	}

	// The GET half of the surface rides the same prefix — /api/v1/models is
	// upstream's own URL for the listing.
	if res := env.Get(t, "/api/v1/models"); res.Status != http.StatusOK {
		t.Errorf("GET /api/v1/models = %d, want 200 (body: %s)", res.Status, truncate(res.Body))
	}

	// Same gate as /v1, not the dashboard group and not a hole: a key the table
	// does not know is refused by the engine middleware, so the status is 401
	// rather than the router's 404.
	bogus := env.Post(t, "/api/v1/chat/completions", ChatBody("deepseek/deepseek-chat", false), WithAPIKey("sk-not-a-real-key"))
	if bogus.Status != http.StatusUnauthorized {
		t.Errorf("POST /api/v1/chat/completions with a bogus key = %d, want 401 (body: %s)",
			bogus.Status, truncate(bogus.Body))
	}
	if got := upstream.Count(); got != 2 {
		t.Errorf("the refused request reached the provider: upstream count %d, want 2", got)
	}
}