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
