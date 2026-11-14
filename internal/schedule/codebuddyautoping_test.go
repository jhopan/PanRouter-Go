package schedule

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"testing"
	"time"
)

// cbTestCycleStart is a fixed cycle start: 2026-03-09T00:00Z, i.e. 07:00 WIB on
// 2026-03-09. Every test derives its tick instants from a cycle start, so the
// clock is always an exact minute of the cycle and no test waits on the wall
// clock.
var cbTestCycleStart = time.Date(2026, 3, 9, 0, 0, 0, 0, time.UTC)

// recordedAttempt is one write the cycle made onto a connection row.
type recordedAttempt struct {
	id     string
	dayKey string
	model  string
	at     time.Time
}

// fakeStore is the connection store the tests drive. RecordAttempt also updates
// the row, because upstream's "already pinged today" reads the row back: without
// that the once-per-cycle contract would only be visible in the ping count.
type fakeStore struct {
	rows    []CodeBuddyConnection
	records []recordedAttempt
	listErr error
}

func (s *fakeStore) Connections() ([]CodeBuddyConnection, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return slices.Clone(s.rows), nil
}

func (s *fakeStore) RecordAttempt(id, dayKey string, at time.Time, model string) error {
	s.records = append(s.records, recordedAttempt{id: id, dayKey: dayKey, model: model, at: at})
	for i := range s.rows {
		if s.rows[i].ID == id {
			s.rows[i].LastAutoPingDayKey = dayKey
			s.rows[i].LastAutoPingAt = at
		}
	}
	return nil
}

// cycleHarness is one auto-ping cycle with every collaborator replaced: a fake
// clock, a fake store and a recording sender.
type cycleHarness struct {
	cycle    *CodeBuddyAutoPing
	store    *fakeStore
	enabled  bool
	model    string
	settings *CodeBuddySettings
	now      time.Time
	// sent is the model of every ping that left the harness, in order.
	sent   []string
	onSend func(conn CodeBuddyConnection, model string) error
}

func newCycleHarness(t *testing.T, conns ...CodeBuddyConnection) *cycleHarness {
	t.Helper()
	h := &cycleHarness{store: &fakeStore{rows: conns}, enabled: true}
	enabledConnections := map[string]bool{}
	for _, conn := range conns {
		enabledConnections[conn.ID] = true
	}
	h.cycle = NewCodeBuddyAutoPing()
	h.cycle.Now = func() time.Time { return h.now }
	h.cycle.Enabled = func() bool { return h.enabled }
	h.cycle.Settings = func() CodeBuddySettings {
		if h.settings != nil {
			return *h.settings
		}
		return CodeBuddySettings{Enabled: true, Connections: enabledConnections, Model: h.model}
	}
	h.cycle.Store = h.store
	// The harness starts with the settings already read once, which is the
	// state after the process's first tick: without this the very first tick
	// would look like a settings change and buy an immediate pass.
	h.cycle.settingsKey = h.cycle.Settings().key()
	h.cycle.Send = func(_ context.Context, conn CodeBuddyConnection, model string) error {
		h.sent = append(h.sent, model)
		if h.onSend != nil {
			return h.onSend(conn, model)
		}
		return nil
	}
	return h
}

func (h *cycleHarness) tick(t *testing.T) []error {
	t.Helper()
	return h.cycle.Tick(context.Background())
}

// cbDueTick is the instant a connection's slot arrives in the cycle that starts
// at start.
func cbDueTick(start time.Time, connectionID string) time.Time {
	dayKey := CodeBuddyDayKey(start)
	return start.Add(time.Duration(CodeBuddySlotMinute(connectionID, dayKey)) * time.Minute)
}

// cbFixture picks a (connection id, cycle) whose slot leaves room minutes of the
// cycle after it, so a test can place every tick of its scenario inside one
// cycle. It is deterministic: the hash and the ids are fixed, so the same pair
// is chosen on every run — the loop is there because the slot is a property of
// the hash, not something a test may assume.
func cbFixture(t *testing.T, room int) (string, time.Time) {
	t.Helper()
	for day := range 40 {
		start := cbTestCycleStart.AddDate(0, 0, day)
		dayKey := CodeBuddyDayKey(start)
		for i := range 40 {
			id := "cb-conn-" + strconv.Itoa(i)
			if slot := CodeBuddySlotMinute(id, dayKey); slot <= 1439-room {
				return id, start
			}
		}
	}
	t.Fatalf("no connection id leaves %d minutes of the cycle", room)
	return "", time.Time{}
}

// TestCodeBuddyAutoPingMasterSwitch pins the switch that has to be SET: off
// means zero pings even on a tick whose slot has arrived, and the per-connection
// map alone never starts the feature.
func TestCodeBuddyAutoPingMasterSwitch(t *testing.T) {
	tests := []struct {
		name     string
		enabled  bool
		settings CodeBuddySettings
		want     int
	}{
		{
			name:     "switch off does not ping a due connection",
			enabled:  false,
			settings: CodeBuddySettings{Enabled: true, Connections: map[string]bool{"cb-conn-1": true}},
			want:     0,
		},
		{
			name:     "switch on pings it",
			enabled:  true,
			settings: CodeBuddySettings{Enabled: true, Connections: map[string]bool{"cb-conn-1": true}},
			want:     1,
		},
		{
			name:     "a leftover connection switch does not start the feature",
			enabled:  true,
			settings: CodeBuddySettings{Enabled: false, Connections: map[string]bool{"cb-conn-1": true}},
			want:     0,
		},
		{
			name:     "a connection switched off is not pinged",
			enabled:  true,
			settings: CodeBuddySettings{Enabled: true, Connections: map[string]bool{}},
			want:     0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn := CodeBuddyConnection{ID: "cb-conn-1", DisplayName: "Account A"}
			h := newCycleHarness(t, conn)
			h.enabled = tt.enabled
			h.settings = &tt.settings
			h.now = cbDueTick(cbTestCycleStart, conn.ID)

			h.tick(t)
			if len(h.sent) != tt.want {
				t.Errorf("pings = %v, want %d", h.sent, tt.want)
			}
		})
	}
}

// TestCodeBuddyAutoPingEnabled_EnvSwitch pins the exact spelling of the switch:
// only "1" turns it on, and never by default.
func TestCodeBuddyAutoPingEnabled_EnvSwitch(t *testing.T) {
	tests := []struct {
		name     string
		bare     *string
		prefixed *string
		want     bool
	}{
		{name: "unset is off", want: false},
		{name: `"1" is on`, bare: new("1"), want: true},
		{name: `"0" is off`, bare: new("0"), want: false},
		{name: `"true" is off`, bare: new("true"), want: false},
		{name: "empty is off", bare: new(""), want: false},
		{name: "the prefixed spelling works", prefixed: new("1"), want: true},
		{name: "a prefixed 0 stays off", bare: new(""), prefixed: new("0"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Every spelling is set explicitly, so an operator's environment
			// cannot decide this test.
			for _, name := range []string{"CODEBUDDY_AUTOPING_ENABLED", "PANROUTER_CODEBUDDY_AUTOPING_ENABLED", "9ROUTER_CODEBUDDY_AUTOPING_ENABLED", "NINEROUTER_CODEBUDDY_AUTOPING_ENABLED"} {
				t.Setenv(name, "")
			}
			if tt.bare != nil {
				t.Setenv("CODEBUDDY_AUTOPING_ENABLED", *tt.bare)
			}
			if tt.prefixed != nil {
				t.Setenv("PANROUTER_CODEBUDDY_AUTOPING_ENABLED", *tt.prefixed)
			}
			if got := CodeBuddyAutoPingEnabled(); got != tt.want {
				t.Errorf("CodeBuddyAutoPingEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestCodeBuddyAutoPingFiresOncePerCycle pins one ping per connection per cycle,
// and that the next cycle pings again.
func TestCodeBuddyAutoPingFiresOncePerCycle(t *testing.T) {
	// The slot is bounded so the two cycles' ticks are more than the two-hour
	// touch throttle apart whatever the hash answers: upstream's throttle is
	// restart-safe across cycles, and this test is about the cycle, not it.
	connID, cycleStart := cbFixture(t, 300)
	conn := CodeBuddyConnection{ID: connID, DisplayName: "Account A"}
	h := newCycleHarness(t, conn)
	h.now = cbDueTick(cycleStart, conn.ID)

	if errs := h.tick(t); len(errs) != 0 {
		t.Fatalf("Tick returned %v, want no error", errs)
	}
	if len(h.sent) != 1 {
		t.Fatalf("pings = %v, want exactly one on the slot minute", h.sent)
	}

	// Later in the same cycle: the row written by the first ping is what makes
	// this a skip.
	h.now = h.now.Add(5 * time.Minute)
	h.tick(t)
	if len(h.sent) != 1 {
		t.Errorf("pings after the slot = %v, want still one", h.sent)
	}
	if len(h.store.records) != 1 {
		t.Errorf("recorded attempts = %d, want 1", len(h.store.records))
	}

	// A full cycle later the connection is due again.
	h.now = cbDueTick(cycleStart.AddDate(0, 0, 1), conn.ID)
	h.tick(t)
	if len(h.sent) != 2 {
		t.Fatalf("pings after the next cycle = %v, want 2", h.sent)
	}
	if got, want := h.store.records[1].dayKey, CodeBuddyDayKey(h.now); got != want {
		t.Errorf("recorded cycle = %q, want %q", got, want)
	}
}

// TestCodeBuddyAutoPingTwoTicksInOneMinute pins the slot dedupe: a tick that
// lands twice inside the same minute pings once.
func TestCodeBuddyAutoPingTwoTicksInOneMinute(t *testing.T) {
	conn := CodeBuddyConnection{ID: "cb-conn-2"}
	h := newCycleHarness(t, conn)
	h.now = cbDueTick(cbTestCycleStart, conn.ID)

	h.tick(t)
	h.tick(t)
	if len(h.sent) != 1 {
		t.Errorf("pings = %v, want exactly one for two ticks in one minute", h.sent)
	}
}

// cbFixtureSlotAtLeast picks a connection id whose slot in the first test cycle
// is at least min, so a test can place a tick before it. Deterministic for the
// same reason cbFixture is.
func cbFixtureSlotAtLeast(t *testing.T, min int) string {
	t.Helper()
	dayKey := CodeBuddyDayKey(cbTestCycleStart)
	for i := range 4000 {
		id := "cb-conn-slot-" + strconv.Itoa(i)
		if CodeBuddySlotMinute(id, dayKey) >= min {
			return id
		}
	}
	t.Fatalf("no connection id has a slot of at least %d", min)
	return ""
}

// TestCodeBuddyAutoPingWaitsForItsSlot pins the deterministic slot: a connection
// that has a record from an earlier cycle waits for its minute of the cycle, and
// pings once that minute arrives.
func TestCodeBuddyAutoPingWaitsForItsSlot(t *testing.T) {
	const minSlot = 180
	connID := cbFixtureSlotAtLeast(t, minSlot)
	slot := CodeBuddySlotMinute(connID, CodeBuddyDayKey(cbTestCycleStart))
	conn := CodeBuddyConnection{
		ID:                 connID,
		LastAutoPingDayKey: CodeBuddyDayKey(cbTestCycleStart.Add(-time.Minute)),
		// An hour before the cycle: this tick is refused by the slot, not by
		// the two-hour touch throttle.
		LastAutoPingAt: cbTestCycleStart.Add(-61 * time.Minute),
	}
	h := newCycleHarness(t, conn)
	// The harness starts like the process does, with the immediate first tick
	// upstream sets in startCodebuddyAutoPing. This test is about the slot
	// rule, which is the arm that applies to every tick after that one.
	h.cycle.immediate = false

	h.now = cbTestCycleStart.Add(time.Duration(slot-1) * time.Minute)
	h.tick(t)
	if len(h.sent) != 0 {
		t.Fatalf("pings one minute before the slot = %v, want none", h.sent)
	}

	h.now = cbTestCycleStart.Add(time.Duration(slot) * time.Minute)
	h.tick(t)
	if len(h.sent) != 1 {
		t.Errorf("pings at the slot minute = %v, want 1", h.sent)
	}
}

// TestCodeBuddyAutoPingImmediateFirstTick pins upstream's `immediateNextTick`:
// startCodebuddyAutoPing sets it, so the first tick after the process comes up
// fires a connection whose slot has not arrived yet instead of waiting hours
// for it. Exactly one tick gets that treatment.
func TestCodeBuddyAutoPingImmediateFirstTick(t *testing.T) {
	const minSlot = 300
	connID := cbFixtureSlotAtLeast(t, minSlot)
	slot := CodeBuddySlotMinute(connID, CodeBuddyDayKey(cbTestCycleStart))
	if slot < minSlot {
		t.Fatalf("fixture slot = %d, want at least %d", slot, minSlot)
	}
	conn := CodeBuddyConnection{
		ID:                 connID,
		LastAutoPingDayKey: CodeBuddyDayKey(cbTestCycleStart.Add(-24 * time.Hour)),
		// A day before the cycle: outside the touch throttle, which is what a
		// tick minutes into the cycle would otherwise trip.
		LastAutoPingAt: cbTestCycleStart.Add(-24 * time.Hour),
	}
	h := newCycleHarness(t, conn)

	h.now = cbTestCycleStart.Add(5 * time.Minute) // long before the slot
	h.tick(t)
	if len(h.sent) != 1 {
		t.Fatalf("first tick pings = %v, want exactly 1 (immediate arm)", h.sent)
	}

	// The flag is spent: the next tick goes back to the slot rule. The row was
	// not rewritten by the harness's store, so the connection still looks
	// unpasted; the touch throttle is what keeps it quiet here.
	h.now = cbTestCycleStart.Add(6 * time.Minute)
	h.tick(t)
	if len(h.sent) != 1 {
		t.Errorf("second tick pings = %v, want still 1", h.sent)
	}
}

// TestCodeBuddyAutoPingSettingsChangeBuysOneImmediateTick pins the ported
// configure arm: the dashboard saving settings is upstream's
// configureCodebuddyAutoPing call, which sets the immediate flag. One tick
// after the change fires early; the tick after that waits for slots again.
func TestCodeBuddyAutoPingSettingsChangeBuysOneImmediateTick(t *testing.T) {
	const minSlot = 300
	connID := cbFixtureSlotAtLeast(t, minSlot)
	conn := CodeBuddyConnection{
		ID:                 connID,
		LastAutoPingDayKey: CodeBuddyDayKey(cbTestCycleStart.Add(-24 * time.Hour)),
		LastAutoPingAt:     cbTestCycleStart.Add(-24 * time.Hour),
	}
	h := newCycleHarness(t, conn)
	h.cycle.immediate = false

	// The harness has already read these settings, so this tick is not a
	// change and the slot rule holds.
	h.now = cbTestCycleStart.Add(5 * time.Minute)
	h.tick(t)
	if len(h.sent) != 0 {
		t.Fatalf("tick before the slot = %v, want none", h.sent)
	}

	h.now = cbTestCycleStart.Add(6 * time.Minute)
	h.tick(t)
	if len(h.sent) != 0 {
		t.Errorf("second tick before the slot = %v, want none", h.sent)
	}

	// A saved change (the model override) is the configure call: the next tick
	// fires without waiting for the slot.
	h.settings = &CodeBuddySettings{Enabled: true, Model: "override-model", Connections: map[string]bool{connID: true}}
	h.now = cbTestCycleStart.Add(7 * time.Minute)
	h.tick(t)
	if len(h.sent) != 1 {
		t.Fatalf("tick after a settings change = %v, want 1", h.sent)
	}
	if got := h.sent[len(h.sent)-1]; got != "override-model" {
		t.Errorf("model after the change = %q, want override-model", got)
	}
}

// TestCodeBuddyAutoPingFiresANeverPingedConnection pins upstream's `isNew` arm:
// a connection with no attempt record at all has not had its check-in in any
// cycle, so the first tick that sees it fires it rather than waiting for a slot
// derived for a day it did not exist on.
func TestCodeBuddyAutoPingFiresANeverPingedConnection(t *testing.T) {
	connID := cbFixtureSlotAtLeast(t, 600)
	conn := CodeBuddyConnection{ID: connID}
	h := newCycleHarness(t, conn)
	h.now = cbTestCycleStart.Add(time.Minute)

	h.tick(t)
	if len(h.sent) != 1 {
		t.Errorf("pings for a connection with no record = %v, want 1", h.sent)
	}
	if recorded := h.store.records; len(recorded) != 1 || recorded[0].dayKey != CodeBuddyDayKey(h.now) {
		t.Errorf("recorded attempts = %+v, want one for this cycle", recorded)
	}
}

// TestCodeBuddyCycleBoundaryIsWIBMidnight pins the cycle keys against a host
// zone that is not WIB: 07:01 WIB starts a cycle, 06:59 WIB is still the
// previous one.
func TestCodeBuddyCycleBoundaryIsWIBMidnight(t *testing.T) {
	// The box runs in whatever zone the operator set; the cycle must not move
	// with it, so the whole table is read under a foreign TZ.
	t.Setenv("TZ", "America/New_York")

	tests := []struct {
		name       string
		at         time.Time
		wantDayKey string
		wantMinute int
	}{
		{
			name:       "07:00 WIB is the reset instant",
			at:         time.Date(2026, 3, 9, 0, 0, 0, 0, time.UTC),
			wantDayKey: "2026-03-09",
			wantMinute: 0,
		},
		{
			name:       "07:01 WIB belongs to the new cycle",
			at:         time.Date(2026, 3, 9, 0, 1, 0, 0, time.UTC),
			wantDayKey: "2026-03-09",
			wantMinute: 1,
		},
		{
			name:       "06:59 WIB still belongs to the previous cycle",
			at:         time.Date(2026, 3, 8, 23, 59, 0, 0, time.UTC),
			wantDayKey: "2026-03-08",
			wantMinute: 1439,
		},
		{
			name:       "06:58 WIB is the last slot of the previous cycle",
			at:         time.Date(2026, 3, 8, 23, 58, 0, 0, time.UTC),
			wantDayKey: "2026-03-08",
			wantMinute: 1438,
		},
		{
			name:       "an instant already in a WIB zone reads the same",
			at:         time.Date(2026, 3, 9, 7, 1, 0, 0, time.FixedZone("WIB", 7*3600)),
			wantDayKey: "2026-03-09",
			wantMinute: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CodeBuddyDayKey(tt.at); got != tt.wantDayKey {
				t.Errorf("CodeBuddyDayKey(%s) = %q, want %q", tt.at, got, tt.wantDayKey)
			}
			if got := CodeBuddyCycleMinute(tt.at); got != tt.wantMinute {
				t.Errorf("CodeBuddyCycleMinute(%s) = %d, want %d", tt.at, got, tt.wantMinute)
			}
		})
	}
}

// TestCodeBuddyCycleBoundaryAtPingLevel pins the boundary the way an operator
// sees it: a row pinged in the previous cycle is pinged by the first tick of the
// new one, and 06:59 WIB is still the previous cycle's business.
func TestCodeBuddyCycleBoundaryAtPingLevel(t *testing.T) {
	newCycle := cbTestCycleStart
	previousCycleKey := CodeBuddyDayKey(newCycle.Add(-2 * time.Hour))

	// The row says the previous cycle was pinged. Its timestamp falls in that
	// cycle and is two hours before the new one, so the restart-safe touch
	// throttle is not what refuses this tick.
	previous := CodeBuddyConnection{
		ID:                 "cb-conn-boundary",
		LastAutoPingDayKey: previousCycleKey,
		LastAutoPingAt:     newCycle.Add(-2 * time.Hour),
	}
	h := newCycleHarness(t, previous)
	h.now = cbDueTick(newCycle, previous.ID)
	h.tick(t)
	if len(h.sent) != 1 {
		t.Fatalf("pings in the new cycle = %v, want 1", h.sent)
	}
	if got := h.store.records[0].dayKey; got != CodeBuddyDayKey(newCycle) {
		t.Errorf("recorded cycle = %q, want %q", got, CodeBuddyDayKey(newCycle))
	}

	// 06:59 WIB is one minute before the boundary: it still belongs to the
	// cycle that started the day before.
	beforeBoundary := CodeBuddyConnection{ID: "cb-conn-before-boundary"}
	h = newCycleHarness(t, beforeBoundary)
	h.now = newCycle.Add(-time.Minute)
	slot := CodeBuddySlotMinute(beforeBoundary.ID, previousCycleKey)
	if CodeBuddyCycleMinute(h.now) < slot {
		t.Fatalf("fixture: slot %d is not due one minute before the boundary", slot)
	}
	h.tick(t)
	if len(h.sent) != 1 {
		t.Fatalf("pings at 06:59 WIB = %v, want 1", h.sent)
	}
	if got := h.store.records[0].dayKey; got != previousCycleKey {
		t.Errorf("recorded cycle at 06:59 WIB = %q, want %q", got, previousCycleKey)
	}
}

// TestCodeBuddyPingModelOverride pins the model a ping uses: the built-in cheap
// alias, the settings override, and the re-read that makes a saved override
// apply without a restart.
func TestCodeBuddyPingModelOverride(t *testing.T) {
	tests := []struct {
		name     string
		override string
		want     []string
	}{
		{name: "no override uses the cheap alias", want: []string{"fast-model"}},
		{name: "the settings override replaces it", override: "gpt-5.6-luna", want: []string{"gpt-5.6-luna"}},
		{name: "a blank override falls back", override: "   ", want: []string{"fast-model"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn := CodeBuddyConnection{ID: "cb-conn-model"}
			h := newCycleHarness(t, conn)
			h.model = tt.override
			h.now = cbDueTick(cbTestCycleStart, conn.ID)

			h.tick(t)
			if !slices.Equal(h.sent, tt.want) {
				t.Errorf("models sent = %v, want %v", h.sent, tt.want)
			}
			if len(h.store.records) != 1 || h.store.records[0].model != tt.want[0] {
				t.Errorf("recorded attempt = %+v, want the model %q", h.store.records, tt.want[0])
			}
		})
	}
}

// TestCodeBuddyPingModelIsRereadPerTick pins that the settings are read on every
// tick: an override saved in the dashboard applies to the next cycle without a
// restart.
func TestCodeBuddyPingModelIsRereadPerTick(t *testing.T) {
	connID, cycleStart := cbFixture(t, 300)
	conn := CodeBuddyConnection{ID: connID}
	h := newCycleHarness(t, conn)

	h.now = cbDueTick(cycleStart, conn.ID)
	h.tick(t)

	h.model = "default-model"
	h.now = cbDueTick(cycleStart.AddDate(0, 0, 1), conn.ID)
	h.tick(t)

	if want := []string{"fast-model", "default-model"}; !slices.Equal(h.sent, want) {
		t.Errorf("models sent = %v, want %v", h.sent, want)
	}
}

// TestCodeBuddyPingWalksOnlyOnAModelGatedRejection pins the candidate walk: a
// plan-gating rejection is the only reason to offer another model, and a
// credits-exhausted answer stops the walk immediately.
func TestCodeBuddyPingWalksOnlyOnAModelGatedRejection(t *testing.T) {
	gated := func(text string) error { return &CodeBuddyPingError{Status: 400, Text: text} }
	refuseAll := func(text string) func(CodeBuddyConnection, string) error {
		return func(CodeBuddyConnection, string) error { return gated(text) }
	}

	tests := []struct {
		name    string
		onSend  func(conn CodeBuddyConnection, model string) error
		want    []string
		wantErr bool
	}{
		{
			name:    "11158 walks the fallback chain",
			onSend:  refuseAll("11158: model_not_in_plan"),
			want:    []string{"fast-model", "default-model", "balanced-model"},
			wantErr: true,
		},
		{
			name:    "not allowed for user type walks the chain",
			onSend:  refuseAll("model fast-model is not allowed for user type personal_free_user"),
			want:    []string{"fast-model", "default-model", "balanced-model"},
			wantErr: true,
		},
		{
			name: "a credits-exhausted answer stops the walk",
			onSend: func(CodeBuddyConnection, string) error {
				return &CodeBuddyPingError{Status: 429, Text: "credits exhausted"}
			},
			want:    []string{"fast-model"},
			wantErr: false,
		},
		{
			name:    "an unclassified failure stops the walk",
			onSend:  func(CodeBuddyConnection, string) error { return errors.New("transport reset") },
			want:    []string{"fast-model"},
			wantErr: true,
		},
		{
			name: "the first candidate succeeding stops the walk",
			onSend: func(CodeBuddyConnection, string) error {
				return nil
			},
			want:    []string{"fast-model"},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn := CodeBuddyConnection{ID: "cb-conn-fallback"}
			h := newCycleHarness(t, conn)
			h.onSend = tt.onSend
			h.now = cbDueTick(cbTestCycleStart, conn.ID)

			errs := h.tick(t)
			if got := len(errs) > 0; got != tt.wantErr {
				t.Errorf("Tick error = %v, want an error: %v", errs, tt.wantErr)
			}
			if !slices.Equal(h.sent, tt.want) {
				t.Errorf("models sent = %v, want %v", h.sent, tt.want)
			}
		})
	}
}

// TestCodeBuddyAutoPingFailureCooldown pins the cooldown: a refused ping is not
// retried on the next tick, and it is retried once the cooldown has passed —
// upstream does not give the cycle up because one attempt failed.
func TestCodeBuddyAutoPingFailureCooldown(t *testing.T) {
	// Room for the cooldown plus a tick, so every tick below stays inside one
	// cycle and this pins the cooldown rather than the cycle boundary.
	connID, cycleStart := cbFixture(t, 70)
	conn := CodeBuddyConnection{ID: connID, DisplayName: "Account A"}
	h := newCycleHarness(t, conn)
	h.onSend = func(CodeBuddyConnection, string) error { return errors.New("upstream refused") }
	h.now = cbDueTick(cycleStart, conn.ID)

	if errs := h.tick(t); len(errs) != 1 {
		t.Fatalf("a failed ping must be reported: %v", errs)
	}
	if len(h.sent) != 1 {
		t.Fatalf("pings = %v, want 1", h.sent)
	}
	if len(h.store.records) != 0 {
		t.Errorf("a failed ping must not be recorded as an attempt: %+v", h.store.records)
	}

	// Ten minutes later the connection is due but in its cooldown.
	h.now = h.now.Add(10 * time.Minute)
	h.tick(t)
	if len(h.sent) != 1 {
		t.Fatalf("pings inside the cooldown = %v, want 1", h.sent)
	}

	// Past the cooldown the connection is offered again.
	h.onSend = nil
	h.now = h.now.Add(CodeBuddyFailureCooldown)
	if errs := h.tick(t); len(errs) != 0 {
		t.Fatalf("Tick after the cooldown returned %v, want no error", errs)
	}
	if len(h.sent) != 2 {
		t.Errorf("pings after the cooldown = %v, want a retry", h.sent)
	}
	if len(h.store.records) != 1 {
		t.Errorf("recorded attempts = %d, want 1", len(h.store.records))
	}
}

// TestCodeBuddyAutoPingSkipsInsideTheTouchThrottle pins the restart-safe minimum
// gap: a connection touched less than two hours ago is left alone even though
// its slot has arrived.
func TestCodeBuddyAutoPingSkipsInsideTheTouchThrottle(t *testing.T) {
	// The slot is bounded to the first 90 minutes of the cycle, so the row's
	// timestamp can sit in the previous cycle (making "already pinged today"
	// false) and still be inside the two-hour throttle.
	connID, cycleStart := cbFixture(t, 1350)
	conn := CodeBuddyConnection{
		ID:                 connID,
		LastAutoPingDayKey: CodeBuddyDayKey(cycleStart.Add(-time.Minute)),
		LastAutoPingAt:     cycleStart.Add(-30 * time.Minute),
	}
	h := newCycleHarness(t, conn)
	h.now = cbDueTick(cycleStart, conn.ID)

	h.tick(t)
	if len(h.sent) != 0 {
		t.Errorf("pings = %v, want none inside the throttle", h.sent)
	}

	// Two hours later, past the throttle and still the previous cycle's record
	// (the row is only rewritten by a ping): now it pings.
	h.now = h.now.Add(CodeBuddyTouchThrottle)
	h.tick(t)
	if len(h.sent) != 1 {
		t.Errorf("pings after the throttle = %v, want 1", h.sent)
	}
}

// TestCodeBuddySlotMinuteIsDeterministic pins the slot: stable for a given
// (connection, cycle) across callers and runs, inside the window upstream
// spreads it over, and different cycles are free to land elsewhere.
func TestCodeBuddySlotMinuteIsDeterministic(t *testing.T) {
	const connID = "cb-conn-slot"
	dayKey := CodeBuddyDayKey(cbTestCycleStart)

	slot := CodeBuddySlotMinute(connID, dayKey)
	if slot < CodeBuddySlotStartMinute || slot > CodeBuddySlotEndMinute {
		t.Fatalf("slot = %d, want within [%d, %d]", slot, CodeBuddySlotStartMinute, CodeBuddySlotEndMinute)
	}
	if again := CodeBuddySlotMinute(connID, dayKey); again != slot {
		t.Errorf("slot changed between calls: %d then %d", slot, again)
	}
	if other := CodeBuddySlotMinute("cb-conn-other", dayKey); other < CodeBuddySlotStartMinute || other > CodeBuddySlotEndMinute {
		t.Errorf("another connection's slot = %d, out of window", other)
	}
}

// TestCodeBuddyPingedToday pins the two records upstream reads: the cycle key it
// wrote, and the timestamp an older row carries.
func TestCodeBuddyPingedToday(t *testing.T) {
	dayKey := "2026-03-09"
	tests := []struct {
		name string
		conn CodeBuddyConnection
		want bool
	}{
		{name: "no record", conn: CodeBuddyConnection{}, want: false},
		{name: "the cycle key matches", conn: CodeBuddyConnection{LastAutoPingDayKey: dayKey}, want: true},
		{
			name: "the timestamp falls in this cycle",
			conn: CodeBuddyConnection{LastAutoPingAt: time.Date(2026, 3, 9, 5, 0, 0, 0, time.UTC)},
			want: true,
		},
		{
			name: "the timestamp falls in the previous cycle",
			conn: CodeBuddyConnection{LastAutoPingAt: time.Date(2026, 3, 8, 23, 0, 0, 0, time.UTC)},
			want: false,
		},
		{name: "another cycle's key", conn: CodeBuddyConnection{LastAutoPingDayKey: "2026-03-08"}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CodeBuddyPingedToday(tt.conn, dayKey); got != tt.want {
				t.Errorf("CodeBuddyPingedToday(%+v) = %v, want %v", tt.conn, got, tt.want)
			}
		})
	}
}

// TestCodeBuddyPingBody pins the request the ping sends: the cheapest model
// asked for one word, in a shape the CodeBuddy executor accepts.
func TestCodeBuddyPingBody(t *testing.T) {
	body, err := CodeBuddyPingBody("fast-model")
	if err != nil {
		t.Fatalf("CodeBuddyPingBody: %v", err)
	}
	var decoded struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
		Stream    bool   `json:"stream"`
		Messages  []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("ping body is not JSON: %v (%s)", err, body)
	}
	if decoded.Model != "fast-model" {
		t.Errorf("model = %q, want fast-model", decoded.Model)
	}
	if decoded.MaxTokens != CodeBuddyPingMaxTokens {
		t.Errorf("max_tokens = %d, want %d", decoded.MaxTokens, CodeBuddyPingMaxTokens)
	}
	if decoded.Stream {
		t.Errorf("stream = true, want a non-stream ping body the executor rewrites")
	}
	if len(decoded.Messages) != 1 || decoded.Messages[0].Role != "user" || decoded.Messages[0].Content != PingText {
		t.Errorf("messages = %+v, want one %q user turn", decoded.Messages, PingText)
	}
}

// TestCodeBuddySettingsParsing pins the settings shape and its failure
// direction: a value the parser does not understand leaves the feature off.
func TestCodeBuddySettingsParsing(t *testing.T) {
	tests := []struct {
		name string
		raw  any
		want CodeBuddySettings
	}{
		{
			name: "the canonical shape",
			raw: map[string]any{
				"enabled":     true,
				"model":       "gpt-5.6-luna",
				"connections": map[string]any{"cb-conn-1": true, "cb-conn-2": false},
			},
			want: CodeBuddySettings{
				Enabled:     true,
				Model:       "gpt-5.6-luna",
				Connections: map[string]bool{"cb-conn-1": true, "cb-conn-2": false},
			},
		},
		{name: "a missing value", raw: nil, want: CodeBuddySettings{Connections: map[string]bool{}}},
		{name: "a wrong type", raw: "on", want: CodeBuddySettings{Connections: map[string]bool{}}},
		{
			name: "a non-boolean connection entry is dropped",
			raw:  map[string]any{"enabled": true, "connections": map[string]any{"cb-conn-1": "on"}},
			want: CodeBuddySettings{Enabled: true, Connections: map[string]bool{}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseCodeBuddySettings(tt.raw)
			if got.Enabled != tt.want.Enabled || got.Model != tt.want.Model || !mapsEqual(got.Connections, tt.want.Connections) {
				t.Errorf("ParseCodeBuddySettings(%v) = %+v, want %+v", tt.raw, got, tt.want)
			}
		})
	}
}

// TestCodeBuddySettingsFromBlob pins the settings key: the dashboard stores the
// cycle's configuration under `codebuddyIntlAutoPing`, and a missing key leaves
// the feature off.
func TestCodeBuddySettingsFromBlob(t *testing.T) {
	blob := map[string]any{
		"codebuddyIntlAutoPing": map[string]any{
			"enabled":     true,
			"connections": map[string]any{"cb-conn-1": true},
		},
	}
	if got := CodeBuddySettingsFromBlob(blob); !got.Enabled || !got.Connections["cb-conn-1"] {
		t.Errorf("CodeBuddySettingsFromBlob = %+v, want the stored configuration", got)
	}
	if got := CodeBuddySettingsFromBlob(nil); got.Enabled {
		t.Errorf("CodeBuddySettingsFromBlob(nil) = %+v, want disabled", got)
	}
	if got := CodeBuddySettingsFromBlob(map[string]any{}); got.Enabled {
		t.Errorf("a blob without the key = %+v, want disabled", got)
	}
}

// TestCodeBuddyAutoPingListFailureIsNotFatal pins the failure direction of a
// connection read: the tick reports nothing and pings nothing rather than
// panicking, and it recovers on the next tick.
func TestCodeBuddyAutoPingListFailureIsNotFatal(t *testing.T) {
	conn := CodeBuddyConnection{ID: "cb-conn-list"}
	h := newCycleHarness(t, conn)
	h.store.listErr = errors.New("database is locked")
	h.now = cbDueTick(cbTestCycleStart, conn.ID)

	if errs := h.tick(t); len(errs) != 0 {
		t.Errorf("Tick returned %v, want no error", errs)
	}
	if len(h.sent) != 0 {
		t.Errorf("pings = %v, want none while the rows cannot be read", h.sent)
	}

	h.store.listErr = nil
	h.tick(t)
	if len(h.sent) != 1 {
		t.Errorf("pings after the read recovers = %v, want 1", h.sent)
	}
}

// TestCodeBuddyAutoPingNilCollaborators keeps a bare cycle from panicking: the
// wiring is what supplies the store and the sender.
func TestCodeBuddyAutoPingNilCollaborators(t *testing.T) {
	cycle := NewCodeBuddyAutoPing()
	cycle.Enabled = func() bool { return true }
	cycle.Settings = func() CodeBuddySettings {
		return CodeBuddySettings{Enabled: true, Connections: map[string]bool{"cb-conn-1": true}}
	}
	cycle.Now = func() time.Time { return cbDueTick(cbTestCycleStart, "cb-conn-1") }

	if errs := cycle.Tick(context.Background()); len(errs) != 0 {
		t.Errorf("Tick with no store returned %v, want no error", errs)
	}
}

func mapsEqual(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
