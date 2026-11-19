package chat

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"panrouter/proxy/internal/db"
	"panrouter/proxy/internal/providers"
)

// TestHandleAccountFallback_AntigravityDetailFreeRefusalOnlyLocksModel pins the
// refusal reproduced on the VPS: the same account/model answers 200 to the
// dashboard's minimal test but a long agent request gets this detail-free 429.
// It carries no reset, quota row, RetryInfo or model-capacity detail, so it is a
// request fingerprint verdict, not evidence that the account spent its weekly
// allowance. It may cool THIS model briefly; it must never write an account
// cooldown or a quota-exhausted park.
func TestHandleAccountFallback_AntigravityDetailFreeRefusalOnlyLocksModel(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "loadCodeAssist") {
			_, _ = w.Write([]byte(`{"cloudaicompanionProject":"proj-test"}`))
			return
		}
		if strings.Contains(r.URL.Path, "fetchConfig") {
			_, _ = w.Write([]byte(`{"experimentConfig":{"experimentFlags":[{"name":"generic-system-prompt","boolValue":true}]}}`))
			return
		}
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(antigravityResourceExhaustedBody))
	}))
	defer upstream.Close()

	oldLoad, oldConfig := loadCodeAssistURL, providers.AntigravityAICodeBaseURL
	loadCodeAssistURL = upstream.URL + "/v1internal:loadCodeAssist"
	providers.AntigravityAICodeBaseURL = upstream.URL
	t.Cleanup(func() {
		loadCodeAssistURL = oldLoad
		providers.AntigravityAICodeBaseURL = oldConfig
		providers.ResetAntigravityExperiments()
	})
	providers.ResetAntigravityExperiments()

	database, cleanup := setupChatTestDB(t)
	defer cleanup()
	if _, err := database.Exec(`DELETE FROM providerConnections`); err != nil {
		t.Fatalf("clear seeded connections: %v", err)
	}
	const connID = "ag-fingerprint"
	seedConnDB(t, database, "antigravity", connID, "token-fingerprint", upstream.URL)

	handler := NewChatHandler(db.NewRepo(database))
	err := handler.handleAccountFallback(context.Background(), httptest.NewRecorder(), "antigravity", "gemini-3.8-flash-high", "",
		[]byte(`{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"hi"}]}`),
		false, false, "/chat/completions")
	if err == nil {
		t.Fatal("detail-free upstream refusal must surface after the model attempt")
	}

	var raw string
	if err := database.QueryRow(`SELECT data FROM providerConnections WHERE id = ?`, connID).Scan(&raw); err != nil {
		t.Fatalf("read connection: %v", err)
	}
	if _, parked := db.ConnectionCooldownUntil(raw); parked {
		t.Fatalf("detail-free refusal wrote an account cooldown: %s", raw)
	}
	if locked, err := db.NewRepo(database).IsConnectionModelLocked(connID, "gemini-3.8-flash-high"); err != nil || !locked {
		t.Fatalf("model lock = %v, %v; want a short model-only lock", locked, err)
	}
}
