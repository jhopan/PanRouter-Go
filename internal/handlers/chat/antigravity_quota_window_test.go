// Antigravity quota-window parity — a real spent quota carries authority:
// ErrorInfo QUOTA_EXHAUSTED plus reset text/timestamp or RetryInfo. The
// detail-free RESOURCE_EXHAUSTED body is deliberately NOT evidence; the same
// account/model can answer 200 for a minimal Hub request and 429 for a long
// agent payload, so it is pinned separately as a short model refusal.
package chat

import (
	"net/http"
	"testing"
	"time"

	"panrouter/proxy/internal/providers"
)

// antigravityResourceExhaustedBody is the exact body from that log line.
const antigravityResourceExhaustedBody = `{"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"RESOURCE_EXHAUSTED"}}`

func TestClassifyError_AntigravityAuthoritativeQuotaStaysQuota(t *testing.T) {
	// The detail-free body is a request fingerprint refusal, not an allowance.
	cls := providers.ClassifyErrorForProvider(http.StatusTooManyRequests, antigravityResourceExhaustedBody, 0, "antigravity")
	if cls.Quota || cls.NewBackoffLevel != 1 || cls.CooldownMs != 2000 {
		t.Fatalf("detail-free classification = %+v, want 2s transient", cls)
	}

	// These Google wordings actually name a spent quota/reset and remain quota.
	for _, text := range []string{
		"Individual quota reached. Please upgrade your subscription to increase your limits. Resets in 74h19m34s.",
		"You exceeded your current quota, please check your plan and billing details.",
	} {
		if got := providers.ClassifyErrorForProvider(http.StatusForbidden, text, 0, "antigravity"); !got.Quota {
			t.Errorf("%q classified %+v, want quota", text, got)
		}
	}

	if got := providers.ClassifyErrorForProvider(http.StatusTooManyRequests, `{"error":{"message":"Too many requests"}}`, 0, "antigravity"); got.Quota {
		t.Errorf("a plain throttle classified as quota: %+v", got)
	}
}

func TestAntigravityQuotaResetPrefersTheReading(t *testing.T) {
	installAntigravityKV(t)
	base := time.Date(2026, 11, 17, 6, 0, 0, 0, time.UTC)
	useAntigravityClock(t, base)

	// No reading: the calendar decides — next Monday 00:00 UTC.
	if got, want := antigravityQuotaResetFor("ag-quota", base), nextWeeklyRollover(base); !got.Equal(want) {
		t.Errorf("reset with no reading = %v, want the weekly boundary %v", got, want)
	}
	if got := antigravityQuotaResetFor("ag-quota", base).Weekday(); got != time.Monday {
		t.Errorf("calendar fallback lands on %v, want Monday", got)
	}

	// A reading names the moment, and that wins over the calendar.
	reading := base.Add(74*time.Hour + 19*time.Minute + 34*time.Second)
	seedAntigravityQuotas("ag-quota", map[string]AntigravityModelQuota{
		"gemini_weekly": {RemainingPercentage: 0, ResetAt: reading},
	})
	if got := antigravityQuotaResetFor("ag-quota", base); !got.Equal(reading) {
		t.Errorf("reset with a reading = %v, want the bucket's %v", got, reading)
	}

	// Only the weekly bucket decides: another family's reset must not unlock an
	// exhausted one (upstream pins usageKey for exactly this reason).
	seedAntigravityQuotas("ag-quota-2", map[string]AntigravityModelQuota{
		"claude_gpt_weekly": {RemainingPercentage: 0, ResetAt: base.Add(2 * time.Hour)},
	})
	if got, want := antigravityQuotaResetFor("ag-quota-2", base), nextWeeklyRollover(base); !got.Equal(want) {
		t.Errorf("reset from a foreign bucket = %v, want the weekly fallback %v", got, want)
	}
}

func TestCooldownSecFor_AntigravityParksUntilTheWeeklyReset(t *testing.T) {
	installAntigravityKV(t)
	base := time.Now().UTC()
	useAntigravityClock(t, base)

	reading := base.Add(74*time.Hour + 19*time.Minute)
	seedAntigravityQuotas("ag-park", map[string]AntigravityModelQuota{
		"gemini_weekly": {RemainingPercentage: 0, ResetAt: reading},
	})

	ue := &upstreamError{StatusCode: http.StatusTooManyRequests, Body: []byte(antigravityExhaustedBody(reading, "74h19m0s"))}
	errorText := extractErrorText(ue.Body)
	cls := providers.ClassifyErrorForProvider(ue.StatusCode, errorText, 0, "antigravity")

	handler := &ChatHandler{}
	got := handler.cooldownSecFor("ag-park", "antigravity", ue, errorText, cls)
	want := int(reading.Sub(base).Seconds())
	if diff := got - want; diff > 2 || diff < -2 {
		t.Errorf("cooldown = %ds, want the bucket's %ds (about 74h)", got, want)
	}
	if got < 3600 {
		t.Errorf("cooldown = %ds, want hours-to-days rather than the 2s burst base", got)
	}
	// Bounded by the provider's own ceiling.
	if got > int(maxCooldownFor("antigravity").Seconds()) {
		t.Errorf("cooldown = %ds exceeds the weekly ceiling %v", got, maxCooldownFor("antigravity"))
	}
}

// A detail-free RESOURCE_EXHAUSTED with quota still on the bucket is the lane's
// refusal, not a spent budget: measured live on 2026-11-18, the same account
// answered that body for one model while its own reading said 99% remained, and
// the account was parked for the whole week. Holding the ordinary cooldown keeps
// the account rotating, which is what lets a second account serve the request.
func TestCooldownSecFor_AntigravityHealthyBucketIsNotParkedForTheWeek(t *testing.T) {
	installAntigravityKV(t)
	base := time.Now().UTC()
	useAntigravityClock(t, base)

	seedAntigravityQuotas("ag-healthy", map[string]AntigravityModelQuota{
		"gemini_weekly": {RemainingPercentage: 99, ResetAt: base.Add(6 * 24 * time.Hour)},
	})

	ue := &upstreamError{StatusCode: http.StatusTooManyRequests, Body: []byte(antigravityResourceExhaustedBody)}
	errorText := extractErrorText(ue.Body)
	cls := providers.ClassifyErrorForProvider(ue.StatusCode, errorText, 0, "antigravity")

	got := (&ChatHandler{}).cooldownSecFor("ag-healthy", "antigravity", ue, errorText, cls)
	if got <= 0 {
		t.Fatalf("cooldown = %ds, want the ordinary quota cooldown", got)
	}
	if weekly := int(nextWeeklyRollover(base).Sub(base).Seconds()); got >= weekly {
		t.Errorf("cooldown = %ds, want less than the weekly window %ds while the bucket has quota", got, weekly)
	}
	if got != int(cls.CooldownMs/1000) {
		t.Errorf("cooldown = %ds, want the classifier's %ds", got, cls.CooldownMs/1000)
	}
}

// A body that NAMES the reset instant is the provider describing its own window,
// so it still parks even when the local reading disagrees — the body is newer
// than the reading, and the reading is what is stale.
func TestCooldownSecFor_AntigravityNamedResetStillParks(t *testing.T) {
	installAntigravityKV(t)
	base := time.Now().UTC()
	useAntigravityClock(t, base)

	seedAntigravityQuotas("ag-named", map[string]AntigravityModelQuota{
		"gemini_weekly": {RemainingPercentage: 99, ResetAt: base.Add(6 * 24 * time.Hour)},
	})

	resetAt := base.Add(30 * time.Hour).Truncate(time.Second)
	ue := &upstreamError{StatusCode: http.StatusTooManyRequests, Body: []byte(antigravityResetOnlyBody(http.StatusTooManyRequests, resetAt))}
	errorText := extractErrorText(ue.Body)
	cls := providers.ClassifyErrorForProvider(ue.StatusCode, errorText, 0, "antigravity")

	got := (&ChatHandler{}).cooldownSecFor("ag-named", "antigravity", ue, errorText, cls)
	if want := int(resetAt.Sub(base).Seconds()); got < want-2 || got > want+2 {
		t.Errorf("cooldown = %ds, want the named reset's %ds", got, want)
	}
}

// A detail-free 429 with no reading on the account names no window at all, so
// there is nothing to park for. The ordinary cooldown keeps the account in
// rotation; parking it until the next weekly boundary on no evidence is exactly
// the 4.5-day park measured on 2026-11-18 (cooldown_s=393774), where the account
// answered the same model normally in its own client moments later.
func TestCooldownSecFor_AntigravityWithoutAReadingHoldsTheOrdinaryCooldown(t *testing.T) {
	installAntigravityKV(t)
	base := time.Now().UTC()
	useAntigravityClock(t, base)

	ue := &upstreamError{StatusCode: http.StatusTooManyRequests, Body: []byte(antigravityResourceExhaustedBody)}
	errorText := extractErrorText(ue.Body)
	cls := providers.ClassifyErrorForProvider(ue.StatusCode, errorText, 0, "antigravity")

	handler := &ChatHandler{}
	got := handler.cooldownSecFor("ag-no-reading", "antigravity", ue, errorText, cls)
	if want := int(cls.CooldownMs / 1000); got != want {
		t.Errorf("cooldown = %ds, want the classifier's %ds", got, want)
	}
	if weekly := int(nextWeeklyRollover(base).Sub(base).Seconds()); got >= weekly {
		t.Errorf("cooldown = %ds, want less than the weekly window %ds with no reading to justify it", got, weekly)
	}
}

// A stale reading — one whose reset already passed — is not a block either: it
// describes an account that is usable again, so it must not license a park.
func TestCooldownSecFor_AntigravityStaleReadingIsNotAPark(t *testing.T) {
	installAntigravityKV(t)
	base := time.Now().UTC()
	useAntigravityClock(t, base)

	seedAntigravityQuotas("ag-stale", map[string]AntigravityModelQuota{
		"gemini_weekly": {RemainingPercentage: 0, ResetAt: base.Add(-time.Minute)},
	})

	ue := &upstreamError{StatusCode: http.StatusTooManyRequests, Body: []byte(antigravityResourceExhaustedBody)}
	errorText := extractErrorText(ue.Body)
	cls := providers.ClassifyErrorForProvider(ue.StatusCode, errorText, 0, "antigravity")

	got := (&ChatHandler{}).cooldownSecFor("ag-stale", "antigravity", ue, errorText, cls)
	if want := int(cls.CooldownMs / 1000); got != want {
		t.Errorf("cooldown = %ds, want the classifier's %ds for a lapsed reading", got, want)
	}
}

// The exact detail-free fingerprint refusal never gains a weekly cooldown from
// a cached 0% row. The row may be stale or belong to a prior request; the body
// names no provider reset, while a minimal request for the same account/model
// can succeed. It must remain a short model-only backoff.
func TestCooldownSecFor_AntigravityDetailFreeRefusalIgnoresExhaustedCache(t *testing.T) {
	installAntigravityKV(t)
	base := time.Now().UTC()
	useAntigravityClock(t, base)
	seedAntigravityQuotas("ag-fingerprint-cache", map[string]AntigravityModelQuota{
		"gemini_weekly": {RemainingPercentage: 0, ResetAt: base.Add(6 * 24 * time.Hour)},
	})

	ue := &upstreamError{StatusCode: http.StatusTooManyRequests, Body: []byte(antigravityResourceExhaustedBody)}
	errorText := extractErrorText(ue.Body)
	cls := providers.ClassifyErrorForProvider(ue.StatusCode, errorText, 0, "antigravity")
	got := (&ChatHandler{}).cooldownSecFor("ag-fingerprint-cache", "antigravity", ue, errorText, cls)
	if got != 2 {
		t.Errorf("cooldown = %ds, want the 2s fingerprint backoff rather than a weekly lock", got)
	}
}
