// Antigravity weekly-family mapping — port of antigravityFamilyKey and
// antigravityArgonBucket from src/sse/services/antigravityQuota.js.
//
// The usage API only reports per-model rows on paid tiers; free-tier accounts
// expose family buckets (gemini_weekly / claude_gpt_weekly) instead. Reading the
// family bucket first is what keeps a free-tier 429 from being misread as "quota
// unknown" — the mistake that made the strike breaker block a family for only
// 15m instead of until its real reset.
package chat

import (
	"regexp"
	"strings"
	"time"

	"panrouter/proxy/internal/translator"
)

// AgArgonBucketKey is the Argon weekly bucket Google reports on plans that bill
// Argon separately (upstream 8a833631).
const AgArgonBucketKey = "gemini_argon_weekly"

var (
	agGeminiFamilyRe = regexp.MustCompile(`(?i)gemini|palm|gemma|imagen`)
	agClaudeFamilyRe = regexp.MustCompile(`(?i)claude|gpt|opus|sonnet`)
	agArgonRe        = regexp.MustCompile(`(?i)argon`)
)

// AntigravityFamilyKey maps a model id to its weekly family bucket key, or ""
// for a model outside the known families (which is then judged by its own row).
func AntigravityFamilyKey(model string) string {
	if model == "" {
		return ""
	}
	if agGeminiFamilyRe.MatchString(model) {
		return "gemini_weekly"
	}
	if agClaudeFamilyRe.MatchString(model) {
		return "claude_gpt_weekly"
	}
	return ""
}

// AntigravityIsArgonModel reports whether the model is a Gemini 4 Argon tier.
func AntigravityIsArgonModel(model string) bool {
	return model != "" && agArgonRe.MatchString(model)
}

// antigravityFamilyKeys lists the buckets that speak for a family: its weekly
// window and, on Pro/Ultra accounts, a real 5h session bucket that also blocks
// the family when spent (decolua/9router#4209).
func antigravityFamilyKeys(familyKey string) []string {
	if familyKey == "" {
		return nil
	}
	return []string{familyKey, strings.TrimSuffix(familyKey, "_weekly") + "_session"}
}

// AntigravityArgonBucket returns the Argon weekly bucket Google reported for
// this account, for an Argon model.
//
// This is a PREFERENCE, not a replacement: an Argon tier shares the free-tier
// Gemini pool while Google bills it on its own bucket, so callers read the Argon
// bucket first when the account reports one and fall back to the family bucket
// otherwise. The reverse never applies — an exhausted Argon bucket must not be
// rescued by healthy plain-Gemini quota.
func AntigravityArgonBucket(quotas map[string]AntigravityModelQuota, model string) (AntigravityModelQuota, bool) {
	if !AntigravityIsArgonModel(model) {
		return AntigravityModelQuota{}, false
	}
	bucket, ok := quotas[AgArgonBucketKey]
	return bucket, ok
}

// antigravityArgonSummaryKeys are the Argon buckets older rows and the Node
// writer may carry under a different name.
var antigravityArgonSummaryKeys = []string{"argon_weekly", "gemini_argon_session", "argon_session"}

// antigravityArgonExhausted reports whether any Argon bucket the account reports
// is spent, and whether it reports one at all. "Reports one" is what makes Argon
// the deciding bucket; without one the family bucket decides.
func antigravityArgonExhausted(quotas map[string]AntigravityModelQuota, model string, now time.Time) (exhausted, hasArgon bool) {
	if argon, ok := AntigravityArgonBucket(quotas, model); ok {
		return quotaEntryExhausted(argon, now), true
	}
	for _, key := range antigravityArgonSummaryKeys {
		bucket, ok := quotas[key]
		if !ok {
			continue
		}
		hasArgon = true
		if quotaEntryExhausted(bucket, now) {
			return true, true
		}
	}
	return false, hasArgon
}

// antigravityFamilyExhausted reports whether a family's weekly or 5h session
// bucket is spent.
func antigravityFamilyExhausted(quotas map[string]AntigravityModelQuota, familyKey string, now time.Time) bool {
	for _, key := range antigravityFamilyKeys(familyKey) {
		if bucket, ok := quotas[key]; ok && quotaEntryExhausted(bucket, now) {
			return true
		}
	}
	return false
}

// antigravityDecision is the quota row that decides a model, plus the cache key a
// synthesized block for it belongs under (upstream:
// `argonQuota ? "gemini_argon_weekly" : (familyKey || model)`).
type antigravityDecision struct {
	Quota    AntigravityModelQuota
	CacheKey string
}

// antigravityDecideQuota resolves the row that speaks for model, in upstream's
// order: the Argon bucket when the account reports one, then the weekly family
// bucket, then the per-model row and its canonical synonym.
func antigravityDecideQuota(quotas map[string]AntigravityModelQuota, model string) (antigravityDecision, bool) {
	if model == "" {
		return antigravityDecision{}, false
	}
	if argon, ok := AntigravityArgonBucket(quotas, model); ok {
		return antigravityDecision{Quota: argon, CacheKey: AgArgonBucketKey}, true
	}
	familyKey := AntigravityFamilyKey(model)
	if familyKey != "" {
		if bucket, ok := quotas[familyKey]; ok {
			return antigravityDecision{Quota: bucket, CacheKey: familyKey}, true
		}
	}
	cacheKey := model
	if familyKey != "" {
		cacheKey = familyKey
	}
	if bucket, ok := quotas[model]; ok {
		return antigravityDecision{Quota: bucket, CacheKey: cacheKey}, true
	}
	if canonical, exists := translator.AntigravityModelSynonyms[model]; exists && canonical != model {
		if bucket, ok := quotas[canonical]; ok {
			return antigravityDecision{Quota: bucket, CacheKey: cacheKey}, true
		}
	}
	return antigravityDecision{}, false
}
