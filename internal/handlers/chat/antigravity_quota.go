package chat

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	json "panrouter/proxy/internal/fastjson"
	"panrouter/proxy/internal/log"
	"panrouter/proxy/internal/providers"
	"panrouter/proxy/internal/translator"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// AntigravityModelQuota represents live quota information for a single model on an Antigravity connection.
type AntigravityModelQuota struct {
	RemainingPercentage float64   `json:"remainingPercentage"`
	ResetAt             time.Time `json:"resetAt"`
}

// AntigravityWeeklyQuota holds parsed weekly quota values for a model family.
type AntigravityWeeklyQuota struct {
	Used                int       `json:"used"`
	Total               int       `json:"total"`
	RemainingPercentage float64   `json:"remainingPercentage"`
	ResetAt             time.Time `json:"resetAt"`
	DisplayName         string    `json:"displayName"`
}

// antigravityQuotaBaseURL hosts the v1internal:fetchAvailableModels discovery RPC.
// Discovery stays on PROD (parity with the Next.js reference); the daily host only
// serves chat traffic. A var (not const) so tests can point it at a local server.
var antigravityQuotaBaseURL = "https://cloudcode-pa.googleapis.com"

var (
	agQuotaMu    sync.RWMutex
	agQuotaCache = make(map[string]map[string]AntigravityModelQuota) // connectionID -> model -> quota
	// agRosterCache mirrors OMP dynamic-models-authoritative: the account's
	// served roster from fetchAvailableModels. Bundled catalog rows that
	// the account does not serve must be blocked before routing, or every
	// request to them 404s at Cloud Code.
	agRosterMu      sync.RWMutex
	agRosterCache   = make(map[string]map[string]struct{}) // connectionID -> served model id set
	agLastRefreshAt = make(map[string]time.Time)           // connectionID -> last refresh timestamp
	agInflightMu    sync.Mutex
	agInflight      = make(map[string]chan struct{}) // connectionID -> in-flight barrier channel

	// MinAntigravityQuotaRefreshInterval prevents hammering quota API during 429 bursts.
	MinAntigravityQuotaRefreshInterval = 30 * time.Second

	// Strike-breaker for optimistic quota (PR #3684): when quota reports >0 but we keep getting 429,
	// after 3 consecutive 429s within 60s, treat as exhausted for 15m.
	agStrikeMu            sync.Mutex
	agStrikes             = make(map[string][]time.Time) // key = connectionID|model -> 429 timestamps
	agStrikeBlocks        = make(map[string]time.Time)   // key -> blockUntil (15m strike block)
	agStrikeWindow        = 60 * time.Second
	agStrikeThreshold     = 3
	agStrikeBlockDuration = 15 * time.Minute

	// Explicit quota-error markers (parity with decolua/9router#4197): only
	// 429s carrying one of these count toward the strike-breaker. Generic or
	// content-triggered 429s must not synthesize 15m blocks while quota reads
	// optimistic. NOTE: bare "RESOURCE_EXHAUSTED" is deliberately NOT a
	// marker — both false-bucket and real-quota 429s carry it; real quota
	// errors additionally carry QUOTA_EXHAUSTED / "Individual quota reached".
	agQuotaErrorMarkers = []string{
		quotaMarkerRateLimitExceeded,
		quotaMarkerQuotaExhausted,
		quotaMarkerIndividualQuota,
	}
)
var (
	agWeeklyMu    sync.RWMutex
	agWeeklyCache = make(map[string]map[string]AntigravityWeeklyQuota) // cacheKey -> weekly quotas
	agWeeklyTTL   = 3 * time.Minute
	agWeeklyAt    = make(map[string]time.Time)
)

// applyActiveStrikeBlocks re-asserts active 15m strike blocks into quotas so a fresh
// optimistic quota reading cannot resurrect a pair we just circuit-broke (#3714).
func applyActiveStrikeBlocks(connectionID string, quotas map[string]AntigravityModelQuota) map[string]AntigravityModelQuota {
	now := time.Now().UTC()
	agStrikeMu.Lock()
	defer agStrikeMu.Unlock()
	for key, blockUntil := range agStrikeBlocks {
		if !strings.HasPrefix(key, connectionID+"|") {
			continue
		}
		if blockUntil.After(now) {
			model := strings.TrimPrefix(key, connectionID+"|")
			quotas[model] = AntigravityModelQuota{
				RemainingPercentage: 0,
				ResetAt:             blockUntil,
			}
		} else {
			delete(agStrikeBlocks, key)
			delete(agStrikes, key)
		}
	}
	return quotas
}

// ClearAntigravityQuotaCache resets the in-memory cache (primarily for unit tests).
func ClearAntigravityQuotaCache() {
	agQuotaMu.Lock()
	defer agQuotaMu.Unlock()
	agQuotaCache = make(map[string]map[string]AntigravityModelQuota)
	agLastRefreshAt = make(map[string]time.Time)

	agRosterMu.Lock()
	agRosterCache = make(map[string]map[string]struct{})
	agRosterMu.Unlock()

	agWeeklyMu.Lock()
	agWeeklyCache = make(map[string]map[string]AntigravityWeeklyQuota)
	agWeeklyAt = make(map[string]time.Time)
	agWeeklyMu.Unlock()
}

// BlockAntigravityModelUntil caches an exhausted model quota in memory until resetAt.
func BlockAntigravityModelUntil(connectionID, model string, resetAt time.Time) {
	if connectionID == "" || model == "" || resetAt.IsZero() {
		return
	}
	checkModels := []string{model}
	if canonical, exists := translator.AntigravityModelSynonyms[model]; exists && canonical != model {
		checkModels = append(checkModels, canonical)
	}
	if lockKey := canonicalLockModel("antigravity", model); lockKey != "" && lockKey != model {
		checkModels = append(checkModels, lockKey)
	}

	agQuotaMu.Lock()
	defer agQuotaMu.Unlock()
	if _, ok := agQuotaCache[connectionID]; !ok {
		agQuotaCache[connectionID] = make(map[string]AntigravityModelQuota)
	}
	for _, m := range checkModels {
		agQuotaCache[connectionID][m] = AntigravityModelQuota{
			RemainingPercentage: 0,
			ResetAt:             resetAt,
		}
	}
	shortConn := shortConnID(connectionID)
	log.Warn("ag_quota", "model locked until reset", "connection", shortConn, "model", model, "resetAt", resetAt.Format(time.RFC3339))
}

// IsAntigravityModelServed reports whether the account's roster lists model.
// Absence is false only when the roster is known; an unknown roster (never
// refreshed) lets the request through so a fresh account is not blanked by
// a missing discovery.
func IsAntigravityModelServed(connectionID, model string) bool {
	agRosterMu.RLock()
	defer agRosterMu.RUnlock()
	roster, ok := agRosterCache[connectionID]
	if !ok || len(roster) == 0 {
		return true
	}
	if _, ok := roster[model]; ok {
		return true
	}
	if canonical, exists := translator.AntigravityModelSynonyms[model]; exists {
		if _, ok := roster[canonical]; ok {
			return true
		}
	}
	return false
}

// IsAntigravityModelBlocked reports whether connectionID has an exhausted quota for model until resetAt.
func IsAntigravityModelBlocked(connectionID, model string) bool {
	if connectionID == "" || model == "" {
		return false
	}
	if !IsAntigravityModelServed(connectionID, model) {
		return true
	}

	agQuotaMu.RLock()
	modelsMap, ok := agQuotaCache[connectionID]
	if !ok || len(modelsMap) == 0 {
		agQuotaMu.RUnlock()
		return false
	}

	// Resolve synonym if present
	checkModels := []string{model}
	if canonical, exists := translator.AntigravityModelSynonyms[model]; exists && canonical != model {
		checkModels = append(checkModels, canonical)
	}
	if model == "gemini-4-argon" {
		checkModels = append(checkModels, "gemini-4-argon-high", "gemini-4-argon-medium")
	}
	if model == "gemini-4-carbon" {
		checkModels = append(checkModels, "gemini-4-carbon-high", "gemini-4-carbon-medium")
	}
	if model == "claude-sonnet-5-5" {
		checkModels = append(checkModels, "claude-sonnet-5-5-high")
	}
	if model == "claude-opus-5-5" {
		checkModels = append(checkModels, "claude-opus-5-5-high")
	}

	now := agNow()
	// Argon decides when the account reports its own Argon bucket (upstream
	// 8a833631): a false 0% on the shared Gemini bucket must not block an Argon
	// tier the account still serves, and an exhausted Argon bucket is never
	// rescued by healthy plain-Gemini quota. Without one, the family bucket
	// decides — that is the Ultra-plan fallback.
	if AntigravityIsArgonModel(model) {
		exhausted, hasArgon := antigravityArgonExhausted(modelsMap, model, now)
		if hasArgon {
			agQuotaMu.RUnlock()
			return exhausted
		}
	}

	// Weekly family + 5h-session buckets (parity with decolua/9router#4209:
	// session exhaustion blocks the family even when the weekly window remains).
	if antigravityFamilyExhausted(modelsMap, AntigravityFamilyKey(model), now) {
		agQuotaMu.RUnlock()
		return true
	}

	// Per-model rows, including the tier aliases that share one backend.
	for _, m := range checkModels {
		if q, exists := modelsMap[m]; exists && quotaEntryExhausted(q, now) {
			agQuotaMu.RUnlock()
			return true
		}
	}
	agQuotaMu.RUnlock()

	return false
}

// quotaEntryExhausted reports whether a cached quota entry means "do not route
// here until reset".
func quotaEntryExhausted(q AntigravityModelQuota, now time.Time) bool {
	return q.RemainingPercentage <= 0 && !q.ResetAt.IsZero() && q.ResetAt.After(now)
}

// RefreshAntigravityQuota fetches live quota for a connection from v1internal:fetchAvailableModels.
func RefreshAntigravityQuota(ctx context.Context, client *http.Client, connectionID, accessToken, projectID string) (map[string]AntigravityModelQuota, error) {
	if connectionID == "" || accessToken == "" {
		return nil, fmt.Errorf("missing connectionID or accessToken")
	}

	now := time.Now().UTC()

	// In-flight refresh coalescing
	agInflightMu.Lock()
	if ch, ok := agInflight[connectionID]; ok {
		agInflightMu.Unlock()
		// Wait for existing in-flight call to finish
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		agQuotaMu.RLock()
		res := agQuotaCache[connectionID]
		agQuotaMu.RUnlock()
		return res, nil
	}

	// Check 30s throttle
	agQuotaMu.RLock()
	lastRef := agLastRefreshAt[connectionID]
	cached := agQuotaCache[connectionID]
	agQuotaMu.RUnlock()

	if now.Sub(lastRef) < MinAntigravityQuotaRefreshInterval && cached != nil {
		agInflightMu.Unlock()
		return cached, nil
	}

	barrier := make(chan struct{})
	agInflight[connectionID] = barrier
	agInflightMu.Unlock()

	defer func() {
		agInflightMu.Lock()
		delete(agInflight, connectionID)
		close(barrier)
		agInflightMu.Unlock()
	}()

	agQuotaMu.Lock()
	agLastRefreshAt[connectionID] = now
	agQuotaMu.Unlock()

	quotaURL := antigravityQuotaBaseURL + "/v1internal:fetchAvailableModels"

	reqBodyMap := map[string]any{}
	if projectID != "" {
		reqBodyMap["project"] = projectID
	}
	bodyBytes, _ := json.Marshal(reqBodyMap)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, quotaURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("create quota request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", providers.AntigravityCLIUserAgent)
	req.Header.Set("X-Client-Name", "antigravity")
	req.Header.Set("X-Client-Version", providers.AntigravityCLIVersion)

	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}

	resp, err := client.Do(req)
	if err != nil {
		log.Warn("ag_quota", "refresh failed", "connection", connectionID[:min(8, len(connectionID))], "error", err)
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		log.Warn("ag_quota", "quota API auth error", "connection", connectionID[:min(8, len(connectionID))], "status", resp.StatusCode)
		if resp.StatusCode == http.StatusForbidden {
			// Google refuses the quota endpoint with VALIDATION_REQUIRED when the
			// account needs a browser verification. Record the verdict so the
			// pre-filter skips the account before the next request goes out; a
			// 403 whose body establishes nothing clears a stale flag, exactly as
			// upstream's usage reading does.
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			RecordAntigravityEligibility(connectionID, AntigravityVerdictFromErrorBody(body))
		}
		return nil, nil
	}

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		log.Warn("ag_quota", "quota API non-200", "connection", connectionID[:min(8, len(connectionID))], "status", resp.StatusCode, "body", string(respBody))
		return nil, fmt.Errorf("quota API returned status %d", resp.StatusCode)
	}

	var data struct {
		Models map[string]struct {
			IsInternal bool `json:"isInternal"`
			QuotaInfo  *struct {
				RemainingFraction float64 `json:"remainingFraction"`
				// any: same three shapes as the family buckets, and the same
				// shared reader (see ParseAntigravityResetTime).
				ResetTime any `json:"resetTime"`
			} `json:"quotaInfo"`
		} `json:"models"`
	}

	if err := json.UnmarshalRead(resp.Body, &data); err != nil {
		return nil, fmt.Errorf("decode quota response: %w", err)
	}

	quotas := make(map[string]AntigravityModelQuota)
	for modelKey, modelData := range data.Models {
		if modelData.QuotaInfo == nil || modelData.IsInternal {
			continue
		}
		var resetAt time.Time
		if modelData.QuotaInfo.ResetTime != nil {
			resetAt = ParseAntigravityResetTime(modelData.QuotaInfo.ResetTime)
		}
		quotas[modelKey] = AntigravityModelQuota{
			RemainingPercentage: modelData.QuotaInfo.RemainingFraction * 100,
			ResetAt:             resetAt,
		}
	}

	// If no model quotas found (e.g. Free-tier accounts have weekly quotas only),
	// best-effort fetch weekly quota summary (parity with Next.js open-sse/services/usage/google.js #3892)
	if len(quotas) == 0 {
		if weekly, err := FetchAntigravityWeeklyQuota(ctx, client, accessToken, projectID); err == nil && len(weekly) > 0 {
			for k, wq := range weekly {
				quotas[k] = AntigravityModelQuota{
					RemainingPercentage: wq.RemainingPercentage,
					ResetAt:             wq.ResetAt,
				}
			}
		}
	}

	// Re-assert active strike blocks so optimistic quota cannot resurrect blocked pair
	quotas = applyActiveStrikeBlocks(connectionID, quotas)

	// Roster-authoritative: the discovery RPC lists what this account is
	// actually served; bundled rows absent from it are dropped from the
	// cache so routing never sends a request Cloud Code will 404.
	roster := make(map[string]struct{}, len(data.Models))
	for modelKey := range data.Models {
		roster[modelKey] = struct{}{}
	}
	if len(roster) > 0 {
		agRosterMu.Lock()
		agRosterCache[connectionID] = roster
		agRosterMu.Unlock()
	}

	agQuotaMu.Lock()
	agQuotaCache[connectionID] = quotas
	agQuotaMu.Unlock()

	// Mirror the fresh reading into the KV store: persist the exhausted buckets,
	// then clear the row once the account is healthy. Sequential so the cleanup
	// cannot delete what the persist just wrote (upstream chains them the same
	// way). Fail-open on both sides.
	persistAntigravityExhausted(connectionID, quotas)
	removeAntigravityPersistedIfHealthy(connectionID, quotas)

	return quotas, nil
}

// Quota-error marker constants backing agQuotaErrorMarkers.
const (
	quotaMarkerRateLimitExceeded = "RATE_LIMIT_EXCEEDED"
	quotaMarkerQuotaExhausted    = "QUOTA_EXHAUSTED"
	quotaMarkerIndividualQuota   = "Individual quota reached"
)

// AntigravityQuotaError bundles the inputs to HandleAntigravityQuotaError.
// Kept as a struct: the handler takes 8 inputs and positional calls proved
// error-prone to extend (see the #4197 port).
type AntigravityQuotaError struct {
	Ctx          context.Context
	Client       *http.Client
	ConnectionID string
	Status       int
	Model        string
	AccessToken  string
	ProjectID    string
	// ErrorMessage is the raw upstream error body/text: 409/429s only feed the
	// optimistic strike-breaker when it carries an explicit quota marker;
	// generic/content-triggered errors return nil without striking (#4197
	// parity).
	ErrorMessage string
}

// isExplicitQuotaError reports whether an upstream error message explicitly
// indicates quota/rate limiting (parity with decolua/9router#4197).
func isExplicitQuotaError(errorMessage string) bool {
	for _, marker := range agQuotaErrorMarkers {
		if strings.Contains(errorMessage, marker) {
			return true
		}
	}
	return false
}

// HandleAntigravityQuotaError handles Antigravity 409/429 errors by refreshing live quota and returning model resetAt.
// Inputs arrive as AntigravityQuotaError: only errors carrying an explicit
// quota marker feed the optimistic strike-breaker; generic/content-triggered
// ones return nil without striking (#4197 parity).
func HandleAntigravityQuotaError(p AntigravityQuotaError) *time.Time {
	if p.Status != http.StatusConflict && p.Status != http.StatusTooManyRequests {
		return nil
	}

	shortConn := shortConnID(p.ConnectionID)
	now := agNow()

	// The body's own instant wins, exactly as upstream orders it: parseError
	// returns resetsAtMs off the 409/429 payload and chat.js only reaches for the
	// live quota API when the payload named nothing
	// (src/sse/handlers/chat.js:443, `... && !resetsAtMs`). Reading the quota
	// instead parks the bucket for whatever the live rows say — a generic two
	// hours through extractResetDuration, or the family window when the free-tier
	// account has no per-model row at all — while the body already stated the
	// exact end.
	if resetAt, ok := quotaResetInstant([]byte(p.ErrorMessage), now); ok {
		log.Info("ag_quota", "409/429 body names the reset instant; parking until it",
			"connection", shortConn, "status", p.Status, "model", p.Model,
			"resetAt", resetAt.Format(time.RFC3339))
		BlockAntigravityModelUntil(p.ConnectionID, p.Model, resetAt)
		return &resetAt
	}

	log.Info("ag_quota", "refreshing quota on error", "connection", shortConn, "status", p.Status, "model", p.Model)

	quotas, err := RefreshAntigravityQuota(p.Ctx, p.Client, p.ConnectionID, p.AccessToken, p.ProjectID)
	if err != nil || len(quotas) == 0 {
		return nil
	}

	// The family's weekly bucket speaks for a free-tier account, the Argon bucket
	// for an Argon tier, and the per-model row only when neither is reported
	// (upstream handleAntigravityQuotaError). Reading the per-model row alone
	// misclassified free-tier 429s as "quota unknown" and cache-blocked the pair
	// for 15m instead of until the real weekly reset.
	decision, ok := antigravityDecideQuota(quotas, p.Model)
	if !ok {
		return nil
	}

	if quotaEntryExhausted(decision.Quota, now) {
		log.Warn("ag_quota", "quota exhausted; CACHE_BLOCK until reset",
			"connection", shortConn, "model", p.Model, "bucket", decision.CacheKey,
			"resetAt", decision.Quota.ResetAt.Format(time.RFC3339))
		// Persist so the block survives a restart / an update.
		persistAntigravityExhausted(p.ConnectionID, antigravityQuotaCacheSnapshot(p.ConnectionID))
		res := decision.Quota.ResetAt
		return &res
	}

	return antigravityStrikeOnOptimistic(p, decision, now)
}

// antigravityStrikeOnOptimistic is the optimistic-quota strike breaker (PR
// #3684) gated by #4197: a 429/409 whose quota reading is still optimistic only
// blocks the pair after agStrikeThreshold strikes inside agStrikeWindow, and a
// generic/content-triggered error (no explicit quota marker in the message) never
// contributes a strike. Returns the block deadline when a strike trips it.
func antigravityStrikeOnOptimistic(p AntigravityQuotaError, decision antigravityDecision, now time.Time) *time.Time {
	if decision.Quota.RemainingPercentage <= 0 || !isExplicitQuotaError(p.ErrorMessage) {
		return nil
	}
	key := p.ConnectionID + "|" + p.Model
	if !agRecordStrike(key, now) {
		return nil
	}

	blockUntil := now.Add(agStrikeBlockDuration)
	agStrikeMu.Lock()
	agStrikeBlocks[key] = blockUntil
	agStrikeMu.Unlock()
	// Synthesize a 0% entry in the shared cache so the pre-filter skips this pair
	// on later requests too, not just the current retry loop (upstream stores it
	// under the deciding bucket key).
	agQuotaMu.Lock()
	if _, ok := agQuotaCache[p.ConnectionID]; !ok {
		agQuotaCache[p.ConnectionID] = make(map[string]AntigravityModelQuota)
	}
	agQuotaCache[p.ConnectionID][decision.CacheKey] = AntigravityModelQuota{
		RemainingPercentage: 0,
		ResetAt:             blockUntil,
	}
	agQuotaMu.Unlock()
	log.Warn("ag_quota", "optimistic quota strike-break: 3x429 within 60s while quota>0, CACHE_BLOCK 15m",
		"connection", shortConnID(p.ConnectionID), "model", p.Model, "bucket", decision.CacheKey,
		"blockUntil", blockUntil.Format(time.RFC3339))
	return &blockUntil
}

// agRecordStrike counts one strike inside a window anchored at the FIRST
// qualifying strike — three errors must all land within agStrikeWindow of that
// first one, not within agStrikeWindow of each other — and reports whether the
// threshold tripped. The bookkeeping is cleared on a trip so the next block needs
// three fresh strikes, exactly as upstream does.
func agRecordStrike(key string, now time.Time) bool {
	agStrikeMu.Lock()
	defer agStrikeMu.Unlock()
	cutoff := now.Add(-agStrikeWindow)
	kept := make([]time.Time, 0, len(agStrikes[key])+1)
	for _, t := range agStrikes[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	kept = append(kept, now)
	if len(kept) < agStrikeThreshold {
		agStrikes[key] = kept
		return false
	}
	delete(agStrikes, key)
	return true
}

// ClearAntigravityStrikes clears strike history for a connection/model (called on success).
func ClearAntigravityStrikes(connectionID, model string) {
	if connectionID == "" || model == "" {
		return
	}
	agStrikeMu.Lock()
	defer agStrikeMu.Unlock()
	delete(agStrikes, connectionID+"|"+model)
	delete(agStrikeBlocks, connectionID+"|"+model)
	if canonical, exists := translator.AntigravityModelSynonyms[model]; exists {
		delete(agStrikes, connectionID+"|"+canonical)
		delete(agStrikeBlocks, connectionID+"|"+canonical)
	}
}

// antigravityResetTimeSecondsCeiling separates a seconds epoch from a
// milliseconds one: anything below 1e12 is read as seconds (the same cut
// upstream's parseResetTime applies).
const antigravityResetTimeSecondsCeiling = 1e12

// antigravityResetTimeTextRe matches a reset value that is entirely digits, the
// shape upstream treats as an epoch rather than as a date string.
var antigravityResetTimeTextRe = regexp.MustCompile(`^\d+$`)

// ParseAntigravityResetTime ports parseResetTime
// (open-sse/services/usage/shared.js:15-42), the reader both Antigravity quota
// endpoints feed their reset value through (google.js:79 for the family
// buckets, google.js:246 for the per-model rows).
//
// Providers spell this field three ways and only the first was accepted before:
//
//  1. an epoch in seconds or milliseconds — as a JSON number or as an
//     all-digit string. Below 1e12 it is seconds, at or above it is
//     milliseconds (upstream's `resetValue < 1e12 ? *1000 : as-is`).
//  2. an RFC3339 instant, which is what Google's own quota service answers
//     with today.
//  3. anything else, including a non-numeric, non-timestamp string: no reset,
//     which is the zero time. A garbage value must not become "now" or an
//     arbitrary instant — the caller treats zero as "the bucket has no known
//     end" and falls back to its generic window.
//
// Precedence follows upstream's order: the numeric branch is tested first, so a
// bare-digit string is an epoch even if it could also be read as a date.
func ParseAntigravityResetTime(v any) time.Time {
	switch value := v.(type) {
	case float64:
		return antigravityResetTimeFromEpoch(value)
	case string:
		if antigravityResetTimeTextRe.MatchString(value) {
			if n, err := strconv.ParseFloat(value, 64); err == nil {
				return antigravityResetTimeFromEpoch(n)
			}
			return time.Time{}
		}
		if t, err := time.Parse(time.RFC3339, value); err == nil {
			return t.UTC()
		}
		return time.Time{}
	default:
		return time.Time{}
	}
}

// antigravityResetTimeFromEpoch converts a numeric epoch to an instant,
// treating values below antigravityResetTimeSecondsCeiling as seconds.
func antigravityResetTimeFromEpoch(n float64) time.Time {
	if n <= 0 {
		return time.Time{}
	}
	if n < antigravityResetTimeSecondsCeiling {
		return time.Unix(int64(n), 0).UTC()
	}
	return time.UnixMilli(int64(n)).UTC()
}

// ParseWeeklyQuotaSummary parses retrieveUserQuotaSummary response JSON into family quotas.
func ParseWeeklyQuotaSummary(raw []byte) map[string]AntigravityWeeklyQuota {
	var payload struct {
		Groups []struct {
			DisplayName string `json:"displayName"`
			Buckets     []struct {
				BucketID          string   `json:"bucketId"`
				DisplayName       string   `json:"displayName"`
				Window            string   `json:"window"`
				Disabled          bool     `json:"disabled"`
				RemainingFraction *float64 `json:"remainingFraction"`
				// any: the field arrives as an RFC3339 string, an epoch number
				// or an all-digit string, and a typed string field would make
				// the whole payload fail to decode on the numeric shapes.
				ResetTime any `json:"resetTime"`
			} `json:"buckets"`
		} `json:"groups"`
		QuotaSummary *struct {
			Groups []struct {
				DisplayName string `json:"displayName"`
				Buckets     []struct {
					BucketID          string   `json:"bucketId"`
					DisplayName       string   `json:"displayName"`
					Window            string   `json:"window"`
					Disabled          bool     `json:"disabled"`
					RemainingFraction *float64 `json:"remainingFraction"`
					ResetTime         any      `json:"resetTime"`
				} `json:"buckets"`
			} `json:"groups"`
		} `json:"quotaSummary"`
	}

	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil
	}

	groups := payload.Groups
	if len(groups) == 0 && payload.QuotaSummary != nil {
		groups = payload.QuotaSummary.Groups
	}
	if len(groups) == 0 {
		return nil
	}

	result := make(map[string]AntigravityWeeklyQuota)
	for _, g := range groups {
		gName := g.DisplayName
		isArgon := strings.Contains(strings.ToLower(gName), "argon")
		isGemini := !isArgon && strings.Contains(strings.ToLower(gName), "gemini")
		isClaudeGPT := strings.Contains(strings.ToLower(gName), "claude") || strings.Contains(strings.ToLower(gName), "gpt")
		if !isArgon && !isGemini && !isClaudeGPT {
			continue
		}

		for _, b := range g.Buckets {
			// Classify weekly vs 5h-session buckets (parity with
			// decolua/9router#4209). A disabled session bucket is kept at
			// 0 (upstream marks it disabled when weekly is hit); disabled
			// weekly buckets are truly disabled and skipped.
			windowType := strings.ToLower(b.Window)
			bText := strings.ToLower(b.BucketID + " " + b.DisplayName)
			isWeekly := windowType == "weekly" || strings.Contains(bText, "weekly")
			isSession := windowType == "5h" || windowType == "daily" ||
				strings.Contains(bText, "five hour") ||
				strings.Contains(bText, "5h") ||
				strings.Contains(bText, "daily")
			if !isWeekly && !isSession {
				continue
			}
			if b.Disabled && isWeekly {
				continue
			}
			var frac float64
			if b.Disabled {
				frac = 0
			} else if b.RemainingFraction != nil {
				frac = *b.RemainingFraction
			} else {
				// null or missing: skip the bucket so the quota stays unknown
				// rather than being reported as exhausted.
				continue
			}
			if frac < 0 {
				frac = 0
			}
			if frac > 1 {
				frac = 1
			}
			total := 1000
			remaining := int(math.Round(float64(total) * frac))
			used := total - remaining
			if used < 0 {
				used = 0
			}

			var resetAt time.Time
			if b.ResetTime != nil {
				resetAt = ParseAntigravityResetTime(b.ResetTime)
			}

			key := "gemini_weekly"
			dName := "Gemini (Weekly)"
			if isArgon {
				key = "gemini_argon_weekly"
				dName = "Gemini 4 Argon (Weekly)"
			} else if isClaudeGPT {
				key = "claude_gpt_weekly"
				dName = "Claude & GPT (Weekly)"
			}
			if !isWeekly {
				if isArgon {
					key = "gemini_argon_session"
					dName = "Gemini 4 Argon (5h)"
				} else if strings.HasPrefix(key, "gemini") {
					key = "gemini_session"
					dName = "Gemini (5h)"
				} else {
					key = "claude_gpt_session"
					dName = "Claude & GPT (5h)"
				}
			}

			// First matching bucket per type wins (no break: one group can
			// yield both a weekly and a session bucket).
			if _, exists := result[key]; !exists {
				entry := AntigravityWeeklyQuota{
					Used:                used,
					Total:               total,
					RemainingPercentage: frac * 100,
					ResetAt:             resetAt,
					DisplayName:         dName,
				}
				result[key] = entry
				switch key {
				case "gemini_argon_weekly":
					result["argon_weekly"] = entry
				case "gemini_argon_session":
					result["argon_session"] = entry
				}
			}
		}
	}
	return result
}

// FetchAntigravityWeeklyQuota fetches the weekly quota summary from Google's retrieveUserQuotaSummary endpoint.
func FetchAntigravityWeeklyQuota(ctx context.Context, client *http.Client, accessToken, projectID string) (map[string]AntigravityWeeklyQuota, error) {
	cacheKey := accessToken + "::" + projectID
	now := time.Now().UTC()

	agWeeklyMu.RLock()
	cached, ok := agWeeklyCache[cacheKey]
	cachedAt := agWeeklyAt[cacheKey]
	agWeeklyMu.RUnlock()
	if ok {
		if now.Sub(cachedAt) < agWeeklyTTL {
			return cached, nil
		}
		// Lazy eviction: token-rotated keys must not accumulate forever
		// (OAuth tokens refresh hourly, minting a fresh key each time).
		agWeeklyMu.Lock()
		if at, ok := agWeeklyAt[cacheKey]; ok && at.Equal(cachedAt) {
			delete(agWeeklyCache, cacheKey)
			delete(agWeeklyAt, cacheKey)
		}
		agWeeklyMu.Unlock()
	}

	url := antigravityQuotaBaseURL + "/v1internal:retrieveUserQuotaSummary"
	reqBody := map[string]any{}
	if projectID != "" {
		reqBody["project"] = projectID
	}
	bodyBytes, _ := json.Marshal(reqBody)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", providers.AntigravityCLIUserAgent)
	req.Header.Set("X-Client-Name", "antigravity")
	req.Header.Set("X-Client-Version", providers.AntigravityCLIVersion)

	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("retrieveUserQuotaSummary returned %d", resp.StatusCode)
	}

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}

	res := ParseWeeklyQuotaSummary(respBytes)
	if len(res) > 0 {
		agWeeklyMu.Lock()
		agWeeklyCache[cacheKey] = res
		agWeeklyAt[cacheKey] = now
		// Opportunistic sweep: drop entries older than 2x TTL so dead
		// token keys drain without a dedicated goroutine.
		cutoff := now.Add(-2 * agWeeklyTTL)
		for k, at := range agWeeklyAt {
			if at.Before(cutoff) {
				delete(agWeeklyCache, k)
				delete(agWeeklyAt, k)
			}
		}
		agWeeklyMu.Unlock()
	}
	return res, nil
}
