// Kiro monthly-cap pre-filter — parity with upstream
// tests/unit/kiro-cap-cache.test.js and kiro-cap-ttl.test.js.
//
// The two failures these pin are both about the *reason* the cap exists:
//   - a capped account must not be re-admitted because the probe reading aged
//     out (upstream kiro-cap-ttl: "STILL skips after the 5-minute probe TTL has
//     passed", "STILL skips a week later while the cap is in effect"); doing so
//     spends one request per model, per retry, for the rest of the month.
//   - a cap must survive a restart, and must not survive its own reset.
package chat

import (
	"errors"
	"testing"
	"time"
)

// kiroAt is the pointer form MarkKiroCapped takes, for a literal instant.
func kiroAt(t time.Time) *time.Time { return &t }

// useKiroClock pins the cap cache's clock for one test.
func useKiroClock(t *testing.T, now time.Time) {
	t.Helper()
	previous := kiroNow
	kiroNow = func() time.Time { return now }
	t.Cleanup(func() { kiroNow = previous })
}

// installKiroCapKV wires the shared fake KV store and clears the cache.
func installKiroCapKV(t *testing.T) *fakeAntigravityKV {
	t.Helper()
	store := newFakeAntigravityKV()
	SetKVStore(store)
	ResetKiroCapCache()
	t.Cleanup(func() {
		SetKVStore(nil)
		ResetKiroCapCache()
	})
	return store
}

// kiroReading builds one usage reading with a single bucket.
func kiroReading(used, total float64, resetAt time.Time) map[string]any {
	quota := map[string]any{"used": used, "total": total, "remaining": total - used, "unlimited": false}
	if !resetAt.IsZero() {
		quota["resetAt"] = resetAt.UTC().Format(time.RFC3339)
	} else {
		quota["resetAt"] = nil
	}
	return map[string]any{"agentic_request": quota}
}

func TestKiroCapSkipsExhaustedAccount(t *testing.T) {
	installKiroCapKV(t)
	base := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	useKiroClock(t, base)

	RecordKiroUsage("conn-a", kiroReading(100, 100, base.Add(5*24*time.Hour)))
	skip, until, reason := ShouldSkipKiroAccount("conn-a")
	if !skip {
		t.Fatal("an exhausted bucket must keep the account out of rotation")
	}
	if reason != "monthly cap reached" {
		t.Errorf("reason = %q, want %q", reason, "monthly cap reached")
	}
	if want := base.Add(5 * 24 * time.Hour); !until.Equal(want) {
		t.Errorf("until = %v, want %v", until, want)
	}

	RecordKiroUsage("conn-b", kiroReading(10, 100, base.Add(5*24*time.Hour)))
	if skip, _, _ := ShouldSkipKiroAccount("conn-b"); skip {
		t.Error("an account with quota left must stay eligible")
	}
}

func TestKiroCapSurvivesTheProbeTTL(t *testing.T) {
	installKiroCapKV(t)
	base := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	useKiroClock(t, base)

	MarkKiroCapped("conn-1", nil)

	// Six minutes later the reading is older than the 5-minute probe TTL, but the
	// cap window is still ahead: the cap is authoritative, the TTL is not.
	useKiroClock(t, base.Add(6*time.Minute))
	if skip, _, _ := ShouldSkipKiroAccount("conn-1"); !skip {
		t.Fatal("a cap inside its window must not expire on reading age")
	}

	// A week later, still inside the monthly window.
	useKiroClock(t, base.Add(7*24*time.Hour))
	if skip, _, _ := ShouldSkipKiroAccount("conn-1"); !skip {
		t.Fatal("a cap must hold until its reset, not until its reading ages out")
	}
}

func TestKiroCapStopsAtItsReset(t *testing.T) {
	installKiroCapKV(t)
	base := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	useKiroClock(t, base)

	past := base.Add(-time.Minute)
	MarkKiroCapped("conn-e", &past)
	if skip, _, _ := ShouldSkipKiroAccount("conn-e"); skip {
		t.Error("a cap whose reset has passed must not skip the account")
	}
	if _, ok := KiroCapSnapshot("conn-e"); ok {
		t.Error("the expired entry must be dropped, not kept as a stale opinion")
	}
}

func TestKiroCapCalendarFallbackIsNextUTCMonth(t *testing.T) {
	installKiroCapKV(t)
	base := time.Date(2026, 11, 15, 9, 30, 0, 0, time.UTC)
	useKiroClock(t, base)

	MarkKiroCapped("conn-d", nil)
	entry, ok := KiroCapSnapshot("conn-d")
	if !ok {
		t.Fatal("mark must store an entry")
	}
	got := time.UnixMilli(entry.CappedUntil).UTC()
	want := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("cappedUntil = %v, want the 1st of next month %v", got, want)
	}
	if entry.Source != "calendar" {
		t.Errorf("source = %q, want calendar", entry.Source)
	}
}

func TestKiroCapStaleReadingWithoutResetDoesNotBlock(t *testing.T) {
	installKiroCapKV(t)
	base := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	useKiroClock(t, base)

	// Unreadable window: the probe TTL is the only thing holding this reading.
	RecordKiroUsage("conn-f", kiroReading(10, 100, time.Time{}))
	useKiroClock(t, base.Add(10*time.Minute))
	if skip, _, _ := ShouldSkipKiroAccount("conn-f"); skip {
		t.Error("a reading with no future reset must not block an account forever")
	}

	// A stale reading that still names a future reset keeps holding.
	RecordKiroUsage("conn-f2", kiroReading(100, 100, base.Add(5*24*time.Hour)))
	useKiroClock(t, base.Add(10*time.Minute))
	if skip, _, _ := ShouldSkipKiroAccount("conn-f2"); !skip {
		t.Error("a stale reading whose reset is still ahead must keep the account out")
	}
}

func TestKiroCapHealthyReadingClearsTheCap(t *testing.T) {
	store := installKiroCapKV(t)
	base := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	useKiroClock(t, base)

	MarkKiroCapped("conn-1", nil)
	if _, ok := store.get(KiroCapScope, "conn-1"); !ok {
		t.Fatal("a cap must be persisted so a restart keeps it")
	}

	RecordKiroUsage("conn-1", kiroReading(1, 100, time.Time{}))
	if skip, _, _ := ShouldSkipKiroAccount("conn-1"); skip {
		t.Error("a healthy reading means the account is usable again")
	}
	if _, ok := store.get(KiroCapScope, "conn-1"); ok {
		t.Error("a healthy reading must remove the persisted row, not leave a stale cap for the next boot")
	}
}

func TestKiroCapRestoreDropsExpiredRows(t *testing.T) {
	store := installKiroCapKV(t)
	base := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	useKiroClock(t, base)

	MarkKiroCapped("conn-live", kiroAt(base.Add(48*time.Hour)))
	MarkKiroCapped("conn-dead", kiroAt(base.Add(-48*time.Hour)))
	ResetKiroCapCache()

	if restored := LoadPersistedKiroCaps(); restored != 1 {
		t.Fatalf("restored = %d, want 1 (the live cap only)", restored)
	}
	if skip, _, _ := ShouldSkipKiroAccount("conn-live"); !skip {
		t.Error("a restored cap must still skip the account")
	}
	if _, ok := store.get(KiroCapScope, "conn-dead"); ok {
		t.Error("a row whose reset already passed must be deleted on load")
	}
}

func TestKiroCapFailOpenOnKVError(t *testing.T) {
	store := installKiroCapKV(t)
	base := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	useKiroClock(t, base)
	store.err = errors.New("kv unavailable")

	// Neither a write, a read nor a delete may panic or block a request.
	MarkKiroCapped("conn-x", nil)
	RecordKiroUsage("conn-x", kiroReading(100, 100, base.Add(time.Hour)))
	ClearKiroCap("conn-x")
	if restored := LoadPersistedKiroCaps(); restored != 0 {
		t.Errorf("restored = %d, want 0 on a KV error", restored)
	}
	// The in-memory verdict still works: persistence is an optimisation, not the
	// mechanism.
	MarkKiroCapped("conn-y", kiroAt(base.Add(time.Hour)))
	if skip, _, _ := ShouldSkipKiroAccount("conn-y"); !skip {
		t.Error("the cap must hold in memory even when the KV store is down")
	}
}

func TestKiroCapClearForgetsOneConnection(t *testing.T) {
	installKiroCapKV(t)
	base := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	useKiroClock(t, base)

	MarkKiroCapped("conn-g", nil)
	MarkKiroCapped("conn-h", nil)
	ClearKiroCap("conn-g")
	if skip, _, _ := ShouldSkipKiroAccount("conn-g"); skip {
		t.Error("clear must forget the named connection")
	}
	if skip, _, _ := ShouldSkipKiroAccount("conn-h"); !skip {
		t.Error("clear must not touch another connection")
	}
}
