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

// A detail-free 429 against a bucket that still has quota must not park the
// account for the weekly window: the turn moves to the next account and answers.
// Measured 2026-11-18 — the same body parked an account for 111 hours while its
// own reading said `gemini_weekly: 99% remaining, 1/100 used`.
func TestHandleAccountFallback_AntigravityHealthyBucketRotatesOnDetailFree429(t *testing.T) {
	var firstHits, secondHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "loadCodeAssist") {
			_, _ = w.Write([]byte(`{"cloudaicompanionProject":"proj-test"}`))
			return
		}
		if r.Header.Get("Authorization") == "Bearer token-1" {
			firstHits.Add(1)
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(antigravityResourceExhaustedBody))
			return
		}
		secondHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"ok"}],"role":"model"}}]}`))
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
	seedConnDB(t, database, "antigravity", "ag-rot-1", "token-1", upstream.URL)
	seedConnDB(t, database, "antigravity", "ag-rot-2", "token-2", upstream.URL)

	repo := db.NewRepo(database)
	handler := NewChatHandler(repo)

	// The bucket is nearly full and refills in two days: the refusal below is not
	// this account's budget.
	installAntigravityKV(t)
	seedAntigravityQuotas("ag-rot-1", map[string]AntigravityModelQuota{
		"gemini_weekly": {RemainingPercentage: 99, ResetAt: time.Now().UTC().Add(48 * time.Hour)},
	})

	err := handler.handleAccountFallback(context.Background(), httptest.NewRecorder(), "antigravity", "gemini-3.8-flash-high", "",
		[]byte(`{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"hi"}]}`),
		false, false, "/chat/completions")
	if err != nil {
		t.Fatalf("the second account can serve this turn, so it must answer: %v", err)
	}
	if secondHits.Load() == 0 {
		t.Error("rotation never reached the second account")
	}
	if firstHits.Load() == 0 {
		t.Error("the first account was skipped before it was dialled; the fix is about the park, not the selection")
	}

	var raw string
	if qerr := database.QueryRow(`SELECT data FROM providerConnections WHERE id = 'ag-rot-1'`).Scan(&raw); qerr != nil {
		t.Fatalf("read connection data: %v", qerr)
	}
	if until, ok := db.ConnectionCooldownUntil(raw); ok {
		if wait := time.Until(until); wait > 6*time.Hour {
			t.Errorf("parked for %v, want the ordinary cooldown rather than the weekly window", wait)
		}
	}
}

// TestHandleAccountFallback_AntigravityQuotaLocksTheNamedModelUntilReset pins
// an authoritative quota response: it names QUOTA_EXHAUSTED and its reset. The
// affected model must wait for that reset without parking unrelated models on
// the account.
func TestHandleAccountFallback_AntigravityQuotaLocksTheNamedModelUntilReset(t *testing.T) {
	reading := time.Now().UTC().Add(72 * time.Hour).Truncate(time.Second)
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
		_, _ = w.Write([]byte(antigravityExhaustedBody(reading, "72h0m0s")))
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
	if _, parked := db.ConnectionCooldownUntil(raw); parked {
		t.Fatalf("authoritative model quota parked the whole account: %s", raw)
	}
	if locked, err := repo.IsConnectionModelLocked("ag-quota-1", "claude-sonnet-4.5"); err != nil || !locked {
		t.Fatalf("model lock = %v, %v; want true", locked, err)
	}

	// A Claude turn is not pre-filtered by the gemini_weekly bucket, so it is
	// dialled and the authoritative ErrorInfo is what records the model lock.
	// One turn costs the in-place retries the reference allows, not a pool walk.
	if got := hits.Load(); got == 0 || got > 4 {
		t.Errorf("upstream requests = %d, want the initial attempt plus bounded retries", got)
	}
}
