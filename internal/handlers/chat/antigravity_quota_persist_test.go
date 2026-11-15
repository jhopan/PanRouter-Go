package chat

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// seedAntigravityQuotas installs a cached quota map for one connection.
func seedAntigravityQuotas(connectionID string, quotas map[string]AntigravityModelQuota) {
	agQuotaMu.Lock()
	defer agQuotaMu.Unlock()
	agQuotaCache[connectionID] = quotas
}

func TestAntigravityQuotaPersistRoundTrip(t *testing.T) {
	store := installAntigravityKV(t)
	base := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	useAntigravityClock(t, base)

	const connID = "ag-persist"
	persistAntigravityExhausted(connID, map[string]AntigravityModelQuota{
		"claude_gpt_weekly": {RemainingPercentage: 0, ResetAt: base.Add(6 * time.Hour)},
		// Healthy reading: kept in memory only.
		"gemini_weekly": {RemainingPercentage: 99.55, ResetAt: base.Add(6 * time.Hour)},
		// 0% whose reset already passed is a stale reading, not a block.
		"gemini_session": {RemainingPercentage: 0, ResetAt: base.Add(-time.Minute)},
		// 0% with no reset at all cannot be a block either.
		"gemini-3.8-flash-high": {RemainingPercentage: 0},
	})

	raw, ok := store.get(AgQuotaScope, connID)
	if !ok {
		t.Fatal("expected the exhausted bucket to be persisted")
	}
	if !rowContainsAll(raw, "claude_gpt_weekly") {
		t.Errorf("row is missing the exhausted bucket: %s", raw)
	}
	for _, unwanted := range []string{"gemini_weekly", "gemini_session", "gemini-3.8-flash-high"} {
		if rowContainsAll(raw, unwanted) {
			t.Errorf("row must not carry %q: %s", unwanted, raw)
		}
	}

	// Simulated restart: memory is gone, the row is not.
	ClearAntigravityQuotaCache()
	if restored := LoadPersistedAntigravityQuota(); restored != 1 {
		t.Fatalf("restored = %d, want 1", restored)
	}
	restoredQuota, ok := antigravityQuotaCacheSnapshot(connID)["claude_gpt_weekly"]
	if !ok {
		t.Fatal("the exhausted bucket was not restored")
	}
	if restoredQuota.RemainingPercentage != 0 || !restoredQuota.ResetAt.Equal(base.Add(6*time.Hour)) {
		t.Errorf("restored bucket = %+v", restoredQuota)
	}

	// A healthy reading clears the row, so the next boot does not come back
	// blocked.
	removeAntigravityPersistedIfHealthy(connID, map[string]AntigravityModelQuota{
		"claude_gpt_weekly": {RemainingPercentage: 100, ResetAt: base.Add(6 * time.Hour)},
	})
	if _, ok := store.get(AgQuotaScope, connID); ok {
		t.Error("the row survived a healthy reading")
	}
}

func TestAntigravityQuotaPersistDropsFullyExpiredRow(t *testing.T) {
	store := installAntigravityKV(t)
	base := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	useAntigravityClock(t, base)

	store.set(AgQuotaScope, "ag-expired", `{"gemini_weekly":{"remainingPercentage":0,"resetAt":"2026-11-14T00:00:00Z"}}`)
	store.set(AgQuotaScope, "ag-live", `{"gemini_weekly":{"remainingPercentage":0,"resetAt":"2026-11-16T00:00:00Z"}}`)

	if restored := LoadPersistedAntigravityQuota(); restored != 1 {
		t.Fatalf("restored = %d, want 1", restored)
	}
	if _, ok := store.get(AgQuotaScope, "ag-expired"); ok {
		t.Error("an all-expired row should be deleted at boot, not reloaded forever")
	}
	if cached := antigravityQuotaCacheSnapshot("ag-expired"); len(cached) > 0 {
		t.Errorf("an expired row must not populate the cache: %v", cached)
	}
	if _, ok := antigravityQuotaCacheSnapshot("ag-live")["gemini_weekly"]; !ok {
		t.Error("the live row must be restored")
	}
}

func TestAntigravityQuotaPersistFailOpen(t *testing.T) {
	store := installAntigravityKV(t)
	store.err = errors.New("kv down")
	useAntigravityClock(t, time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC))

	// Neither a write, a read nor a delete may panic or block a request.
	persistAntigravityExhausted("ag-x", map[string]AntigravityModelQuota{
		"gemini_weekly": {RemainingPercentage: 0, ResetAt: time.Now().Add(time.Hour)},
	})
	removeAntigravityPersistedIfHealthy("ag-x", nil)
	if restored := LoadPersistedAntigravityQuota(); restored != 0 {
		t.Errorf("restored = %d, want 0", restored)
	}
}

func TestSelectAntigravitySkipsToRevalidate(t *testing.T) {
	base := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	future := base.Add(6 * time.Hour)
	past := base.Add(-time.Hour)

	tests := []struct {
		name     string
		model    string
		buckets  map[string]AntigravityModelQuota
		token    string
		lastSeen time.Time
		want     bool
	}{
		{
			name:    "future-and-zero bucket is due for verification",
			model:   "gemini-3.8-flash-high",
			buckets: map[string]AntigravityModelQuota{"gemini_weekly": {RemainingPercentage: 0, ResetAt: future}},
			token:   "tok",
			want:    true,
		},
		{
			name:    "bucket that still has quota is not verified",
			model:   "gemini-3.8-flash-high",
			buckets: map[string]AntigravityModelQuota{"gemini_weekly": {RemainingPercentage: 99.55, ResetAt: future}},
			token:   "tok",
		},
		{
			name:    "bucket whose reset passed is not verified",
			model:   "gemini-3.8-flash-high",
			buckets: map[string]AntigravityModelQuota{"gemini_weekly": {RemainingPercentage: 0, ResetAt: past}},
			token:   "tok",
		},
		{
			name:  "no cached entry",
			model: "gemini-3.8-flash-high",
			token: "tok",
		},
		{
			name:    "no access token",
			model:   "gemini-3.8-flash-high",
			buckets: map[string]AntigravityModelQuota{"gemini_weekly": {RemainingPercentage: 0, ResetAt: future}},
		},
		{
			name:     "inside the 5m cooldown",
			model:    "gemini-3.8-flash-high",
			buckets:  map[string]AntigravityModelQuota{"gemini_weekly": {RemainingPercentage: 0, ResetAt: future}},
			token:    "tok",
			lastSeen: base.Add(-time.Minute),
		},
		{
			name:     "past the 5m cooldown",
			model:    "gemini-3.8-flash-high",
			buckets:  map[string]AntigravityModelQuota{"gemini_weekly": {RemainingPercentage: 0, ResetAt: future}},
			token:    "tok",
			lastSeen: base.Add(-AgRevalidateCooldown - time.Second),
			want:     true,
		},
		{
			name:    "model outside the known families",
			model:   "xmodel-1",
			buckets: map[string]AntigravityModelQuota{"gemini_weekly": {RemainingPercentage: 0, ResetAt: future}},
			token:   "tok",
		},
		{
			name:  "argon bucket decides: exhausted argon with healthy gemini still verifies",
			model: "gemini-4-argon-high",
			buckets: map[string]AntigravityModelQuota{
				"gemini_weekly":       {RemainingPercentage: 100, ResetAt: future},
				"gemini_argon_weekly": {RemainingPercentage: 0, ResetAt: future},
			},
			token: "tok",
			want:  true,
		},
		{
			name:  "argon bucket decides: healthy argon with exhausted gemini is not skipped",
			model: "gemini-4-argon-high",
			buckets: map[string]AntigravityModelQuota{
				"gemini_weekly":       {RemainingPercentage: 0, ResetAt: future},
				"gemini_argon_weekly": {RemainingPercentage: 100, ResetAt: future},
			},
			token: "tok",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ClearAntigravityQuotaCache()
			ClearAntigravityRevalidateState()
			if tt.buckets != nil {
				seedAntigravityQuotas("ag-a", tt.buckets)
			}
			if !tt.lastSeen.IsZero() {
				agRevalidateMu.Lock()
				agLastRevalidateAt["ag-a"] = tt.lastSeen
				agRevalidateMu.Unlock()
			}

			got := SelectAntigravitySkipsToRevalidate([]AntigravityRevalidateCandidate{
				{ConnectionID: "ag-a", AccessToken: tt.token},
			}, tt.model, base)
			if (len(got) == 1) != tt.want {
				t.Fatalf("selection = %v, want due=%v", got, tt.want)
			}
		})
	}
}

func TestRevalidateAntigravityExhaustedStampsBeforeAwait(t *testing.T) {
	installAntigravityKV(t)
	ClearAntigravityRevalidateState()
	base := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	useAntigravityClock(t, base)
	seedAntigravityQuotas("ag-a", map[string]AntigravityModelQuota{
		"gemini_weekly": {RemainingPercentage: 0, ResetAt: base.Add(6 * time.Hour)},
	})

	var calls int32
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	refresh := func(ctx context.Context, connectionID, accessToken, projectID string) (map[string]AntigravityModelQuota, error) {
		atomic.AddInt32(&calls, 1)
		once.Do(func() { close(entered) })
		<-release
		return nil, nil
	}
	candidates := []AntigravityRevalidateCandidate{{ConnectionID: "ag-a", AccessToken: "tok", ProjectID: "proj"}}

	done := make(chan struct{})
	go func() {
		RevalidateAntigravityExhausted(context.Background(), refresh, candidates, "gemini-3.8-flash-high", base)
		close(done)
	}()
	<-entered // the first call is now inside the quota read

	// A second request for the same account must not start a second quota call:
	// the timestamp is recorded before the await.
	RevalidateAntigravityExhausted(context.Background(), refresh, candidates, "gemini-3.8-flash-high", base)
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("concurrent callers made %d quota calls, want 1", got)
	}

	close(release)
	<-done

	// Inside the cooldown the account is not re-read again...
	RevalidateAntigravityExhausted(context.Background(), refresh, candidates, "gemini-3.8-flash-high", base.Add(30*time.Second))
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("call inside the cooldown made %d quota calls, want 1", got)
	}
	// ...but past it, it is.
	seedAntigravityQuotas("ag-a", map[string]AntigravityModelQuota{
		"gemini_weekly": {RemainingPercentage: 0, ResetAt: base.Add(6 * time.Hour)},
	})
	RevalidateAntigravityExhausted(context.Background(), refresh, candidates, "gemini-3.8-flash-high", base.Add(AgRevalidateCooldown+time.Second))
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("call past the cooldown made %d quota calls, want 2", got)
	}
}

func TestRevalidateAntigravityExhaustedFailOpen(t *testing.T) {
	installAntigravityKV(t)
	base := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	seedAntigravityQuotas("ag-a", map[string]AntigravityModelQuota{
		"gemini_weekly": {RemainingPercentage: 0, ResetAt: base.Add(6 * time.Hour)},
	})
	candidates := []AntigravityRevalidateCandidate{{ConnectionID: "ag-a", AccessToken: "tok"}}

	// A throwing quota read must neither panic nor surface an error.
	refresh := func(context.Context, string, string, string) (map[string]AntigravityModelQuota, error) {
		return nil, errors.New("quota api down")
	}
	RevalidateAntigravityExhausted(context.Background(), refresh, candidates, "gemini-3.8-flash-high", base)
	if _, ok := antigravityQuotaCacheSnapshot("ag-a")["gemini_weekly"]; !ok {
		t.Error("a failed re-verification must leave the cached reading untouched")
	}
}

func TestAntigravityFamilyKey(t *testing.T) {
	tests := map[string]string{
		"gemini-3.8-flash-high": "gemini_weekly",
		"gemini-4-argon":        "gemini_weekly",
		"gemini-4-carbon-high":  "gemini_weekly",
		"palm-2":                "gemini_weekly",
		"gemma-3-27b":           "gemini_weekly",
		"imagen-4":              "gemini_weekly",
		"claude-sonnet-5-5":     "claude_gpt_weekly",
		"claude-opus-5-5-high":  "claude_gpt_weekly",
		"gpt-oss-120b-medium":   "claude_gpt_weekly",
		"opus-4":                "claude_gpt_weekly",
		"sonnet-4":              "claude_gpt_weekly",
		"xmodel-1":              "",
		"":                      "",
	}
	for model, want := range tests {
		if got := AntigravityFamilyKey(model); got != want {
			t.Errorf("AntigravityFamilyKey(%q) = %q, want %q", model, got, want)
		}
	}
}

func TestAntigravityArgonBucket(t *testing.T) {
	base := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	// The fixture's "reset in the future" only means anything against a pinned
	// clock: IsAntigravityModelBlocked compares resetAt with agNow(), so on a real
	// clock this test expired six hours after it was written and reported that a
	// spent Argon bucket no longer blocked anything.
	useAntigravityClock(t, base)
	future := base.Add(6 * time.Hour)
	quotas := map[string]AntigravityModelQuota{
		"gemini_argon_weekly": {RemainingPercentage: 0, ResetAt: future},
		"gemini_weekly":       {RemainingPercentage: 100, ResetAt: future},
	}

	bucket, ok := AntigravityArgonBucket(quotas, "gemini-4-argon-high")
	if !ok || bucket.RemainingPercentage != 0 {
		t.Fatalf("argon bucket = %+v ok=%v", bucket, ok)
	}
	if _, ok := AntigravityArgonBucket(quotas, "gemini-3.8-flash-high"); ok {
		t.Error("a non-Argon model must not read the Argon bucket")
	}
	if _, ok := AntigravityArgonBucket(map[string]AntigravityModelQuota{"gemini_weekly": {RemainingPercentage: 40}}, "gemini-4-argon"); ok {
		t.Error("no reported Argon bucket must yield the family fallback, not a bucket")
	}
	if _, ok := AntigravityArgonBucket(nil, "gemini-4-argon"); ok {
		t.Error("nil quotas must yield no bucket")
	}

	// Exhausted Argon is not rescued by healthy plain-Gemini quota.
	seedAntigravityQuotas("ag-argon", quotas)
	if !IsAntigravityModelBlocked("ag-argon", "gemini-4-argon-high") {
		t.Error("an exhausted Argon bucket must block the Argon tier despite healthy gemini_weekly")
	}
	if IsAntigravityModelBlocked("ag-argon", "gemini-3.8-flash-high") {
		t.Error("healthy plain-Gemini quota must stay usable")
	}

	// The preference cuts both ways: a healthy Argon bucket decides, so a plain
	// Gemini 0% must not block an Argon tier the account still serves.
	seedAntigravityQuotas("ag-argon-2", map[string]AntigravityModelQuota{
		"gemini_argon_weekly": {RemainingPercentage: 100, ResetAt: future},
		"gemini_weekly":       {RemainingPercentage: 0, ResetAt: future},
	})
	if IsAntigravityModelBlocked("ag-argon-2", "gemini-4-argon-high") {
		t.Error("the Argon bucket decides: healthy Argon must not be blocked by gemini_weekly")
	}
	if !IsAntigravityModelBlocked("ag-argon-2", "gemini-3.8-flash-high") {
		t.Error("plain Gemini is still decided by gemini_weekly")
	}

	decision, ok := antigravityDecideQuota(map[string]AntigravityModelQuota{
		"gemini_argon_weekly": {RemainingPercentage: 0, ResetAt: future},
		"gemini_weekly":       {RemainingPercentage: 100, ResetAt: future},
		"gemini-4-argon-high": {RemainingPercentage: 100, ResetAt: future},
	}, "gemini-4-argon-high")
	if !ok || decision.CacheKey != AgArgonBucketKey || decision.Quota.RemainingPercentage != 0 {
		t.Errorf("argon must decide over the model row: %+v ok=%v", decision, ok)
	}

	// Ultra plan: no Argon bucket reported, so the family bucket decides.
	decision, ok = antigravityDecideQuota(map[string]AntigravityModelQuota{
		"gemini_weekly": {RemainingPercentage: 0, ResetAt: future},
	}, "gemini-4-argon")
	if !ok || decision.CacheKey != "gemini_weekly" {
		t.Errorf("fallback decision = %+v ok=%v", decision, ok)
	}
}

func rowContainsAll(haystack string, needles ...string) bool {
	for _, needle := range needles {
		if !strings.Contains(haystack, needle) {
			return false
		}
	}
	return true
}
