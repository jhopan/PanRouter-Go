package providers

import (
	"net/http"
	"testing"
)

// Cloudflare error 1015 is a block of the EGRESS IP, not a throttle of the
// account (upstream errorConfig.js:93-100). It must land on the flat one-hour
// cooldown: the generic "rate limit" row would read the same body as a burst and
// advance the backoff ladder, so the blocked address was re-dialled every few
// seconds for the whole block instead of the request falling to another account.
func TestClassifyError_CloudflareIPBlockParksForAnHour(t *testing.T) {
	for _, text := range []string{
		"<html><head><title>Access denied</title></head><body>error 1015: You are being rate limited</body></html>",
		"You are being rate limited",
		"error 1015",
	} {
		cls := ClassifyError(http.StatusTooManyRequests, text, 0)
		if !cls.ShouldFallback {
			t.Errorf("expected fallback for %q, got %+v", text, cls)
		}
		if cls.CooldownMs != cooldownCloudflareIPBlock {
			t.Errorf("cooldown for %q = %dms, want %dms", text, cls.CooldownMs, cooldownCloudflareIPBlock)
		}
		if cls.NewBackoffLevel != 0 {
			t.Errorf("a 1015 block must not advance the backoff ladder (%q went to level %d)", text, cls.NewBackoffLevel)
		}
	}
}

// An HTML body that is not a 1015 block (Cloudflare's managed challenge, a cold
// start behind it) is infra-level and transient: short cooldown, and still not an
// account verdict.
func TestClassifyError_HTMLBodyIsShortCooldown(t *testing.T) {
	cls := ClassifyError(http.StatusForbidden, "<!doctype html><html><body>Just a moment...</body></html>", 0)
	if !cls.ShouldFallback {
		t.Errorf("expected fallback for an HTML body, got %+v", cls)
	}
	if cls.CooldownMs != cooldownShort {
		t.Errorf("cooldown = %dms, want %dms", cls.CooldownMs, cooldownShort)
	}
}

// A provider that meters a free tier states the spent WINDOW rather than a code
// (upstream QUOTA_EXHAUSTED_SIGNALS). Each wording has to classify as a spent
// budget, or the empty account takes the burst ladder and is re-dialled all day
// instead of being skipped.
func TestClassifyError_FreeWindowWordingsAreSpentBudgets(t *testing.T) {
	for _, text := range []string{
		`{"success":false,"errors":[{"code":3036,"message":"daily limit exceeded"}]}`,
		`daily quota reached`,
		`weekly limit reached`,
		`weekly quota reached`,
		`monthly quota reached`,
		`usage will resume when the quota will reset`,
		`quota exhausted`,
		`free limit reached`,
		`daily free limit reached`,
		`usage limit has been reached`,
		`individual quota reached`,
		`upgrade your subscription to increase your limits`,
		`You exceeded your current quota`,
		`{"code":"inference_cap_error"}`,
	} {
		cls := ClassifyError(http.StatusTooManyRequests, text, 0)
		if !cls.Quota {
			t.Errorf("%q classified %+v, want the quota verdict", text, cls)
		}
		if cls.NewBackoffLevel != 0 {
			t.Errorf("%q climbed the burst ladder to level %d, want 0", text, cls.NewBackoffLevel)
		}
	}
}

// A plain rate limit keeps the exponential ladder: the Cloudflare rows must not
// have widened into every 429.
func TestClassifyError_GenericRateLimitStillBacksOff(t *testing.T) {
	cls := ClassifyError(http.StatusTooManyRequests, "rate limit exceeded, slow down", 0)
	if !cls.ShouldFallback {
		t.Fatalf("expected fallback, got %+v", cls)
	}
	if cls.NewBackoffLevel != 1 {
		t.Errorf("backoff level = %d, want 1", cls.NewBackoffLevel)
	}
	if cls.CooldownMs == cooldownCloudflareIPBlock {
		t.Error("a generic rate limit must not take the Cloudflare IP-block cooldown")
	}
}
