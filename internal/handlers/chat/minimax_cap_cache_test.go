// MiniMax Code plan-cap cache — parity with open-sse/services/minimaxCapCache.js and
// tests/unit/minimax-block-persistence.test.js.
//
// The failure it prevents: a plan that ran out mid-conversation (the 470k-token turn
// that produced the feature) answers "usage limit reached: insufficient balance
// (1008)", and every retry is a real request against a plan that is empty until it
// refills. The block is a flat 24h because the operator asked for one, and the usage
// tracker overwrites it the moment the plan comes back.
package chat

import (
	"errors"
	"testing"
	"time"

	json "panrouter/proxy/internal/fastjson"
)

// useMinimaxClock pins the cap cache's clock for one test.
func useMinimaxClock(t *testing.T, now time.Time) {
	t.Helper()
	previous := minimaxNow
	minimaxNow = func() time.Time { return now }
	t.Cleanup(func() { minimaxNow = previous })
}

// installMinimaxCapKV wires the shared fake KV store and clears the cache.
func installMinimaxCapKV(t *testing.T) *fakeAntigravityKV {
	t.Helper()
	store := newFakeAntigravityKV()
	SetKVStore(store)
	ResetMiniMaxCapCache()
	t.Cleanup(func() {
		SetKVStore(nil)
		ResetMiniMaxCapCache()
	})
	return store
}

// minimaxWindow builds one coding-plan window row.
func minimaxWindow(remainingPercent float64, resetAt time.Time) map[string]any {
	row := map[string]any{"remainingPercentage": remainingPercent, "total": 100}
	if resetAt.IsZero() {
		row["resetAt"] = nil
	} else {
		row["resetAt"] = resetAt.UTC().Format(time.RFC3339)
	}
	return map[string]any{"coding_plan": row}
}

func TestMinimaxCapBlocksASpentPlan(t *testing.T) {
	installMinimaxCapKV(t)
	base := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	useMinimaxClock(t, base)

	RecordMiniMaxUsage("conn-a", minimaxWindow(0, base.Add(3*time.Hour)))
	skip, until, reason := ShouldSkipMiniMaxAccount("conn-a")
	if !skip {
		t.Fatal("a plan with 0% left must keep the account out of rotation")
	}
	if reason != "plan window exhausted" {
		t.Errorf("reason = %q", reason)
	}
	if until == nil || !until.Equal(base.Add(3*time.Hour)) {
		t.Errorf("until = %v, want the window reset", until)
	}

	// A plan with quota left stays eligible.
	RecordMiniMaxUsage("conn-b", minimaxWindow(40, base.Add(3*time.Hour)))
	if skip, _, _ := ShouldSkipMiniMaxAccount("conn-b"); skip {
		t.Error("an account with quota left must stay eligible")
	}
}

func TestMinimaxCapBlockLastsTwentyFourHours(t *testing.T) {
	installMinimaxCapKV(t)
	base := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	useMinimaxClock(t, base)

	// A credit balance of zero names no reset: the user has to top up.
	MarkMiniMaxExhausted("conn-1", nil)
	if skip, _, reason := ShouldSkipMiniMaxAccount("conn-1"); !skip || reason != "credit balance exhausted" {
		t.Fatalf("skip = %v reason = %q, want the credit-balance block", skip, reason)
	}

	// Ten hours later the 5-minute reading TTL is long gone, but the 24h block holds.
	useMinimaxClock(t, base.Add(10*time.Hour))
	if skip, _, _ := ShouldSkipMiniMaxAccount("conn-1"); !skip {
		t.Error("the block must hold for its 24 hours, not its reading TTL")
	}
}

func TestMinimaxCapBlockPersistsAndRestores(t *testing.T) {
	store := installMinimaxCapKV(t)
	base := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	useMinimaxClock(t, base)

	MarkMiniMaxExhausted("conn-live", nil)
	MarkMiniMaxExhausted("conn-dead", nil)
	// One of them was written long enough ago that its 24h window closed while the
	// process was down.
	minimaxCapMu.Lock()
	dead := minimaxCapCache["conn-dead"]
	dead.CheckedAt = base.Add(-25 * time.Hour).UnixMilli()
	minimaxCapCache["conn-dead"] = dead
	encoded := mustMarshal(t, dead)
	minimaxCapMu.Unlock()
	if err := store.SetKV(MinimaxCapScope, "conn-dead", encoded); err != nil {
		t.Fatalf("seed stale row: %v", err)
	}
	ResetMiniMaxCapCache()

	if restored := LoadPersistedMiniMaxCaps(); restored != 1 {
		t.Fatalf("restored = %d, want 1 (the live block only)", restored)
	}
	if skip, _, _ := ShouldSkipMiniMaxAccount("conn-live"); !skip {
		t.Error("a restored block must still skip the account")
	}
	if _, ok := store.get(MinimaxCapScope, "conn-dead"); ok {
		t.Error("a block whose 24h window already passed must be deleted on load")
	}
}

func TestMinimaxCapHealthyReadingReleasesTheBlock(t *testing.T) {
	store := installMinimaxCapKV(t)
	base := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	useMinimaxClock(t, base)

	MarkMiniMaxExhausted("conn-1", nil)
	if _, ok := store.get(MinimaxCapScope, "conn-1"); !ok {
		t.Fatal("the block must be persisted so a restart keeps it")
	}

	// The plan refills: the tracker says so, and the account is eligible again
	// without waiting out the 24h block.
	RecordMiniMaxUsage("conn-1", minimaxWindow(100, base.Add(2*time.Hour)))
	if skip, _, _ := ShouldSkipMiniMaxAccount("conn-1"); skip {
		t.Error("a healthy reading must release the account immediately")
	}
	if _, ok := store.get(MinimaxCapScope, "conn-1"); ok {
		t.Error("a healthy reading must remove the persisted block")
	}
}

func TestMinimaxCapExpiredWindowIsDropped(t *testing.T) {
	installMinimaxCapKV(t)
	base := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	useMinimaxClock(t, base)

	// The window reset time is in the past: the plan is usable again.
	useMinimaxClock(t, base.Add(-time.Hour))
	RecordMiniMaxUsage("conn-1", minimaxWindow(0, base))
	useMinimaxClock(t, base)
	if skip, _, _ := ShouldSkipMiniMaxAccount("conn-1"); skip {
		t.Error("a window whose reset passed must not block")
	}
}

func TestMinimaxCapFailOpenOnKVError(t *testing.T) {
	store := installMinimaxCapKV(t)
	base := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	useMinimaxClock(t, base)
	store.err = errors.New("kv unavailable")

	MarkMiniMaxExhausted("conn-x", nil)
	RecordMiniMaxUsage("conn-x", minimaxWindow(0, base.Add(time.Hour)))
	ClearMiniMaxCap("conn-x")
	if restored := LoadPersistedMiniMaxCaps(); restored != 0 {
		t.Errorf("restored = %d, want 0 on a KV error", restored)
	}
	// The in-memory block still works: persistence is an optimisation, not the
	// mechanism.
	MarkMiniMaxExhausted("conn-y", nil)
	if skip, _, _ := ShouldSkipMiniMaxAccount("conn-y"); !skip {
		t.Error("the block must hold in memory while the KV store is down")
	}
}

// mustMarshal encodes one entry the way the persistence layer does.
func mustMarshal(t *testing.T, entry minimaxCapEntry) string {
	t.Helper()
	encoded, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal entry: %v", err)
	}
	return string(encoded)
}
