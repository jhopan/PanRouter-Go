package chat

import (
	"net/http"
	"testing"
	"time"
)

func TestExtractResetDuration_GoogleRPCDetails(t *testing.T) {
	body := []byte(`{
  "error": {
    "code": 429,
    "message": "Individual quota reached. Please upgrade your subscription to increase your limits. Resets in 1h12m28s.",
    "status": "RESOURCE_EXHAUSTED",
    "details": [
      {
        "@type": "type.googleapis.com/google.rpc.ErrorInfo",
        "reason": "QUOTA_EXHAUSTED",
        "domain": "cloudcode-pa.googleapis.com",
        "metadata": {
          "uiMessage": "true",
          "model": "gemini-3.8-flash-tiered",
          "quotaResetDelay": "1h12m28.109534319s"
        }
      }
    ]
  }
}`)

	dur, ok := extractResetDuration(body)
	if !ok {
		t.Fatalf("expected extractResetDuration to succeed")
	}

	expectedMin := 1*time.Hour + 12*time.Minute + 28*time.Second
	expectedMax := 1*time.Hour + 12*time.Minute + 29*time.Second
	if dur < expectedMin || dur > expectedMax {
		t.Errorf("expected duration around 1h12m28s, got %v", dur)
	}

	retryAfter := extractRetryAfter(body)
	if retryAfter == "" {
		t.Errorf("expected non-empty retryAfter from Google RPC body")
	}
}

func TestExtractResetDuration_TextMessage(t *testing.T) {
	body := []byte(`{"error":{"message":"Rate limit exceeded. Resets in 45m30s. Try again later."}}`)
	dur, ok := extractResetDuration(body)
	if !ok {
		t.Fatalf("expected extractResetDuration to succeed on message text")
	}
	expected := 45*time.Minute + 30*time.Second
	if dur != expected {
		t.Errorf("expected 45m30s, got %v", dur)
	}
}

func TestExtractResetDuration_CodeBuddyDate(t *testing.T) {
	cst := time.FixedZone("CST", 8*3600)
	future := time.Now().In(cst).Add(25 * time.Minute)
	futureStr := future.Format("2006-01-02 15:04:05")
	body := []byte(`{"code":6004,"msg":"模型超出频率限制,恢复时间 ` + futureStr + ` UTC+8"}`)
	dur, ok := extractResetDuration(body)
	if !ok {
		t.Fatalf("expected extractResetDuration to succeed on CodeBuddy date format")
	}
	// The body names 25 minutes out and the parser adds upstream's ten-minute
	// safety margin (RESET_SAFETY_MARGIN_MS), because asking one second early
	// earns another refusal — which is itself the automation signal.
	if dur < 34*time.Minute || dur > 36*time.Minute {
		t.Errorf("expected duration around 35m (25m + 10m margin), got %v", dur)
	}
}

// TestExtractResetDuration_CodeBuddyDateHonoursTheBodyOffset pins the reason
// the offset is read out of the body rather than taken from the host: 10:05 at
// UTC+8 is 02:05 UTC whether the gateway runs on WIB (UTC+7), WITA (UTC+8) or
// anything else. Building the instant from the server's local zone would shift
// the lock by the difference — an hour, on the deployment that reported it.
func TestExtractResetDuration_CodeBuddyDateHonoursTheBodyOffset(t *testing.T) {
	base := time.Now().UTC().Add(30 * time.Minute).Truncate(time.Second)
	// Same wall-clock instant, expressed in two offsets the provider might
	// print. Both must resolve to the same wait.
	wib := base.In(time.FixedZone("WIB", 7*3600)).Format("2006-01-02 15:04:05")
	wita := base.In(time.FixedZone("WITA", 8*3600)).Format("2006-01-02 15:04:05")

	first, ok := extractResetDuration([]byte(
		`{"code":6004,"msg":"超出频率限制,恢复时间 ` + wib + ` UTC+7"}`))
	if !ok {
		t.Fatal("UTC+7 body did not parse")
	}
	second, ok := extractResetDuration([]byte(
		`{"code":6004,"msg":"超出频率限制,恢复时间 ` + wita + ` UTC+8"}`))
	if !ok {
		t.Fatal("UTC+8 body did not parse")
	}
	if diff := first - second; diff > time.Minute || diff < -time.Minute {
		t.Errorf("the two offsets resolve %v apart, want the same instant: %v vs %v", diff, first, second)
	}
}

// TestExtractResetDuration_MonthlyResetIsNotClampedToTwoHours is the behaviour
// that keeps a monthly-quota account out of rotation until its own reset. The
// generic ceiling is two hours; clamping a reset days away to that is what put
// an exhausted CodeBuddy account back in the pool.
func TestExtractResetDuration_MonthlyResetIsNotClampedToTwoHours(t *testing.T) {
	cst := time.FixedZone("CST", 8*3600)
	future := time.Now().In(cst).Add(20 * 24 * time.Hour)
	futureStr := future.Format("2006-01-02 15:04:05")
	body := []byte(`{"code":6004,"msg":"超出频率限制,恢复时间 ` + futureStr + ` UTC+8"}`)

	dur, ok := extractResetDuration(body)
	if !ok {
		t.Fatal("expected the CodeBuddy date form to parse")
	}
	if dur < 19*24*time.Hour || dur > 21*24*time.Hour {
		t.Errorf("dur = %v, want the reset itself (~20 days), not a two-hour clamp", dur)
	}
	if dur > maxMonthlyResetCooldown {
		t.Errorf("dur = %v exceeds the 40-day ceiling", dur)
	}
}

// TestMaxCooldownFor pins the per-provider ceiling: the generic two hours is
// right for a provider that names a wait in minutes, and wrong for the two that
// name a real release time.
func TestMaxCooldownFor(t *testing.T) {
	tests := []struct {
		provider string
		want     time.Duration
	}{
		{"openai", maxResetCooldown},
		// Antigravity's free tier refills WEEKLY (upstream quotaWindows.js:
		// `antigravity: { scope: QUOTA_SCOPES.WEEKLY, usageKey: "gemini_weekly" }`),
		// so the reset it names is at most one window out. A two-hour ceiling
		// handed the account back while it was still out of quota, and the
		// request was re-dialled for the rest of the week.
		{"antigravity", maxWeeklyResetCooldown},
		{"deepseek-web", deepSeekWebMaxMuteCooldown},
		{"codebuddy-intl", maxMonthlyResetCooldown},
		{"codebuddy-cn", maxMonthlyResetCooldown},
		{"cbai", maxMonthlyResetCooldown},
		{"cbcn", maxMonthlyResetCooldown},
	}
	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			if got := maxCooldownFor(tt.provider); got != tt.want {
				t.Errorf("maxCooldownFor(%q) = %v, want %v", tt.provider, got, tt.want)
			}
		})
	}
}

// TestRetryableCooldownSec_CodeBuddyMonthlyWindow pins the end-to-end reading: a
// 429 whose body names a monthly reset parks the account for that window, while
// the same body on a provider with the generic ceiling is capped at two hours.
func TestRetryableCooldownSec_CodeBuddyMonthlyWindow(t *testing.T) {
	cst := time.FixedZone("CST", 8*3600)
	future := time.Now().In(cst).Add(10 * 24 * time.Hour)
	body := []byte(`{"code":6004,"msg":"超出频率限制,恢复时间 ` +
		future.Format("2006-01-02 15:04:05") + ` UTC+8"}`)
	ue := &upstreamError{StatusCode: http.StatusTooManyRequests, Body: body}

	got := retryableCooldownSec(http.StatusTooManyRequests, 2*time.Second, ue, "codebuddy-intl")
	if want := int((10*24*time.Hour + 10*time.Minute) / time.Second); got != want {
		t.Errorf("codebuddy-intl cooldown = %ds, want %ds", got, want)
	}

	capped := retryableCooldownSec(http.StatusTooManyRequests, 2*time.Second, ue, "openai")
	if want := int(maxResetCooldown / time.Second); capped != want {
		t.Errorf("openai cooldown = %ds, want the two-hour ceiling %ds", capped, want)
	}
}

// TestCodeBuddyResetInstant_HonoursTheBodyOffset is the absolute-instant half
// of the reset contract: the lock is built from the offset printed in the
// body, never from the server's zone.
//
// The deployment that reported this runs WIB (UTC+7) while the body said
// UTC+8, so reading 10:05 as server-local parked the account an hour off the
// real reset — and the retry inside that window is the automation signature
// the provider bans on. Swapping time.Local proves the answer never consults
// it.
func TestCodeBuddyResetInstant_HonoursTheBodyOffset(t *testing.T) {
	originalLocal := time.Local
	t.Cleanup(func() { time.Local = originalLocal })

	wantReset := time.Date(2026, 11, 2, 2, 5, 0, 0, time.UTC)
	wita := time.FixedZone("WITA", 8*3600)
	tests := []struct {
		name string
		body string
	}{
		{
			name: "the deployed shape",
			body: `{"code":6004,"msg":"超出频率限制, your usage will reset at 2026-11-02 10:05:00 UTC+8"}`,
		},
		{
			name: "zero-padded offset",
			body: `{"code":6004,"msg":"resets at 2026-11-02 10:05:00 UTC+08:00"}`,
		},
		{
			name: "hours and minutes",
			body: `{"code":6004,"msg":"resets at 2026-11-02 10:05:00 UTC+8:00"}`,
		},
		{
			name: "no offset falls back to the provider's UTC+8",
			body: `{"code":6004,"msg":"resets at 2026-11-02 10:05:00"}`,
		},
		{
			name: "the server's own zone named in the body",
			body: `{"code":6004,"msg":"resets at 2026-11-02 09:05:00 UTC+7"}`,
		},
		{
			name: "a west offset",
			body: `{"code":6004,"msg":"resets at 2026-11-01 21:05:00 UTC-5"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, zone := range []*time.Location{originalLocal, time.FixedZone("WIB", 7*3600), wita, time.UTC} {
				time.Local = zone
				got, ok := codebuddyResetInstant([]byte(tt.body))
				if !ok {
					t.Fatalf("codebuddyResetInstant failed under host zone %v", zone)
				}
				if !got.Equal(wantReset) {
					t.Errorf("reset instant = %s, want %s (host zone %v)",
						got.UTC().Format(time.RFC3339), wantReset.Format(time.RFC3339), zone)
				}
				// The lock is the reset plus the ten-minute margin.
				locked := got.Add(resetSafetyMargin)
				if want := "2026-11-02T02:15:00Z"; locked.UTC().Format(time.RFC3339) != want {
					t.Errorf("lock = %s, want %s", locked.UTC().Format(time.RFC3339), want)
				}
				if want := "2026-11-02 10:15:00"; locked.In(wita).Format("2006-01-02 15:04:05") != want {
					t.Errorf("lock in WITA = %q, want %q", locked.In(wita).Format("2006-01-02 15:04:05"), want)
				}
			}
		})
	}
}

// A body without a usable instant must not invent one: the generic cooldown
// classification stays in charge.
func TestCodeBuddyResetInstant_RejectsMalformedBodies(t *testing.T) {
	for _, body := range []string{
		`{"code":6004,"msg":"超出频率限制"}`,
		`{"code":6004,"msg":"resets at 2026-11-02 10:05:00 UTC+25"}`,
		`{"code":6004,"msg":"resets at 2026-11-02 10:05:00 UTC+8:75"}`,
	} {
		if _, ok := codebuddyResetInstant([]byte(body)); ok {
			t.Errorf("codebuddyResetInstant(%s) parsed, want rejection", body)
		}
	}
}

func TestExtractResetDuration_ClampingSafety(t *testing.T) {
	// Case 1: Extremely long duration (> 2 hours) is clamped to maxResetCooldown (2 hours) to avoid deadlocks.
	bodyLong := []byte(`{"error":{"message":"Resets in 720h."}}`)
	durLong, ok := extractResetDuration(bodyLong)
	if !ok {
		t.Fatalf("expected ok for long duration")
	}
	if durLong != maxResetCooldown {
		t.Errorf("expected clamped to %v, got %v", maxResetCooldown, durLong)
	}

	// Case 2: Sub-5s duration is clamped to minResetCooldown (5s)
	bodyShort := []byte(`{"error":{"message":"Resets in 1s."}}`)
	durShort, ok := extractResetDuration(bodyShort)
	if !ok {
		t.Fatalf("expected ok for short duration")
	}
	if durShort != minResetCooldown {
		t.Errorf("expected clamped to %v, got %v", minResetCooldown, durShort)
	}
}

func TestBlockAntigravityModelUntil_Safety(t *testing.T) {
	ClearAntigravityQuotaCache()
	connID := "test-conn-123"
	// The 3.7 family shares one backend id across its tiers, so a block on one
	// tier must block the whole family.
	model := "gemini-3.7-flash-low"

	// Initially not blocked
	if IsAntigravityModelBlocked(connID, model) {
		t.Errorf("expected not blocked initially")
	}

	// Block for 1 hour
	resetAt := time.Now().UTC().Add(1 * time.Hour)
	BlockAntigravityModelUntil(connID, model, resetAt)

	// Model should now be blocked
	if !IsAntigravityModelBlocked(connID, model) {
		t.Errorf("expected model %s to be blocked", model)
	}

	// Its canonical backend must also be blocked
	if !IsAntigravityModelBlocked(connID, "gemini-3.7-flash-tiered") {
		t.Errorf("expected canonical model gemini-3.7-flash-tiered to also be blocked")
	}

	// An expired resetAt should not block
	pastResetAt := time.Now().UTC().Add(-1 * time.Minute)
	BlockAntigravityModelUntil(connID, "gemini-3.7-flash-high", pastResetAt)
	if IsAntigravityModelBlocked(connID, "gemini-3.7-flash-high") {
		t.Errorf("expected expired resetAt not to block")
	}
}
