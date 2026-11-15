// Antigravity pre-skip re-verification — port of the "Pre-skip re-verification"
// block in src/sse/services/antigravityQuota.js.
//
// The pre-filter skips an account whose cached weekly bucket reads 0%. That skip
// happens BEFORE any request goes out — so the chat error path, the only thing
// that ever refreshed this cache, never runs. A stale or wrong 0% therefore
// froze the account until its own cached resetAt, days away.
//
// Observed upstream (VPS, 2026-11-07): a cached gemini_weekly 0% with resetAt
// 27h 9m in the future kept a healthy account out of rotation for that whole
// window, while the upstream weekly bucket read 99.55% available and the official
// CLI served the same account without complaint. Nothing could clear the entry,
// because clearing required a request that the entry itself blocked.
//
// So: verify an exhausted bucket before believing it. One quota API call — never
// a chat call — bounded per connection, and fail-open.
package chat

import (
	"context"
	"strings"
	"sync"
	"time"

	json "encoding/json/v2"

	"panrouter/proxy/internal/log"
	"panrouter/proxy/internal/models"
)

const (
	// AgRevalidateCooldown bounds how often one connection's cached 0% bucket is
	// re-verified (upstream REVALIDATE_COOLDOWN_MS = 5 min). Shorter would spend
	// the quota API on every request; longer keeps a false 0% in force.
	AgRevalidateCooldown = 5 * time.Minute
	// agRevalidateMaxEntries bounds the cooldown map. Entries older than the
	// cooldown suppress nothing, so they are dropped on every write; this cap
	// only guards a burst of distinct connection ids.
	agRevalidateMaxEntries = 4096
)

// AntigravityRevalidateCandidate is one connection the pre-filter may skip: what
// the selector needs to decide, and what a re-verification call needs to run.
type AntigravityRevalidateCandidate struct {
	ConnectionID string
	AccessToken  string
	ProjectID    string
}

// AntigravityQuotaRefresher re-reads live quota for one account. Injected so the
// selector and the re-verification are testable without a network.
type AntigravityQuotaRefresher func(ctx context.Context, connectionID, accessToken, projectID string) (map[string]AntigravityModelQuota, error)

var (
	agRevalidateMu     sync.Mutex
	agLastRevalidateAt = make(map[string]time.Time)
)

// ClearAntigravityRevalidateState forgets the re-verification timestamps (tests,
// mirroring upstream's _clearRevalidateState).
func ClearAntigravityRevalidateState() {
	agRevalidateMu.Lock()
	defer agRevalidateMu.Unlock()
	agLastRevalidateAt = make(map[string]time.Time)
}

// SelectAntigravitySkipsToRevalidate reports which connections are about to be
// skipped on a cached, still-future exhausted bucket and are due for
// re-verification. Pure selection — no network, no clock of its own, safe to
// unit-test.
//
// Only a bucket that would actually block THIS request is worth a call: a bucket
// whose reset has already passed blocks nothing, and an account without a token
// cannot be re-read at all.
func SelectAntigravitySkipsToRevalidate(candidates []AntigravityRevalidateCandidate, model string, now time.Time) []string {
	if len(candidates) == 0 || model == "" {
		return nil
	}
	familyKey := AntigravityFamilyKey(model)
	if familyKey == "" {
		return nil
	}
	var ids []string
	for _, c := range candidates {
		if c.ConnectionID == "" || c.AccessToken == "" {
			continue
		}
		cached := antigravityQuotaCacheSnapshot(c.ConnectionID)
		family, hasFamily := cached[familyKey]
		if !hasFamily {
			continue
		}
		// Argon (upstream 8a833631): its own weekly bucket decides when reported;
		// the family bucket stays the fallback.
		deciding := family
		if argon, ok := AntigravityArgonBucket(cached, model); ok {
			deciding = argon
		}
		if !quotaEntryExhausted(deciding, now) {
			continue
		}
		if now.Sub(agLastRevalidate(c.ConnectionID)) < AgRevalidateCooldown {
			continue
		}
		ids = append(ids, c.ConnectionID)
	}
	return ids
}

// RevalidateAntigravityExhausted re-verifies the cached exhausted buckets we are
// about to skip on. One quota call per connection per cooldown, in parallel like
// upstream's Promise.all so a long quota call does not add its latency to the
// request that triggered it.
//
// Fail-open: it never returns an error, and a refresh that fails leaves the
// cached reading untouched — the request then proceeds exactly as it would have
// without the re-verification.
func RevalidateAntigravityExhausted(
	ctx context.Context,
	refresh AntigravityQuotaRefresher,
	candidates []AntigravityRevalidateCandidate,
	model string,
	now time.Time,
) {
	ids := SelectAntigravitySkipsToRevalidate(candidates, model, now)
	if len(ids) == 0 || refresh == nil {
		return
	}

	byID := make(map[string]AntigravityRevalidateCandidate, len(candidates))
	for _, c := range candidates {
		byID[c.ConnectionID] = c
	}

	// Record every id BEFORE awaiting: a slow quota call must not let the next
	// request start a second one for the same connection.
	agMarkRevalidated(ids, now)

	var wg sync.WaitGroup
	for _, id := range ids {
		c := byID[id]
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := refresh(ctx, id, c.AccessToken, c.ProjectID); err != nil {
				log.Warn("ag_quota", "pre-skip re-verify failed (non-fatal)", "connection", shortConnID(id), "error", err)
			}
		}()
	}
	wg.Wait()
}

// antigravityAnyExhaustedCached reports whether any of conns holds a cached
// bucket that would block model right now. It is the cheap gate in front of
// AntigravityRevalidateCandidates: the per-row data decode, and any quota call,
// only happen for the requests the pre-filter is about to skip.
func antigravityAnyExhaustedCached(conns []*models.ProviderConnection, model string, now time.Time) bool {
	for _, c := range conns {
		if c == nil || c.ID == "" {
			continue
		}
		cached := antigravityQuotaCacheSnapshot(c.ID)
		if len(cached) == 0 {
			continue
		}
		decision, ok := antigravityDecideQuota(cached, model)
		if ok && quotaEntryExhausted(decision.Quota, now) {
			return true
		}
	}
	return false
}

// AntigravityRevalidateCandidates builds the selector's input from stored
// connection rows. The token and project id live in each row's data blob, which
// is why the caller passes rows rather than ids.
func AntigravityRevalidateCandidates(conns []*models.ProviderConnection) []AntigravityRevalidateCandidate {
	out := make([]AntigravityRevalidateCandidate, 0, len(conns))
	for _, c := range conns {
		if c == nil || c.ID == "" {
			continue
		}
		var connData ConnectionData
		if strings.TrimSpace(c.Data) != "" {
			_ = json.Unmarshal([]byte(c.Data), &connData)
		}
		out = append(out, AntigravityRevalidateCandidate{
			ConnectionID: c.ID,
			AccessToken:  resolveProviderAuthToken("antigravity", &connData, extractAPIKey(&connData)),
			ProjectID:    projectIDFromConnData(c.Data),
		})
	}
	return out
}

// agLastRevalidate reads a connection's last re-verification stamp.
func agLastRevalidate(connectionID string) time.Time {
	agRevalidateMu.Lock()
	defer agRevalidateMu.Unlock()
	return agLastRevalidateAt[connectionID]
}

// agMarkRevalidated stamps the connections whose cached bucket we are about to
// re-read, pruning stamps that have outlived the cooldown first.
func agMarkRevalidated(ids []string, now time.Time) {
	agRevalidateMu.Lock()
	defer agRevalidateMu.Unlock()
	for id, at := range agLastRevalidateAt {
		if now.Sub(at) >= AgRevalidateCooldown {
			delete(agLastRevalidateAt, id)
		}
	}
	if len(agLastRevalidateAt) >= agRevalidateMaxEntries {
		agLastRevalidateAt = make(map[string]time.Time)
	}
	for _, id := range ids {
		agLastRevalidateAt[id] = now
	}
}
