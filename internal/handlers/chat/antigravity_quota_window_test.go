// Antigravity quota-window parity — open-sse/config/quotaWindows.js
// (`antigravity: { scope: WEEKLY, usageKey: "gemini_weekly" }`,
// QUOTA_EXHAUSTED_SIGNALS) and open-sse/services/quotaWindow.js
// `resolveQuotaResetAt`.
//
// The failure these pin, seen in the field: the generation endpoint answers
//
//	429 {"error":{"code":429,"message":"Resource has been exhausted (e.g. check
//	     quota).","status":"RESOURCE_EXHAUSTED"}}
//
// and the account was locked for TWO SECONDS and re-dialled all week, because the
// quota evidence is the body's STATUS field with an underscore while the local
// pattern spelled it with a space.
package chat

import (
	"net/http"
	"testing"
	"time"

	"panrouter/proxy/internal/providers"
)

// antigravityResourceExhaustedBody is the exact body from that log line.
const antigravityResourceExhaustedBody = `{"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"RESOURCE_EXHAUSTED"}}`

func TestClassifyError_AntigravityResourceExhaustedIsQuota(t *testing.T) {
	cls := providers.ClassifyErrorForProvider(http.StatusTooManyRequests, antigravityResourceExhaustedBody, 0, "antigravity")
	if !cls.Quota {
		t.Fatalf("classified %+v, want a quota verdict (not a burst)", cls)
	}
	// A spent quota must not advance the exponential ladder: the account is empty,
	// not chatty.
	if cls.NewBackoffLevel != 0 {
		t.Errorf("backoff level = %d, want 0", cls.NewBackoffLevel)
	}

	// The other Google wordings the reference lists.
	for _, text := range []string{
		"Individual quota reached. Please upgrade your subscription to increase your limits. Resets in 74h19m34s.",
		"You exceeded your current quota, please check your plan and billing details.",
	} {
		if got := providers.ClassifyErrorForProvider(http.StatusForbidden, text, 0, "antigravity"); !got.Quota {
			t.Errorf("%q classified %+v, want quota", text, got)
		}
	}

	// A genuine burst is still a burst.
	plain := providers.ClassifyErrorForProvider(http.StatusTooManyRequests, `{"error":{"message":"Too many requests"}}`, 0, "antigravity")
	if plain.Quota {
		t.Errorf("a plain throttle classified as quota: %+v", plain)
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

	ue := &upstreamError{StatusCode: http.StatusTooManyRequests, Body: []byte(antigravityResourceExhaustedBody)}
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

func TestCooldownSecFor_AntigravityWithoutAReadingUsesTheCalendar(t *testing.T) {
	installAntigravityKV(t)
	base := time.Now().UTC()
	useAntigravityClock(t, base)

	ue := &upstreamError{StatusCode: http.StatusTooManyRequests, Body: []byte(antigravityResourceExhaustedBody)}
	errorText := extractErrorText(ue.Body)
	cls := providers.ClassifyErrorForProvider(ue.StatusCode, errorText, 0, "antigravity")

	handler := &ChatHandler{}
	got := handler.cooldownSecFor("ag-no-reading", "antigravity", ue, errorText, cls)
	want := int(nextWeeklyRollover(base).Sub(base).Seconds())
	if got < want || got > want+2 {
		t.Errorf("cooldown = %ds, want the next weekly boundary %ds", got, want)
	}
}
