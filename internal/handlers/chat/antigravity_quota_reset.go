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

// antigravityLowestBucketRemaining reports the smallest remaining percentage
// across the account's LIVE buckets, and whether any live reading exists at all.
//
// No model is needed, and none can be: what this feeds is a decision to take the
// whole ACCOUNT out of rotation for a week, and an account is only provably spent
// when every bucket it reports has nothing left. Judging that by one bucket is
// what parked a healthy account for 4.5 days on a Claude 429 while its Gemini
// bucket still read 99% (measured 2026-11-18: cooldown_s=393774 until
// 2026-11-23, on an account whose own client answered the same model normally).
//
// A lapsed reset is not a block, so a stale row is ignored rather than counted
// as zero.
func antigravityLowestBucketRemaining(connectionID string, now time.Time) (float64, bool) {
	if connectionID == "" {
		return 0, false
	}
	agQuotaMu.RLock()
	buckets := agQuotaCache[connectionID]
	lowest, found := 0.0, false
	for _, quota := range buckets {
		if quota.ResetAt.IsZero() || !quota.ResetAt.After(now) {
			continue
		}
		if !found || quota.RemainingPercentage < lowest {
			lowest = quota.RemainingPercentage
		}
		found = true
	}
	agQuotaMu.RUnlock()
	return lowest, found
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
