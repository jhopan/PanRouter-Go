package db

import (
	json "encoding/json/v2"
	"strings"
	"testing"
	"time"

	"panrouter/proxy/internal/providers"
)

// decodeConnData reads a connection data blob back as a map, so a test asserts on
// the stored JSON instead of on an implementation detail of the writer.
func decodeConnData(t *testing.T, raw string) map[string]any {
	t.Helper()
	var data map[string]any
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		t.Fatalf("decode connection data: %v (raw %s)", err, raw)
	}
	return data
}

// A spent-budget verdict has to store the quota window itself, with the same
// deadline the account cooldown uses and the fields upstream writes beside it
// (src/sse/services/auth.js:539-566). One reset instant for the lock, the park
// and the message is the point: two dates would let the dashboard show one while
// the router unparks at the other.
func TestLockConnectionQuotaExhausted_RecordsTheParkAndItsDeadline(t *testing.T) {
	repo, cleanup := setupHealthConnTestDB(t)
	defer cleanup()
	insertTestConn(t, repo)

	until := time.Now().UTC().Add(7 * 24 * time.Hour).Truncate(time.Second)
	if err := repo.LockConnectionQuotaExhausted("conn-1", until, 2, 429); err != nil {
		t.Fatalf("LockConnectionQuotaExhausted: %v", err)
	}

	raw := readConnData(t, repo, "conn-1")
	stored := decodeConnData(t, raw)
	want := until.Format(time.RFC3339)
	for _, key := range []string{"rateLimitedUntil", "quotaExhaustedUntil"} {
		if got, _ := stored[key].(string); got != want {
			t.Errorf("%s = %v, want %s — the lock and the quota park share one deadline", key, stored[key], want)
		}
	}
	if got, _ := stored["testStatus"].(string); got != providers.StatusUnavailable {
		t.Errorf("testStatus = %v, want %q", stored["testStatus"], providers.StatusUnavailable)
	}
	if got, ok := stored["errorCode"].(float64); !ok || int(got) != 429 {
		t.Errorf("errorCode = %v, want 429", stored["errorCode"])
	}
	if got, ok := stored["backoffLevel"].(float64); !ok || int(got) != 2 {
		t.Errorf("backoffLevel = %v, want 2", stored["backoffLevel"])
	}
	at, _ := stored["lastErrorAt"].(string)
	if _, err := time.Parse(time.RFC3339, at); err != nil {
		t.Errorf("lastErrorAt = %q, want an RFC3339 instant: %v", at, err)
	}
	lastError, _ := stored["lastError"].(map[string]any)
	message, _ := lastError["message"].(string)
	if !strings.Contains(message, "Quota exhausted — resets "+want) {
		t.Errorf("lastError = %v, want the upstream sentence naming %s", stored["lastError"], want)
	}

	// The readers routing and the dashboard go through answer with that instant.
	if got, ok := ConnectionCooldownUntil(raw); !ok || !got.Equal(until) {
		t.Errorf("ConnectionCooldownUntil = %v (ok=%v), want %s", got, ok, until)
	}
	if got, ok := ConnectionBlockedUntil(raw); !ok || !got.Equal(until) {
		t.Errorf("ConnectionBlockedUntil = %v (ok=%v), want %s", got, ok, until)
	}
}

// A quota park answers before the generic cooldown, because a quota refusal need
// not write one — upstream reads the same pair in the same order
// (open-sse/services/routerQuotaState.js:56). A park that has already lapsed is
// not a park, and it must not hide a live cooldown behind it either.
func TestConnectionCooldownUntil_ReadsTheQuotaPark(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	const (
		quotaFuture = "2026-11-22T12:00:00Z"
		quotaPast   = "2026-11-01T12:00:00Z"
		lockFuture  = "2026-11-15T13:00:00Z"
		lockPast    = "2026-11-15T11:00:00Z"
	)
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "quota park only",
			raw:  `{"quotaExhaustedUntil":"` + quotaFuture + `"}`,
			want: quotaFuture,
		},
		{
			name: "quota park wins over the generic cooldown",
			raw:  `{"quotaExhaustedUntil":"` + quotaFuture + `","rateLimitedUntil":"` + lockFuture + `"}`,
			want: quotaFuture,
		},
		{
			name: "lapsed quota park falls back to the live cooldown",
			raw:  `{"quotaExhaustedUntil":"` + quotaPast + `","rateLimitedUntil":"` + lockFuture + `"}`,
			want: lockFuture,
		},
		{
			name: "lapsed quota park alone parks nothing",
			raw:  `{"quotaExhaustedUntil":"` + quotaPast + `"}`,
		},
		{
			name: "lapsed cooldown alone parks nothing",
			raw:  `{"rateLimitedUntil":"` + lockPast + `"}`,
		},
		{
			name: "unreadable quota park leaves the cooldown to answer",
			raw:  `{"quotaExhaustedUntil":"soon","rateLimitedUntil":"` + lockFuture + `"}`,
			want: lockFuture,
		},
		{
			name: "unreadable cooldown leaves the quota park to answer",
			raw:  `{"quotaExhaustedUntil":"` + quotaFuture + `","rateLimitedUntil":"soon"}`,
			want: quotaFuture,
		},
		{
			name: "quota park of the wrong type parks nothing",
			raw:  `{"quotaExhaustedUntil":12345}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			until, ok := ConnectionCooldownUntilAt(tt.raw, now)
			if ok != (tt.want != "") {
				t.Fatalf("ConnectionCooldownUntil(%s) ok = %v, want %v", tt.raw, ok, tt.want != "")
			}
			if !ok {
				return
			}
			if got := until.UTC().Format(time.RFC3339); got != tt.want {
				t.Errorf("cooldown = %s, want %s", got, tt.want)
			}
		})
	}
}

// The selector's one question — is this account parked — has to include the
// quota park, and only while it is live: a stale date must neither park the
// connection nor hide a live park behind it.
func TestConnectionBlockedUntil_ReadsTheQuotaParkWhileItIsLive(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	const (
		quotaFuture = "2026-11-22T12:00:00Z"
		quotaPast   = "2026-11-01T12:00:00Z"
		lockFuture  = "2026-11-15T13:00:00Z"
		lockPast    = "2026-11-15T11:00:00Z"
	)
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "quota park only",
			raw:  `{"quotaExhaustedUntil":"` + quotaFuture + `"}`,
			want: quotaFuture,
		},
		{
			name: "earliest live park wins",
			raw:  `{"quotaExhaustedUntil":"` + quotaFuture + `","rateLimitedUntil":"` + lockFuture + `"}`,
			want: lockFuture,
		},
		{
			name: "lapsed quota park alone parks nothing",
			raw:  `{"quotaExhaustedUntil":"` + quotaPast + `"}`,
		},
		{
			name: "lapsed quota park does not hide the live cooldown",
			raw:  `{"quotaExhaustedUntil":"` + quotaPast + `","rateLimitedUntil":"` + lockFuture + `"}`,
			want: lockFuture,
		},
		{
			name: "lapsed cooldown alone parks nothing",
			raw:  `{"rateLimitedUntil":"` + lockPast + `"}`,
		},
		{
			name: "unreadable quota park leaves the cooldown to answer",
			raw:  `{"quotaExhaustedUntil":null,"rateLimitedUntil":"` + lockFuture + `"}`,
			want: lockFuture,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			until, ok := ConnectionBlockedUntilAt(tt.raw, now)
			if ok != (tt.want != "") {
				t.Fatalf("ConnectionBlockedUntil(%s) ok = %v, want %v", tt.raw, ok, tt.want != "")
			}
			if !ok {
				return
			}
			if got := until.UTC().Format(time.RFC3339); got != tt.want {
				t.Errorf("park = %s, want %s", got, tt.want)
			}
		})
	}
}

// A served request proves the account works, so the quota park goes with the
// generic cooldown — upstream drops both in clearAccountError
// (src/sse/services/auth.js:620-627).
func TestClearConnectionRateLimit_ClearsTheQuotaPark(t *testing.T) {
	repo, cleanup := setupHealthConnTestDB(t)
	defer cleanup()
	insertTestConn(t, repo)

	if err := repo.LockConnectionQuotaExhausted("conn-1", time.Now().UTC().Add(7*24*time.Hour), 1, 429); err != nil {
		t.Fatalf("LockConnectionQuotaExhausted: %v", err)
	}
	if err := repo.ClearConnectionRateLimit("conn-1"); err != nil {
		t.Fatalf("ClearConnectionRateLimit: %v", err)
	}

	raw := readConnData(t, repo, "conn-1")
	if until, ok := ConnectionCooldownUntil(raw); ok {
		t.Errorf("cooldown until %s survived a served request", until)
	}
	if until, ok := ConnectionBlockedUntil(raw); ok {
		t.Errorf("park until %s survived a served request", until)
	}
	stored := decodeConnData(t, raw)
	if stored["quotaExhaustedUntil"] != nil {
		t.Errorf("quotaExhaustedUntil = %v, want null", stored["quotaExhaustedUntil"])
	}
	if stored["rateLimitedUntil"] != nil {
		t.Errorf("rateLimitedUntil = %v, want null", stored["rateLimitedUntil"])
	}
}

// TestResetConnectionHealthState_ClearsTheQuotaPark pins the park's lifetime on
// the operator-reset path.
//
// The quota park sits beside rateLimitedUntil and oauthLockedUntil on the same
// connection row, and the Claude/Codex credit-reset panels call this helper to
// bring an account back. Leaving the park behind makes the reset look like it did
// nothing: rotation keeps skipping the account until the provider's own window
// ends, which is the wait the button exists to skip.
func TestResetConnectionHealthState_ClearsTheQuotaPark(t *testing.T) {
	repo, cleanup := setupHealthConnTestDB(t)
	defer cleanup()
	insertTestConn(t, repo)

	until := time.Now().UTC().Add(7 * 24 * time.Hour).Truncate(time.Second)
	if err := repo.LockConnectionQuotaExhausted("conn-1", until, 2, 429); err != nil {
		t.Fatalf("LockConnectionQuotaExhausted: %v", err)
	}
	if _, ok := ConnectionCooldownUntil(readConnData(t, repo, "conn-1")); !ok {
		t.Fatal("precondition: the fixture must carry a live quota park")
	}

	if err := repo.ResetConnectionHealthState("conn-1"); err != nil {
		t.Fatalf("ResetConnectionHealthState: %v", err)
	}

	raw := readConnData(t, repo, "conn-1")
	if got, ok := ConnectionCooldownUntil(raw); ok {
		t.Errorf("the account is still parked until %s after a health reset", got)
	}
	if value, present := decodeConnData(t, raw)["quotaExhaustedUntil"]; present && value != nil {
		t.Errorf("quotaExhaustedUntil = %v after a health reset, want it cleared", value)
	}
}
