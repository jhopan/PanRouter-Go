package chat

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	json "panrouter/proxy/internal/fastjson"
)

// fakeAntigravityKV is the injected KV store: an in-memory scope map plus a
// switch that makes every call fail, so the fail-open paths are exercised
// without a database.
type fakeAntigravityKV struct {
	mu   sync.Mutex
	rows map[string]map[string]string
	err  error
}

func newFakeAntigravityKV() *fakeAntigravityKV {
	return &fakeAntigravityKV{rows: make(map[string]map[string]string)}
}

func (f *fakeAntigravityKV) GetKVScope(scope string) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	out := make(map[string]string, len(f.rows[scope]))
	for k, v := range f.rows[scope] {
		out[k] = v
	}
	return out, nil
}

func (f *fakeAntigravityKV) SetKV(scope, key, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	if f.rows[scope] == nil {
		f.rows[scope] = make(map[string]string)
	}
	f.rows[scope][key] = value
	return nil
}

func (f *fakeAntigravityKV) DeleteKV(scope, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	delete(f.rows[scope], key)
	return nil
}

func (f *fakeAntigravityKV) get(scope, key string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.rows[scope][key]
	return v, ok
}

func (f *fakeAntigravityKV) set(scope, key, value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rows[scope] == nil {
		f.rows[scope] = make(map[string]string)
	}
	f.rows[scope][key] = value
}

// installAntigravityKV wires the fake for one test and clears both caches so no
// sibling test's state leaks in.
func installAntigravityKV(t *testing.T) *fakeAntigravityKV {
	t.Helper()
	store := newFakeAntigravityKV()
	SetKVStore(store)
	ClearAntigravityIneligibleCache()
	ClearAntigravityQuotaCache()
	ClearAntigravityRevalidateState()
	t.Cleanup(func() {
		SetKVStore(nil)
		ClearAntigravityIneligibleCache()
		ClearAntigravityQuotaCache()
		ClearAntigravityRevalidateState()
	})
	return store
}

// useAntigravityClock points the injected clock at a value the test can move.
func useAntigravityClock(t *testing.T, at time.Time) *time.Time {
	t.Helper()
	now := at
	old := agNow
	agNow = func() time.Time { return now }
	t.Cleanup(func() { agNow = old })
	return &now
}

func TestParseAntigravityEligibility_LoadCodeAssist(t *testing.T) {
	tests := []struct {
		name    string
		payload map[string]any
		want    *AntigravityEligibility
	}{
		{
			name: "ineligible tier carries the verification gate",
			payload: map[string]any{"ineligibleTiers": []any{map[string]any{
				"tierId":        "free-tier",
				"reasonCode":    "VALIDATION_REQUIRED",
				"validationUrl": "https://accounts.google.com/v3/signin/continue?authuser=1",
				"reasonMessage": "Your account is not eligible for Antigravity",
			}}},
			want: &AntigravityEligibility{
				ReasonCode:    "VALIDATION_REQUIRED",
				ReasonMessage: "Your account is not eligible for Antigravity",
				ValidationURL: "https://accounts.google.com/v3/signin/continue?authuser=1",
				TierID:        "free-tier",
			},
		},
		{
			name: "prefers the tier that names a reason",
			payload: map[string]any{"ineligibleTiers": []any{
				map[string]any{"tierId": "first"},
				map[string]any{"tierId": "second", "reasonCode": "VALIDATION_REQUIRED"},
			}},
			want: &AntigravityEligibility{ReasonCode: "VALIDATION_REQUIRED", TierID: "second"},
		},
		{
			name:    "allowed tier only means eligible",
			payload: map[string]any{"allowedTiers": []any{map[string]any{"tierId": "free-tier"}}},
			want:    &AntigravityEligibility{Eligible: true},
		},
		{
			name:    "no eligibility fields means no opinion",
			payload: map[string]any{"cloudaicompanionProject": "proj-1"},
		},
		{name: "nil payload", payload: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseAntigravityEligibility(tt.payload)
			if (got == nil) != (tt.want == nil) {
				t.Fatalf("verdict = %+v, want %+v", got, tt.want)
			}
			if got == nil {
				return
			}
			if *got != *tt.want {
				t.Errorf("verdict = %+v, want %+v", *got, *tt.want)
			}
		})
	}
}

func TestParseAntigravityQuotaEligibility_403(t *testing.T) {
	tests := []struct {
		name    string
		payload map[string]any
		want    *AntigravityEligibility
	}{
		{
			name: "ErrorInfo detail carries reason and metadata",
			payload: map[string]any{"error": map[string]any{
				"code":    403,
				"status":  "PERMISSION_DENIED",
				"message": "Verify your account to continue.",
				"details": []any{map[string]any{
					"reason":   "VALIDATION_REQUIRED",
					"metadata": map[string]any{"validation_url": "https://accounts.google.com/x"},
				}},
			}},
			want: &AntigravityEligibility{
				ReasonCode:    "VALIDATION_REQUIRED",
				ReasonMessage: "Verify your account to continue.",
				ValidationURL: "https://accounts.google.com/x",
			},
		},
		{
			name: "Help detail alone is enough",
			payload: map[string]any{"error": map[string]any{
				"details": []any{map[string]any{
					"links": []any{map[string]any{"url": "https://accounts.google.com/help"}},
				}},
			}},
			want: &AntigravityEligibility{ValidationURL: "https://accounts.google.com/help"},
		},
		{
			name:    "plain 403 has no eligibility",
			payload: map[string]any{"error": map[string]any{"code": 403, "message": "forbidden"}},
		},
		{name: "no error object", payload: map[string]any{"code": 403}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseAntigravityQuotaEligibility(tt.payload)
			if (got == nil) != (tt.want == nil) {
				t.Fatalf("verdict = %+v, want %+v", got, tt.want)
			}
			if got != nil && *got != *tt.want {
				t.Errorf("verdict = %+v, want %+v", *got, *tt.want)
			}
		})
	}
}

func TestAntigravityVerdictFromErrorBody(t *testing.T) {
	// The quota details name the gate but carry no link; the ban detector's
	// appeal extraction is what supplies the one link Google wants opened.
	body := []byte(`{"error":{"details":[{"reason":"VALIDATION_REQUIRED","metadata":{"appeal_url":"https://forms.gle/appeal"}}]}}`)
	verdict := AntigravityVerdictFromErrorBody(body)
	if verdict == nil {
		t.Fatal("expected a verdict")
	}
	if verdict.ReasonCode != "VALIDATION_REQUIRED" {
		t.Errorf("reasonCode = %q", verdict.ReasonCode)
	}
	if verdict.ValidationURL != "https://forms.gle/appeal" {
		t.Errorf("validationUrl = %q, want the appeal link", verdict.ValidationURL)
	}

	// A hostile link is dropped rather than carried.
	hostile := []byte(`{"error":{"details":[{"reason":"VALIDATION_REQUIRED","metadata":{"appeal_url":"https://evil.example/x"}}]}}`)
	if v := AntigravityVerdictFromErrorBody(hostile); v == nil || v.ValidationURL != "" {
		t.Errorf("hostile link must not survive: %+v", v)
	}

	if v := AntigravityVerdictFromErrorBody([]byte(`not json`)); v != nil {
		t.Errorf("malformed body must yield no verdict, got %+v", v)
	}
	if v := AntigravityVerdictFromErrorBody([]byte(`{"error":{"code":403}}`)); v != nil {
		t.Errorf("plain 403 must yield no verdict, got %+v", v)
	}
}

func TestAntigravityIneligibleTTL(t *testing.T) {
	store := installAntigravityKV(t)
	now := useAntigravityClock(t, time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC))

	if !RecordAntigravityEligibility("ag-ttl", &AntigravityEligibility{ReasonCode: "VALIDATION_REQUIRED"}) {
		t.Fatal("expected the account to be flagged")
	}
	if _, ok := store.get(agIneligibleScope, "ag-ttl"); !ok {
		t.Fatal("expected the flag to be persisted")
	}
	if !ShouldSkipAntigravityAccount("ag-ttl") {
		t.Fatal("a fresh flag must skip the account")
	}

	*now = now.Add(AgIneligibleTTL - time.Minute)
	if !ShouldSkipAntigravityAccount("ag-ttl") {
		t.Error("a flag inside the 15m window must still skip the account")
	}

	*now = now.Add(2 * time.Minute) // 15m + 1m
	if ShouldSkipAntigravityAccount("ag-ttl") {
		t.Error("an expired flag must not skip the account")
	}
}

func TestAntigravityIneligibleRestoreDropsExpiredRows(t *testing.T) {
	store := installAntigravityKV(t)
	base := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	useAntigravityClock(t, base)

	stale := agIneligibleRow{Flagged: true, FlaggedAt: base.Add(-AgIneligibleTTL - time.Minute).UnixMilli(), ReasonCode: "VALIDATION_REQUIRED"}
	fresh := agIneligibleRow{Flagged: true, FlaggedAt: base.Add(-time.Minute).UnixMilli(), ReasonCode: "VALIDATION_REQUIRED"}
	writeAntigravityRow(t, store, "ag-stale", stale)
	writeAntigravityRow(t, store, "ag-fresh", fresh)

	if kept := RestoreAntigravityIneligible(); kept != 1 {
		t.Fatalf("restored = %d, want 1", kept)
	}
	if got := GetAntigravityIneligibility("ag-fresh"); got == nil || got.ReasonCode != "VALIDATION_REQUIRED" {
		t.Errorf("fresh flag not restored: %+v", got)
	}
	if ShouldSkipAntigravityAccount("ag-stale") {
		t.Error("an expired row must not block the account")
	}
	if _, ok := store.get(agIneligibleScope, "ag-stale"); ok {
		t.Error("the expired row should have been deleted, not just ignored")
	}
}

func TestAntigravityIneligibleHealthyVerdictClearsPersistedRowAfterRestart(t *testing.T) {
	store := installAntigravityKV(t)
	useAntigravityClock(t, time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC))

	if !RecordAntigravityEligibility("ag-restart", &AntigravityEligibility{ReasonCode: "VALIDATION_REQUIRED", ValidationURL: "https://accounts.google.com/keep-out-of-db"}) {
		t.Fatal("expected the account to be flagged")
	}
	raw, ok := store.get(agIneligibleScope, "ag-restart")
	if !ok {
		t.Fatal("expected the flag to be persisted")
	}
	// The verification link carries a continuation token: the row must not hold
	// it (providers/antigravity_ban.go), only the reason and the stamp.
	if strings.Contains(raw, "accounts.google.com") {
		t.Errorf("persisted row leaks the verification URL: %s", raw)
	}

	// Simulated restart: the in-memory map is empty, the row is not.
	ClearAntigravityIneligibleCache()
	if ShouldSkipAntigravityAccount("ag-restart") {
		t.Fatal("precondition: a restart empties the in-memory map")
	}

	// A healthy reading must remove the row even though the map held nothing —
	// the bug that kept a verified account flagged until its TTL ran out.
	if RecordAntigravityEligibility("ag-restart", &AntigravityEligibility{Eligible: true}) {
		t.Fatal("a healthy verdict must not flag the account")
	}
	if _, ok := store.get(agIneligibleScope, "ag-restart"); ok {
		t.Error("the persisted row survived a healthy verdict")
	}
	if kept := RestoreAntigravityIneligible(); kept != 0 {
		t.Errorf("restored = %d, want 0 after the clear", kept)
	}
}

func TestAntigravityIneligibleFailOpenOnKVError(t *testing.T) {
	store := installAntigravityKV(t)
	useAntigravityClock(t, time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC))
	store.err = errors.New("kv unavailable")

	if !RecordAntigravityEligibility("ag-broken", &AntigravityEligibility{ReasonCode: "VALIDATION_REQUIRED"}) {
		t.Fatal("a KV failure must not stop the in-memory flag")
	}
	if !ShouldSkipAntigravityAccount("ag-broken") {
		t.Error("the account must still be skipped while the store is down")
	}
	if kept := RestoreAntigravityIneligible(); kept != 0 {
		t.Errorf("restore = %d, want 0 on a KV error", kept)
	}
	persistAntigravityExhausted("ag-broken", map[string]AntigravityModelQuota{
		"gemini_weekly": {ResetAt: time.Now().Add(time.Hour)},
	})
	if loaded := LoadPersistedAntigravityQuota(); loaded != 0 {
		t.Errorf("load = %d, want 0 on a KV error", loaded)
	}

	store.err = nil
	if RecordAntigravityEligibility("ag-broken", nil) {
		t.Error("a no-opinion verdict must clear the flag")
	}
	if ShouldSkipAntigravityAccount("ag-broken") {
		t.Error("the flag must be gone after the clear")
	}
}

func TestAntigravityIneligibleCacheStaysBounded(t *testing.T) {
	installAntigravityKV(t)
	useAntigravityClock(t, time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC))

	agIneligibleMu.Lock()
	for i := range agIneligibleMaxEntries {
		agIneligibleCache[string(rune('a'+i%26))+string(rune('a'+i/26))] = AntigravityIneligibleEntry{
			FlaggedAt: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
		}
	}
	agIneligibleMu.Unlock()

	// Every entry is expired, so the prune empties the map without dropping a
	// live flag.
	RecordAntigravityEligibility("ag-live", &AntigravityEligibility{ReasonCode: "VALIDATION_REQUIRED"})

	agIneligibleMu.RLock()
	size := len(agIneligibleCache)
	agIneligibleMu.RUnlock()
	if size > agIneligibleMaxEntries {
		t.Errorf("cache grew to %d entries, cap is %d", size, agIneligibleMaxEntries)
	}
	if !ShouldSkipAntigravityAccount("ag-live") {
		t.Error("the live flag must survive pruning")
	}
}

func writeAntigravityRow(t *testing.T, store *fakeAntigravityKV, connectionID string, row agIneligibleRow) {
	t.Helper()
	raw, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("marshal row: %v", err)
	}
	store.set(agIneligibleScope, connectionID, string(raw))
}
