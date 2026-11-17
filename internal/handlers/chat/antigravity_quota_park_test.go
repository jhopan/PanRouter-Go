package chat

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"panrouter/proxy/internal/db"
)

// TestHandleAccountFallback_AntigravityQuotaParksUntilTheWeeklyReset is the field
// case: the generation endpoint answers 429 RESOURCE_EXHAUSTED with no reset in the
// body. The account's own weekly reading names the moment, so the park must end
// there — the previous behaviour locked it for two seconds and re-dialled it all
// week, one guaranteed-failed request at a time.
func TestHandleAccountFallback_AntigravityQuotaParksUntilTheWeeklyReset(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// The onboarding RPC answers first: without a project ID the lane refuses to
		// dial at all, and this test is about the quota verdict, not onboarding.
		if strings.Contains(r.URL.Path, "loadCodeAssist") {
			_, _ = w.Write([]byte(`{"cloudaicompanionProject":"proj-test"}`))
			return
		}
		hits.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(antigravityResourceExhaustedBody))
	}))
	defer upstream.Close()

	previousLCA := loadCodeAssistURL
	loadCodeAssistURL = upstream.URL + "/v1internal:loadCodeAssist"
	t.Cleanup(func() { loadCodeAssistURL = previousLCA })

	database, cleanup := setupChatTestDB(t)
	defer cleanup()
	if _, err := database.Exec(`DELETE FROM providerConnections`); err != nil {
		t.Fatalf("clear seeded connections: %v", err)
	}
	seedConnDB(t, database, "antigravity", "ag-quota-1", "token-1", upstream.URL)

	repo := db.NewRepo(database)
	handler := NewChatHandler(repo)

	// The reading the tracker already holds: the weekly bucket refills in ~3 days.
	installAntigravityKV(t)
	reading := time.Now().UTC().Add(72 * time.Hour).Truncate(time.Second)
	seedAntigravityQuotas("ag-quota-1", map[string]AntigravityModelQuota{
		"gemini_weekly": {RemainingPercentage: 0, ResetAt: reading},
	})

	err := handler.handleAccountFallback(context.Background(), httptest.NewRecorder(), "antigravity", "claude-sonnet-4.5", "",
		[]byte(`{"model":"claude-sonnet-4.5","messages":[{"role":"user","content":"hi"}]}`),
		false, false, "/chat/completions")
	if err == nil {
		t.Fatal("an exhausted quota must surface, not be answered")
	}

	var raw string
	if qerr := database.QueryRow(`SELECT data FROM providerConnections WHERE id = 'ag-quota-1'`).Scan(&raw); qerr != nil {
		t.Fatalf("read connection data: %v", qerr)
	}
	until, ok := db.ConnectionCooldownUntil(raw)
	if !ok {
		t.Fatalf("no park recorded on the exhausted account: %s", raw)
	}
	if wait := time.Until(until); wait < 71*time.Hour || wait > 73*time.Hour {
		t.Errorf("parked for %v, want the bucket's ~72h reset (not the 2s burst base)", wait)
	}
	// And the selector sees it as blocked, so the next request does not re-dial it.
	if blocked, ok := db.ConnectionBlockedUntil(raw); !ok || blocked.Before(reading.Add(-time.Minute)) {
		t.Errorf("account not excluded from rotation until the reset: %v %v", blocked, ok)
	}

	// A Claude turn is not pre-filtered by the gemini_weekly bucket, so the account
	// is dialled and the park is what the reading decides. (A Gemini turn with that
	// reading is skipped before the request at all — see the selection test.)
	// One turn costs the in-place retries the reference allows, not a pool walk.
	if got := hits.Load(); got == 0 || got > 4 {
		t.Errorf("upstream requests = %d, want the initial attempt plus bounded retries", got)
	}
}
