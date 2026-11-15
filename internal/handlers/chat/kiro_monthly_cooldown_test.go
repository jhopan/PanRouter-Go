// Kiro's monthly cap and the cooldown it earns — parity with upstream
// src/sse/services/auth.js (`kiroMonthly`) and open-sse/config/quotaWindows.js
// (`kiro: { scope: QUOTA_SCOPES.MONTHLY }`).
//
// The failure this pins: the 402 body is
// `{"message":"You have reached the limit.","reason":"MONTHLY_REQUEST_COUNT"}` —
// no "quota" keyword anywhere — so it fell through to the generic status rule
// and the account was locked for two hours. Two hours later it was re-dialled,
// refused, locked again: for the rest of the month, once per model.
package chat

import (
	"net/http"
	"testing"
	"time"

	"panrouter/proxy/internal/providers"
)

func TestKiroMonthlyResetPrefersTheUsageReading(t *testing.T) {
	installKiroCapKV(t)
	base := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	useKiroClock(t, base)

	// No reading yet: the calendar decides.
	if got, want := KiroMonthlyReset("conn-x", base), time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("reset with no reading = %v, want %v", got, want)
	}

	// A reading that names nextDateReset wins over the calendar.
	reading := base.Add(9 * 24 * time.Hour)
	RecordKiroUsage("conn-x", kiroReading(100, 100, reading))
	if got := KiroMonthlyReset("conn-x", base); !got.Equal(reading) {
		t.Errorf("reset with a reading = %v, want the usage API's %v", got, reading)
	}
}

func TestKiroMonthlyCooldownIsTheMonthlyWindow(t *testing.T) {
	installKiroCapKV(t)
	ResetKiroCapCache()
	defer ResetKiroCapCache()

	handler := &ChatHandler{}
	ue := &upstreamError{StatusCode: http.StatusPaymentRequired, Body: []byte(`{"message":"You have reached the limit.","reason":"MONTHLY_REQUEST_COUNT"}`)}
	errorText := extractErrorText(ue.Body)

	cls := classifyForTest(http.StatusPaymentRequired, errorText)
	got := handler.cooldownSecFor("conn-m", "kiro", ue, errorText, cls)

	// One calendar month, bounded by the monthly ceiling. The exact number
	// depends on today's date, so the assertion is the window's shape: longer
	// than any throttle, never past the ceiling.
	if got <= int((24 * time.Hour).Seconds()) {
		t.Errorf("cooldown = %ds, want the monthly window rather than a two-hour throttle", got)
	}
	if got > int(maxCooldownFor("kiro").Seconds()) {
		t.Errorf("cooldown = %ds exceeds the monthly ceiling %v", got, maxCooldownFor("kiro"))
	}

	// The verdict must also leave a cap behind, so the next request skips the
	// account instead of re-discovering the refusal.
	if skip, _, _ := ShouldSkipKiroAccount("conn-m"); !skip {
		t.Error("a monthly-cap verdict must mark the account capped")
	}
}

func TestKiroThrottleKeepsTheGenericCooldown(t *testing.T) {
	installKiroCapKV(t)
	ResetKiroCapCache()
	defer ResetKiroCapCache()

	handler := &ChatHandler{}
	ue := &upstreamError{StatusCode: http.StatusTooManyRequests, Body: []byte(`{"message":"Too many requests"}`)}
	errorText := extractErrorText(ue.Body)

	got := handler.cooldownSecFor("conn-t", "kiro", ue, errorText, classifyForTest(http.StatusTooManyRequests, errorText))
	if got > 300 {
		t.Errorf("cooldown = %ds, want the transient throttle rather than a monthly park", got)
	}
	if skip, _, _ := ShouldSkipKiroAccount("conn-t"); skip {
		t.Error("a throttle says nothing about the monthly allowance")
	}
}

func TestKiroMonthlyVerdict(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		status   int
		text     string
		want     bool
	}{
		{name: "the real 402 body", provider: "kiro", status: 402, text: `{"message":"You have reached the limit.","reason":"MONTHLY_REQUEST_COUNT"}`, want: true},
		{name: "reason code alone", provider: "kiro", status: 402, text: "MONTHLY_REQUEST_COUNT", want: true},
		{name: "alias spelling", provider: "kiro", status: 429, text: "You have reached the limit.", want: true},
		{name: "another provider's words", provider: "openai", status: 402, text: "You have reached the limit.", want: false},
		{name: "a 400 is not a cap", provider: "kiro", status: 400, text: `{"reason":"MONTHLY_REQUEST_COUNT"}`, want: false},
		{name: "an unrelated 402", provider: "kiro", status: 402, text: "payment required", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := KiroMonthlyVerdict(tt.provider, tt.status, tt.text); got != tt.want {
				t.Errorf("KiroMonthlyVerdict(%q, %d, %q) = %v, want %v", tt.provider, tt.status, tt.text, got, tt.want)
			}
		})
	}
}

// classifyForTest classifies one body the way the fallback loop does, so the
// cooldown tests exercise the real rule table rather than a stub.
func classifyForTest(status int, errorText string) (cls providers.ErrorClassification) {
	return providers.ClassifyErrorForProvider(status, errorText, 0, "kiro")
}
