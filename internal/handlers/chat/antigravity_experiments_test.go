package chat

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"panrouter/proxy/internal/providers"
)

// TestRefreshAntigravityExperimentsReadsTheConfig pins the shape of the read.
//
// The captured client calls it with GET and no body; a POST answers 404, which
// silently left every account on the generic identity branch and logged
// "identity config read failed status=404" on each quota-error refresh.
func TestRefreshAntigravityExperimentsReadsTheConfig(t *testing.T) {
	providers.ResetAntigravityExperiments()
	t.Cleanup(providers.ResetAntigravityExperiments)

	var gotMethod, gotPath, gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"experimentConfig":{"experimentFlags":[
			{"name":"enable-hook-status","boolValue":true},
			{"name":"generic-system-prompt","boolValue":false}
		]}}`))
	}))
	defer server.Close()

	previous := providers.AntigravityAICodeBaseURL
	providers.AntigravityAICodeBaseURL = server.URL
	t.Cleanup(func() { providers.AntigravityAICodeBaseURL = previous })

	const token = "ya29.method-test"
	RefreshAntigravityExperiments(context.Background(), server.Client(), token)

	if gotMethod != http.MethodGet {
		t.Errorf("config read used %s, want GET (a POST answers 404)", gotMethod)
	}
	if gotPath != "/v1alpha:fetchConfig" {
		t.Errorf("config read path = %q, want /v1alpha:fetchConfig", gotPath)
	}
	if gotAuth != "Bearer "+token {
		t.Errorf("Authorization = %q, want the connection's bearer token", gotAuth)
	}
	if providers.AntigravityGenericSystemPromptForToken(token) {
		t.Error("a stored false must select the coding identity branch")
	}
	if providers.AntigravityNeedsExperimentRead(token) {
		t.Error("a successful read must satisfy the next request")
	}

	// A server that refuses leaves the account on the generic branch rather than
	// on a guessed one, and asks again soon.
	failingServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failingServer.Close()
	providers.AntigravityAICodeBaseURL = failingServer.URL

	const failing = "ya29.failing"
	RefreshAntigravityExperiments(context.Background(), failingServer.Client(), failing)
	if !providers.AntigravityGenericSystemPromptForToken(failing) {
		t.Error("the generic identity must survive a failed read")
	}
	if providers.AntigravityNeedsExperimentRead(failing) {
		t.Error("a failed read must use the short failure backoff, not retry on every request")
	}
}
