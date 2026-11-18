// Antigravity quota-window resolution — the Go form of
// open-sse/services/quotaWindow.js `resolveQuotaResetAt` for this provider.
//
// Why it exists: Antigravity's free tier refills WEEKLY (upstream
// quotaWindows.js: `antigravity: { scope: WEEKLY, usageKey: "gemini_weekly" }`),
// and the account's own reading already names the moment: the weekly bucket's
// resetAt. Without this, a `RESOURCE_EXHAUSTED` verdict was classified as a burst,
// the account was locked for TWO SECONDS and re-dialled for the rest of the week —
// each retry a guaranteed-failed request to a lane that bans accounts over repeated
// request patterns.
//
// The order is the reference's: the usage reading first (the bucket the provider
// itself reported), then the calendar — next Monday 00:00 UTC — and the result is
// bounded by the provider's ceiling.
package chat

import "time"

// AntigravityQuotaUsageKey is the bucket whose reset decides the window. Upstream
// pins `gemini_weekly` deliberately: the resolver is provider-scoped (no model), so
// reading the earliest reset across ALL rows would unlock a genuinely exhausted
// family as soon as some other family refills.
const AntigravityQuotaUsageKey = "gemini_weekly"

// antigravityQuotaResetFor returns the instant this account's quota window reopens,
// from the reading the tracker already holds, or the next weekly boundary when no
// reading names one.
func antigravityQuotaResetFor(connectionID string, now time.Time) time.Time {
	if reset, ok := antigravityBucketResetFor(connectionID, AntigravityQuotaUsageKey, now); ok {
		return reset
	}
	return nextWeeklyRollover(now)
}

// antigravityBucketRemaining reports the account's own remaining percentage for
// a bucket, and whether a live reading from the provider exists at all.
func antigravityBucketRemaining(connectionID, bucket string, now time.Time) (float64, bool) {
	if connectionID == "" || bucket == "" {
		return 0, false
	}
	agQuotaMu.RLock()
	quota, ok := agQuotaCache[connectionID][bucket]
	agQuotaMu.RUnlock()
	if !ok || quota.ResetAt.IsZero() || !quota.ResetAt.After(now) {
		return 0, false
	}
	return quota.RemainingPercentage, true
}

// antigravityBucketResetFor reads one bucket's reset out of the in-memory quota
// cache, requiring it to be in the future — a lapsed reset names an account that is
// usable again, not a window to park for.
func antigravityBucketResetFor(connectionID, bucket string, now time.Time) (time.Time, bool) {
	if connectionID == "" || bucket == "" {
		return time.Time{}, false
	}
	agQuotaMu.RLock()
	buckets := agQuotaCache[connectionID]
	agQuotaMu.RUnlock()
	if len(buckets) == 0 {
		return time.Time{}, false
	}
	quota, ok := buckets[bucket]
	if !ok || quota.ResetAt.IsZero() || !quota.ResetAt.After(now) {
		return time.Time{}, false
	}
	return quota.ResetAt.UTC(), true
}
