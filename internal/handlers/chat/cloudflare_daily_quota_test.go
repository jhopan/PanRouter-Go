package chat

import (
	"net/http"
	"testing"
	"time"

	"panrouter/proxy/internal/providers"
)

// Cloudflare Workers AI meters its free allocation per calendar day, so a spent
// allocation is a whole-window refusal: the account must leave rotation until the
// day rolls over, not take the 2s→5min backoff the generic 429 earns. Upstream
// classifies the wording through QUOTA_EXHAUSTED_SIGNALS and parks it on the
// quota window's daily default (00:00 UTC).

const cloudflareDailyLimitBody = `{"success":false,"errors":[{"code":3036,"message":"daily limit exceeded"}],"result":null}`

func TestCloudflareDailyLimitIsASpentBudget(t *testing.T) {
	cls := providers.ClassifyErrorForProvider(http.StatusTooManyRequests, cloudflareDailyLimitBody, 0, "cloudflare-ai")
	if !cls.Quota {
		t.Fatalf("classified %+v, want the quota verdict", cls)
	}
	if cls.NewBackoffLevel != 0 {
		t.Errorf("backoff level = %d, want 0: a spent day must not climb the burst ladder", cls.NewBackoffLevel)
	}
}

// A genuine burst from the same lane keeps the short cooldown: the daily rows
// must not have swallowed every 429.
func TestCloudflareBurstRateLimitStaysABurst(t *testing.T) {
	burst := `{"success":false,"errors":[{"code":1000,"message":"Rate limit exceeded, too many requests"}],"result":null}`
	cls := providers.ClassifyErrorForProvider(http.StatusTooManyRequests, burst, 0, "cloudflare-ai")
	if cls.Quota {
		t.Errorf("classified %+v, want a burst rather than a spent day", cls)
	}
	if cls.CooldownMs >= int(time.Hour/time.Millisecond) {
		t.Errorf("classified %+v, want a short burst cooldown", cls)
	}
}

// The skip: the account is parked until the next 00:00 UTC, so every request in
// between falls to another connection instead of re-dialling the empty one.
func TestCloudflareDailyParkReachesTheNextUTCMidnight(t *testing.T) {
	ue := &upstreamError{StatusCode: http.StatusTooManyRequests, Body: []byte(cloudflareDailyLimitBody)}
	cls := providers.ClassifyErrorForProvider(ue.StatusCode, cloudflareDailyLimitBody, 0, "cloudflare-ai")

	secs := (&ChatHandler{}).cooldownSecFor("conn-cf", "cloudflare-ai", ue, cloudflareDailyLimitBody, cls)
	if secs <= 60 {
		t.Fatalf("cooldown = %ds, want a park that reaches the calendar reset", secs)
	}
	if max := int((24 * time.Hour) / time.Second); secs > max {
		t.Errorf("cooldown = %ds, want at most a day", secs)
	}
	unlock := time.Now().UTC().Add(time.Duration(secs) * time.Second)
	if unlock.Hour() != 0 || unlock.Minute() != 0 || unlock.Second() != 0 {
		t.Errorf("unlock at %v, want the next 00:00 UTC", unlock)
	}
}

// The alias resolves to the same window, so a connection stored as `cf` skips
// exactly like `cloudflare-ai`.
func TestCloudflareAliasSharesTheDailyWindow(t *testing.T) {
	if scope, ok := scopeFor("cf"); !ok || scope != scopeDaily {
		t.Fatalf("scopeFor(cf) = %v, %v; want the daily window", scope, ok)
	}
}