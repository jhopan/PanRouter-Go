package chat

import (
	"net/http"
	"testing"
	"time"

	"panrouter/proxy/internal/providers"
)

// tokenharborFreeAllowanceBody is the body the Token Harbor free tier answers
// once its rolling allowance is spent. Upstream reads the same sentence
// (src/sse/services/auth.js:20-60 tokenharborResetMs) to park the account at
// the instant the provider names instead of guessing one.
const tokenharborFreeAllowanceBody = "You've used this period's free allowance. " +
	"Your next rolling 7-day period starts on 2 Oct 2026 at 06:22 UTC."

// tokenharborResetInstant is the instant that sentence names.
var tokenharborResetInstant = time.Date(2026, 10, 2, 6, 22, 0, 0, time.UTC)

// TestTokenharborResetAt_ExactInstant pins the parse: an instant, not a
// duration, and one the host's zone cannot move. `now` is handed over in a
// non-UTC zone on purpose — the body names UTC, so that is what must come back
// whatever the gateway's clock is set to.
func TestTokenharborResetAt_ExactInstant(t *testing.T) {
	now := time.Date(2026, 9, 30, 23, 5, 0, 0, time.FixedZone("WIB", 7*60*60))
	tests := []struct {
		name     string
		provider string
		body     string
	}{
		{"plain body", "tokenharbor", tokenharborFreeAllowanceBody},
		{"json 429 envelope", "tokenharbor", `{"error":{"message":"` + tokenharborFreeAllowanceBody + `","code":429}}`},
		{"alias th", "th", tokenharborFreeAllowanceBody},
		{"alias thh", "thh", tokenharborFreeAllowanceBody},
		{"lowercase month", "tokenharbor", "You've used this period's free allowance. Your next rolling 7-day period starts on 2 oct 2026 at 06:22 UTC."},
		{"unpadded hour", "tokenharbor", "You've used this period's free allowance. Your next rolling 7-day period starts on 2 Oct 2026 at 6:22 UTC."},
		{"bare 'starts on' clause", "tokenharbor", "You've used this period's free allowance. The new window starts on 2 Oct 2026 at 06:22 UTC."},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := tokenharborResetAt(http.StatusTooManyRequests, tt.provider, []byte(tt.body), now)
			if !ok {
				t.Fatalf("no reset parsed out of %q", tt.body)
			}
			if !got.Equal(tokenharborResetInstant) {
				t.Errorf("reset = %v, want %v", got, tokenharborResetInstant)
			}
			if got.Location() != time.UTC {
				t.Errorf("reset zone = %v, want UTC", got.Location())
			}
		})
	}
}

// TestTokenharborResetAt_WordingMustMatch pins the refusal half: a body whose
// wording does not carry the sentence shape yields no reset (the weekly
// fallback applies instead) rather than a guessed date.
func TestTokenharborResetAt_WordingMustMatch(t *testing.T) {
	now := tokenharborResetInstant.Add(-72 * time.Hour)
	tests := []struct {
		name string
		body string
	}{
		{"no reset sentence at all", "You've used this period's free allowance. Try again later."},
		{"full month name", "You've used this period's free allowance. Your next rolling 7-day period starts on 2 October 2026 at 06:22 UTC."},
		{"no UTC suffix", "You've used this period's free allowance. Your next rolling 7-day period starts on 2 Oct 2026 at 06:22."},
		{"another lane's date shape", "You've used this period's free allowance. Quota resets at 2026-10-02 06:22:00."},
		{"allowance marker absent", "Your next rolling 7-day period starts on 2 Oct 2026 at 06:22 UTC."},
		{"impossible day of month", "You've used this period's free allowance. Your next rolling 7-day period starts on 31 Feb 2026 at 06:22 UTC."},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if reset, ok := tokenharborResetAt(http.StatusTooManyRequests, "tokenharbor", []byte(tt.body), now); ok {
				t.Errorf("reset = %v from %q, want no parse", reset, tt.body)
			}
		})
	}
}

// TestTokenharborResetAt_ProviderAndStatusGating pins the scope: only the
// tokenharbor lane, only a 429. Another provider's body carrying the same
// sentence is not evidence of Token Harbor's window, and only a 429 carries the
// allowance refusal.
func TestTokenharborResetAt_ProviderAndStatusGating(t *testing.T) {
	now := tokenharborResetInstant.Add(-time.Hour)
	body := []byte(tokenharborFreeAllowanceBody)

	for _, provider := range []string{"openai", "codebuddy-intl", "cbai", "deepseek-web", "gemini", ""} {
		if reset, ok := tokenharborResetAt(http.StatusTooManyRequests, provider, body, now); ok {
			t.Errorf("provider %q parsed %v, want nothing", provider, reset)
		}
	}
	for _, status := range []int{
		http.StatusBadRequest,
		http.StatusPaymentRequired,
		http.StatusForbidden,
		http.StatusInternalServerError,
		http.StatusServiceUnavailable,
	} {
		if reset, ok := tokenharborResetAt(status, "tokenharbor", body, now); ok {
			t.Errorf("status %d parsed %v, want nothing", status, reset)
		}
	}
	if _, ok := tokenharborResetAt(http.StatusTooManyRequests, "tokenharbor", body, now); !ok {
		t.Error("a tokenharbor 429 must still parse; the gate is rejecting everything")
	}
}

// TestTokenharborResetAt_PastInstantParksNothing pins upstream's `ms > Date.now()`
// guard: a named instant already gone is no future window, so the parse
// contributes no park (the calendar fallback decides instead) rather than
// clamping a dead date to a token cooldown.
func TestTokenharborResetAt_PastInstantParksNothing(t *testing.T) {
	body := "You've used this period's free allowance. Your next rolling 7-day period starts on 1 Jan 2020 at 06:22 UTC."
	if reset, ok := tokenharborResetAt(http.StatusTooManyRequests, "tokenharbor", []byte(body), tokenharborResetInstant); ok {
		t.Errorf("reset = %v, ok = true, want no reset from an instant already past", reset)
	}
}

// TestNextWeeklyRollover pins the calendar fallback: the coming Monday at
// 00:00 UTC, or the Monday after when now IS a Monday (upstream
// calendarResetMs(QUOTA_SCOPES.WEEKLY), weeklyResetWeekday = 1).
func TestNextWeeklyRollover(t *testing.T) {
	tests := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{"midweek", time.Date(2026, 9, 29, 6, 22, 0, 0, time.UTC), time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)},
		{"sunday", time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC), time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)},
		{"monday midnight rolls a whole week", time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := nextWeeklyRollover(tt.now); !got.Equal(tt.want) {
				t.Errorf("nextWeeklyRollover(%v) = %v, want %v", tt.now, got, tt.want)
			}
		})
	}
}

// TestTokenharborPark_InstantBeatsFixedCooldown ports the precedence upstream
// gives the parsed instant (src/sse/services/auth.js:408-441): the
// precise-reset branch decides before the rule table is consulted, and the
// value it produces is bounded by the provider's own quota window. The clock is
// injected so each case reads the same calendar.
func TestTokenharborPark_InstantBeatsFixedCooldown(t *testing.T) {
	now := time.Date(2026, 9, 29, 6, 22, 0, 0, time.UTC) // Tuesday
	body := []byte(tokenharborFreeAllowanceBody)
	ue := &upstreamError{StatusCode: http.StatusTooManyRequests, Body: body}

	// The classifier still recognises the free-allowance 429 and still returns
	// its flat quota cooldown: the row this change must not disturb.
	cls := providers.ClassifyErrorForProvider(http.StatusTooManyRequests, string(body), 0, "tokenharbor")
	if !cls.ShouldFallback || cls.CooldownMs != providers.QuotaExhaustedCooldownMs {
		t.Fatalf("classifier = %+v, want the free-allowance quota row (%dms)", cls, providers.QuotaExhaustedCooldownMs)
	}
	base := time.Duration(cls.CooldownMs) * time.Millisecond

	wantInstant := int(tokenharborResetInstant.Sub(now) / time.Second) // 3 days
	if got := retryableCooldownSecAt(http.StatusTooManyRequests, base, ue, "tokenharbor", now); got != wantInstant {
		t.Errorf("cooldown = %ds, want the instant the body named (%ds), not the fixed %dms", got, wantInstant, cls.CooldownMs)
	}

	// An instant beyond the weekly window is capped at it, not honoured.
	far := "You've used this period's free allowance. Your next rolling 7-day period starts on 2 Dec 2026 at 06:22 UTC."
	ueFar := &upstreamError{StatusCode: http.StatusTooManyRequests, Body: []byte(far)}
	if got := retryableCooldownSecAt(http.StatusTooManyRequests, base, ueFar, "tokenharbor", now); got != int(maxWeeklyResetCooldown/time.Second) {
		t.Errorf("cooldown = %ds, want the %v weekly ceiling", got, maxWeeklyResetCooldown)
	}

	// No parseable reset: the weekly rollover parks the account instead.
	wantRollover := int(nextWeeklyRollover(now).Sub(now) / time.Second)
	if wantRollover != 495480 {
		t.Fatalf("fallback from %v = %ds, want 495480s (Mon 2026-10-05 00:00 UTC)", now, wantRollover)
	}
	for _, tt := range []struct{ name, body string }{
		{"body shape changed", "You've used this period's free allowance. Your next rolling period starts soon."},
		{"instant already past", "You've used this period's free allowance. Your next rolling 7-day period starts on 1 Jan 2020 at 06:22 UTC."},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ue := &upstreamError{StatusCode: http.StatusTooManyRequests, Body: []byte(tt.body)}
			if got := retryableCooldownSecAt(http.StatusTooManyRequests, base, ue, "tokenharbor", now); got != wantRollover {
				t.Errorf("cooldown = %ds, want the next weekly rollover (%ds)", got, wantRollover)
			}
		})
	}
}

// TestTokenharborPark_LeavesOtherLanesAlone pins the blast radius. A 429 on any
// other lane keeps its own cooldown, the CodeBuddy stop-the-rotation rule keeps
// firing (fallback.go stopsRotationOnRateLimit), and tokenharbor keeps
// rotating — the park changes how long the account waits, never whether the
// pass may try the next account.
func TestTokenharborPark_LeavesOtherLanesAlone(t *testing.T) {
	now := time.Date(2026, 9, 29, 6, 22, 0, 0, time.UTC)
	ue := &upstreamError{StatusCode: http.StatusTooManyRequests, Body: []byte(tokenharborFreeAllowanceBody)}

	for _, provider := range []string{"codebuddy-intl", "cbai", "openai", ""} {
		if got := retryableCooldownSecAt(http.StatusTooManyRequests, 2*time.Second, ue, provider, now); got != 2 {
			t.Errorf("provider %q cooldown = %ds, want its own 2s base", provider, got)
		}
	}
	if !stopsRotationOnRateLimit("codebuddy-intl", ue) || !stopsRotationOnRateLimit("cbai", ue) {
		t.Error("the CodeBuddy stop-the-rotation rule must keep firing")
	}
	if stopsRotationOnRateLimit("tokenharbor", ue) {
		t.Error("tokenharbor rotates on a rate limit; the park must not change that")
	}
}

// TestMaxCooldownFor_TokenharborWeeklyCeiling pins the scope table the park and
// the ceiling both read: tokenharbor (by id or alias) is WEEKLY, and the
// CodeBuddy spellings keep their MONTHLY ceiling.
func TestMaxCooldownFor_TokenharborWeeklyCeiling(t *testing.T) {
	for _, provider := range []string{"tokenharbor", "th", "thh"} {
		if got := maxCooldownFor(provider); got != maxWeeklyResetCooldown {
			t.Errorf("maxCooldownFor(%q) = %v, want the weekly window %v", provider, got, maxWeeklyResetCooldown)
		}
	}
	for _, provider := range []string{"codebuddy-intl", "codebuddy-cn", "cbai", "cbcn"} {
		if got := maxCooldownFor(provider); got != maxMonthlyResetCooldown {
			t.Errorf("maxCooldownFor(%q) = %v, want the monthly ceiling %v", provider, got, maxMonthlyResetCooldown)
		}
	}
	if got := maxCooldownFor("openai"); got != maxResetCooldown {
		t.Errorf("maxCooldownFor(openai) = %v, want the generic %v", got, maxResetCooldown)
	}
}

// TestTokenharborQuotaVerdictResetsTheLadder pins the two fields upstream's own
// test asserts for this body (tests/unit/tokenharbor-quota.test.js:16-23):
// shouldFallback, the weekly ceiling on the cooldown — and `newBackoffLevel: 0`.
//
// A spent free allowance is not a burst, so the exponential ladder must not
// advance *and* must not keep an old level: upstream returns 0 and the row stores
// it. Without that, an account that had escalated on earlier throttles kept its
// raised level through a quota refusal and every later read treated it as a
// repeat offender.
func TestTokenharborQuotaVerdictResetsTheLadder(t *testing.T) {
	cls := providers.ClassifyErrorForProvider(http.StatusTooManyRequests, tokenharborFreeAllowanceBody, 3, "tokenharbor")
	if !cls.ShouldFallback {
		t.Fatalf("classification = %+v, want fallback", cls)
	}
	if cls.NewBackoffLevel != 0 {
		t.Errorf("NewBackoffLevel = %d, want 0 (a quota refusal resets the ladder)", cls.NewBackoffLevel)
	}
	if cls.CooldownMs > int(maxWeeklyResetCooldown/time.Millisecond) {
		t.Errorf("CooldownMs = %d, want at most the weekly ceiling", cls.CooldownMs)
	}

	// An alias routes to the same lane, and a lane whose rule *does* back off
	// still escalates — the reset is for non-burst verdicts only.
	if alias := providers.ClassifyErrorForProvider(http.StatusTooManyRequests, tokenharborFreeAllowanceBody, 3, "th"); alias.NewBackoffLevel != 0 {
		t.Errorf("th alias NewBackoffLevel = %d, want 0", alias.NewBackoffLevel)
	}
	if escalating := providers.ClassifyError(http.StatusTooManyRequests, "too many requests, slow down", 3); escalating.NewBackoffLevel == 0 {
		t.Error("a burst rule must still escalate the ladder")
	}
}

// TestNextWeeklyRolloverIsMondayMidnightUTC pins the calendar fallback the
// reference asserts when the 429 body names no parseable reset
// (tests/unit/tokenharbor-quota.test.js:25-29: getUTCDay() == 1, getUTCHours() == 0).
func TestNextWeeklyRolloverIsMondayMidnightUTC(t *testing.T) {
	// A Wednesday, mid-afternoon, in a zone that is not UTC.
	now := time.Date(2026, 11, 18, 15, 30, 0, 0, time.FixedZone("WITA", 8*3600))
	got := nextWeeklyRollover(now).UTC()
	if got.Weekday() != time.Monday {
		t.Errorf("fallback lands on %s, want Monday", got.Weekday())
	}
	if got.Hour() != 0 || got.Minute() != 0 || got.Second() != 0 {
		t.Errorf("fallback lands at %s, want 00:00:00 UTC", got)
	}
	if !got.After(now.UTC()) {
		t.Errorf("fallback %s is not after now %s", got, now.UTC())
	}

	// On a Monday already past midnight the fallback is the *next* Monday, not
	// the instant that has already gone.
	monday := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	if next := nextWeeklyRollover(monday).UTC(); next.Weekday() != time.Monday || next.Equal(monday) {
		t.Errorf("on a Monday the fallback = %s, want the next Monday", next)
	}
}
