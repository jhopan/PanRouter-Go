package chat

import (
	"context"
	"database/sql"
	json "encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"panrouter/proxy/internal/db"
	"panrouter/proxy/internal/providers"
)

// quotaParkedConn reads a connection row as stored, and decodes its data blob,
// so a test asserts on the persisted park rather than on a log line.
func quotaParkedConn(t *testing.T, database *sql.DB, connID string) (string, map[string]any) {
	t.Helper()
	var raw string
	if err := database.QueryRow(`SELECT data FROM providerConnections WHERE id = ?`, connID).Scan(&raw); err != nil {
		t.Fatalf("read connection %s: %v", connID, err)
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		t.Fatalf("decode connection %s data: %v (raw %s)", connID, err, raw)
	}
	return raw, data
}

// allowanceBodyAt renders Token Harbor's free-allowance refusal with a reset the
// park parser accepts: the exact sentence shape upstream matches (TH_RESET_RE,
// src/sse/services/auth.js:22), built from a real clock so the instant is always
// ahead of the request under test.
func allowanceBodyAt(reset time.Time) string {
	utc := reset.UTC()
	return "You've used this period's free allowance. " +
		"Your next rolling 7-day period starts on " + utc.Format("2 Jan 2006") +
		" at " + utc.Format("15:04") + " UTC."
}

// refuseWith answers every request with one status and body.
func refuseWith(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A quota-exhaustion verdict parks the account until the window the provider
// named and records that deadline as the quota park, which is what the dashboard
// and the router's quota readout consult (upstream src/sse/services/auth.js:552).
// Token Harbor's free allowance is the first case of it: its 429 body names the
// exact instant the next 7-day window starts.
func TestQuotaVerdictRecordsTheParkUntilTheNamedWindow(t *testing.T) {
	reset := time.Now().UTC().Add(72 * time.Hour).Truncate(time.Minute)
	srv := refuseWith(t, http.StatusTooManyRequests, allowanceBodyAt(reset))

	database, cleanup := setupChatTestDB(t)
	defer cleanup()
	seedConnDB(t, database, "tokenharbor", "conn-th", "sk-th", srv.URL)
	h := NewChatHandler(db.NewRepo(database))

	reqBody := []byte(`{"model":"grok-4.7","messages":[{"role":"user","content":"hi"}]}`)
	if err := h.handleAccountFallback(context.Background(), httptest.NewRecorder(), "tokenharbor", "grok-4.7", "", reqBody, false, false, "/v1/chat/completions"); err == nil {
		t.Fatal("expected the free-allowance 429 to surface as an error")
	}

	raw, stored := quotaParkedConn(t, database, "conn-th")
	park, _ := stored["quotaExhaustedUntil"].(string)
	if park == "" {
		t.Fatalf("no quota park recorded: %v", stored)
	}
	if lock, _ := stored["rateLimitedUntil"].(string); lock != park {
		t.Errorf("rateLimitedUntil = %q but quotaExhaustedUntil = %q; the lock and the park share one deadline", lock, park)
	}
	got, err := time.Parse(time.RFC3339, park)
	if err != nil {
		t.Fatalf("quota park %q is not RFC3339: %v", park, err)
	}
	// The parsed instant wins over both the classifier's flat hour and the
	// weekly calendar fallback it shares a ceiling with.
	if diff := got.Sub(reset); diff > time.Second || diff < -time.Second {
		t.Errorf("quota park ends at %s, want the instant the body named (%s)", got, reset)
	}
	if status, _ := stored["testStatus"].(string); status != providers.StatusUnavailable {
		t.Errorf("testStatus = %q, want %q", status, providers.StatusUnavailable)
	}
	if code, ok := stored["errorCode"].(float64); !ok || int(code) != http.StatusTooManyRequests {
		t.Errorf("errorCode = %v, want 429", stored["errorCode"])
	}
	// The classifier advances the level on the 429 body it read (the prose body
	// carries no text rule), and the park records the level that produced it.
	if level, ok := stored["backoffLevel"].(float64); !ok || int(level) != 1 {
		t.Errorf("backoffLevel = %v, want 1", stored["backoffLevel"])
	}
	stamp, _ := stored["lastErrorAt"].(string)
	if _, err := time.Parse(time.RFC3339, stamp); err != nil {
		t.Errorf("lastErrorAt = %q, want an RFC3339 instant: %v", stamp, err)
	}
	lastError, _ := stored["lastError"].(map[string]any)
	message, _ := lastError["message"].(string)
	if !strings.Contains(message, "Quota exhausted — resets "+park) {
		t.Errorf("lastError = %v, want the upstream sentence naming %s", stored["lastError"], park)
	}

	// Both readers route and render from answer with the parked instant.
	until, ok := db.ConnectionCooldownUntil(raw)
	if !ok || !until.Equal(got) {
		t.Errorf("ConnectionCooldownUntil = %v (ok=%v), want %s", until, ok, got)
	}
	if blocked, ok := db.ConnectionBlockedUntil(raw); !ok || !blocked.Equal(got) {
		t.Errorf("ConnectionBlockedUntil = %v (ok=%v), want %s", blocked, ok, got)
	}
}

// A plain throttle is not a spent budget: the account waits out the window and
// keeps the generic cooldown, with no quota park recorded (upstream's own suite
// pins the same split: "generic rate limits keep the short backoff path").
func TestThrottleVerdictLeavesTheQuotaParkUnset(t *testing.T) {
	srv := refuseWith(t, http.StatusTooManyRequests, `{"error":{"message":"Rate limit reached. Please slow down.","type":"rate_limit_error"}}`)

	database, cleanup := setupChatTestDB(t)
	defer cleanup()
	seedConnDB(t, database, "tokenharbor", "conn-th", "sk-th", srv.URL)
	h := NewChatHandler(db.NewRepo(database))

	reqBody := []byte(`{"model":"grok-4.7","messages":[{"role":"user","content":"hi"}]}`)
	if err := h.handleAccountFallback(context.Background(), httptest.NewRecorder(), "tokenharbor", "grok-4.7", "", reqBody, false, false, "/v1/chat/completions"); err == nil {
		t.Fatal("expected the 429 to surface as an error")
	}

	raw, stored := quotaParkedConn(t, database, "conn-th")
	if stored["quotaExhaustedUntil"] != nil {
		t.Errorf("a throttle recorded a quota park: %v", stored["quotaExhaustedUntil"])
	}
	if stored["testStatus"] != nil {
		t.Errorf("a throttle stamped testStatus = %v, want it untouched", stored["testStatus"])
	}
	lock, _ := stored["rateLimitedUntil"].(string)
	if lock == "" {
		t.Fatalf("the account was not cooled down at all: %v", stored)
	}
	// What the readers answer is the generic cooldown, not a quota park.
	until, ok := db.ConnectionCooldownUntil(raw)
	if !ok {
		t.Fatal("expected the account to be in cooldown after a 429")
	}
	if !until.After(time.Now()) {
		t.Errorf("cooldown until %s is not in the future", until)
	}
	if want, err := time.Parse(time.RFC3339, lock); err == nil && !until.Equal(want) {
		t.Errorf("reader answered %s, want the stored generic cooldown %s", until, want)
	}
}

// A quota verdict on an account-scoped status the cooldown table states as a
// bare code is still a spent budget: errclass reads a 402 as KindQuotaExhausted,
// so the park is recorded even though no body wording matched a rule.
func TestQuotaVerdictReadsThePaymentRequiredStatus(t *testing.T) {
	srv := refuseWith(t, http.StatusPaymentRequired, `{"error":{"message":"Payment required"}}`)

	database, cleanup := setupChatTestDB(t)
	defer cleanup()
	seedConnDB(t, database, "tokenharbor", "conn-th", "sk-th", srv.URL)
	h := NewChatHandler(db.NewRepo(database))

	reqBody := []byte(`{"model":"grok-4.7","messages":[{"role":"user","content":"hi"}]}`)
	if err := h.handleAccountFallback(context.Background(), httptest.NewRecorder(), "tokenharbor", "grok-4.7", "", reqBody, false, false, "/v1/chat/completions"); err == nil {
		t.Fatal("expected the 402 to surface as an error")
	}

	_, stored := quotaParkedConn(t, database, "conn-th")
	park, _ := stored["quotaExhaustedUntil"].(string)
	if park == "" {
		t.Fatalf("a 402 is a spent balance and must be parked as one: %v", stored)
	}
	if lock, _ := stored["rateLimitedUntil"].(string); lock != park {
		t.Errorf("rateLimitedUntil = %q but quotaExhaustedUntil = %q; want one deadline", lock, park)
	}
}

// A served request proves the account is usable again, so the persisted quota
// park goes with the account cooldown it was written beside. The forward path is
// driven directly: an account that is parked is exactly the one the selector
// skips, so a parked row can only be served through the forced path — which is
// what reaches this same success branch.
func TestServedRequestClearsTheQuotaPark(t *testing.T) {
	const okBody = `{"id":"chatcmpl-1","object":"chat.completion","created":0,"model":"grok-4.7",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okBody))
	}))
	t.Cleanup(srv.Close)

	database, cleanup := setupChatTestDB(t)
	defer cleanup()
	seedConnDB(t, database, "tokenharbor", "conn-th", "sk-th", srv.URL)
	repo := db.NewRepo(database)
	if err := repo.LockConnectionQuotaExhausted("conn-th", time.Now().UTC().Add(7*24*time.Hour), 1, http.StatusTooManyRequests); err != nil {
		t.Fatalf("park the account: %v", err)
	}
	h := NewChatHandler(repo)

	rec := httptest.NewRecorder()
	body := []byte(`{"model":"grok-4.7","messages":[{"role":"user","content":"hi"}]}`)
	err := h.tryForwardWithConnection(forwardRequestParams{
		Ctx: context.Background(), W: rec, Provider: "tokenharbor", Model: "grok-4.7",
		ConnectionID: "conn-th", ConnData: &ConnectionData{APIKey: "sk-th", BaseURL: srv.URL}, Body: body,
		IsStream: false, TranslateResponse: false, Endpoint: "/v1/chat/completions",
	})
	if err != nil {
		t.Fatalf("a served request must not fail: %v", err)
	}

	raw, stored := quotaParkedConn(t, database, "conn-th")
	if stored["quotaExhaustedUntil"] != nil {
		t.Errorf("quotaExhaustedUntil = %v, want null after a served request", stored["quotaExhaustedUntil"])
	}
	if until, ok := db.ConnectionCooldownUntil(raw); ok {
		t.Errorf("the account is still parked until %s after being served", until)
	}
}
