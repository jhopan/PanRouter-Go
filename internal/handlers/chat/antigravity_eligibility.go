// Antigravity account-eligibility pre-filter — port of
// open-sse/services/antigravityCapCache.js.
//
// Why: Google can flag an account as ineligible ("VALIDATION_REQUIRED") and
// require a browser re-verification. The official CLI 1.4.8 reads
// `ineligibleTiers` from loadCodeAssist and STOPS — it never sends the chat
// request. This gateway used to ignore the field and only regex-scan the
// payload for the word "verify", so a flagged account kept serving traffic and
// every request was a wasted round-trip.
//
// Two upstream signals carry the gate:
//
//  1. POST /v1internal:loadCodeAssist → 200 with
//     {"ineligibleTiers":[{"reasonCode":"VALIDATION_REQUIRED","validationUrl":…}]}
//  2. POST /v1internal:fetchAvailableModels → 403 PERMISSION_DENIED with
//     {"error":{"details":[{"reason":…,"metadata":{"validation_url":…}}]}}
//
// We remember the verdict per connection and skip the account BEFORE the
// request goes out, exactly like the Kiro and MiniMax pre-filters.
//
// The cache is advisory: a missing or expired entry means "no opinion" and the
// account stays eligible — we never block on a guess. A flagged entry is
// persisted so it survives a restart, and a later healthy reading clears it, so
// the user verifying in a browser is enough to bring the account back.
package chat

import (
	"sync"
	"time"

	json "panrouter/proxy/internal/fastjson"
	"panrouter/proxy/internal/log"
	"panrouter/proxy/internal/providers"
)

const (
	// AgIneligibleTTL is how long a recorded flag keeps an account out of
	// rotation (upstream CACHE_TTL_MS = 15 min). Past it the account may have
	// been verified in a browser, so the next probe decides again.
	AgIneligibleTTL = 15 * time.Minute
	// agIneligibleScope is the KV scope holding the persisted flags.
	agIneligibleScope = "antigravity_ineligible"
	// agIneligibleMaxEntries bounds the in-memory map. Google never flags more
	// than a handful of accounts per deployment, so hitting this bound means
	// the entries are stale noise: expired ones go first, and a map still at
	// the cap is dropped whole — an empty advisory cache fails open.
	agIneligibleMaxEntries = 512
)

// AntigravityEligibility is one eligibility verdict about an account: the shape
// both upstream parsers return.
type AntigravityEligibility struct {
	Eligible      bool
	ReasonCode    string
	ReasonMessage string
	// ValidationURL is the browser-verification link Google shipped. It carries
	// a continuation token, so it stays in memory (and on the connection row's
	// providerSpecificData.verificationUrl, the one sanctioned store) — never
	// in the KV row or a log line. See providers.RedactAppealURLs.
	ValidationURL string
	TierID        string
}

// AntigravityIneligibleEntry is the cached flag for one connection.
type AntigravityIneligibleEntry struct {
	FlaggedAt     time.Time
	ReasonCode    string
	ValidationURL string
}

var (
	agIneligibleMu    sync.RWMutex
	agIneligibleCache = make(map[string]AntigravityIneligibleEntry)
)

// agIneligibleRow is the persisted flag. `flaggedAt` is a millisecond epoch to
// match the Node writer that shares this table; the validation URL is
// deliberately absent (see AntigravityEligibility.ValidationURL).
type agIneligibleRow struct {
	Flagged    bool   `json:"flagged"`
	FlaggedAt  int64  `json:"flaggedAt"`
	ReasonCode string `json:"reasonCode"`
}

// ShouldSkipAntigravityAccount reports whether the account is known-ineligible
// and still fresh.
func ShouldSkipAntigravityAccount(connectionID string) bool {
	if connectionID == "" {
		return false
	}
	now := agNow()
	agIneligibleMu.Lock()
	defer agIneligibleMu.Unlock()
	entry, ok := agIneligibleCache[connectionID]
	if !ok {
		return false
	}
	if now.Sub(entry.FlaggedAt) > AgIneligibleTTL {
		delete(agIneligibleCache, connectionID)
		return false
	}
	return true
}

// GetAntigravityIneligibility returns the cached flag for the dashboard and the
// log, or nil when the account is not flagged.
func GetAntigravityIneligibility(connectionID string) *AntigravityIneligibleEntry {
	agIneligibleMu.RLock()
	defer agIneligibleMu.RUnlock()
	entry, ok := agIneligibleCache[connectionID]
	if !ok {
		return nil
	}
	return &entry
}

// antigravityIneligibleSkip is the selection-path test: skip the account and log
// the same line upstream logs. The account stays skipped until the TTL runs out,
// a healthy reading clears the flag, or the dashboard's check-and-release lifts
// it.
func antigravityIneligibleSkip(connectionID string) bool {
	if !ShouldSkipAntigravityAccount(connectionID) {
		return false
	}
	reason := "VALIDATION_REQUIRED"
	if info := GetAntigravityIneligibility(connectionID); info != nil && info.ReasonCode != "" {
		reason = info.ReasonCode
	}
	log.Debug("ag_quota", "INELIGIBLE — skip upstream, needs browser verification",
		"connection", shortConnID(connectionID), "reason", reason)
	return true
}

// RecordAntigravityEligibility records a verdict and reports whether the account
// is now considered flagged.
//
// A healthy or no-opinion verdict clears the entry and its persisted row
// UNCONDITIONALLY — not only when the in-memory map happens to hold the flag.
// After a restart the map is empty while the persisted row is still there, so
// clearing only on a cache hit would leave the flag alive until its TTL ran out.
// Fail-open: a KV error never fails the request.
func RecordAntigravityEligibility(connectionID string, verdict *AntigravityEligibility) bool {
	if connectionID == "" {
		return false
	}
	if verdict == nil || verdict.Eligible {
		agIneligibleMu.Lock()
		delete(agIneligibleCache, connectionID)
		agIneligibleMu.Unlock()
		agPersistIneligible(connectionID, nil)
		return false
	}

	entry := AntigravityIneligibleEntry{
		FlaggedAt:     agNow(),
		ReasonCode:    verdict.ReasonCode,
		ValidationURL: verdict.ValidationURL,
	}
	agIneligibleMu.Lock()
	if len(agIneligibleCache) >= agIneligibleMaxEntries {
		agPruneIneligibleLocked(entry.FlaggedAt)
	}
	agIneligibleCache[connectionID] = entry
	agIneligibleMu.Unlock()
	agPersistIneligible(connectionID, &entry)
	log.Warn("ag_quota", "account flagged ineligible by Google — skipping it until verified",
		"connection", shortConnID(connectionID), "reason", entry.ReasonCode,
		"verification", providers.RedactAppealURLs(entry.ValidationURL))
	return true
}

// ClearAntigravityIneligible drops the flag for a connection — the dashboard's
// "check & release" button after the user verified the account in a browser.
func ClearAntigravityIneligible(connectionID string) {
	if connectionID == "" {
		return
	}
	agIneligibleMu.Lock()
	delete(agIneligibleCache, connectionID)
	agIneligibleMu.Unlock()
	agPersistIneligible(connectionID, nil)
}

// ClearAntigravityIneligibleCache resets the in-memory map (tests).
func ClearAntigravityIneligibleCache() {
	agIneligibleMu.Lock()
	defer agIneligibleMu.Unlock()
	agIneligibleCache = make(map[string]AntigravityIneligibleEntry)
}

// RestoreAntigravityIneligible loads the persisted flags at startup. A flag
// older than the TTL is stale (the account may have been verified meanwhile) and
// is dropped together with its row, so the next probe decides again. Fail-open:
// a KV error never prevents startup, and returns how many entries were kept.
func RestoreAntigravityIneligible() int {
	kv := kvStore()
	if kv == nil {
		return 0
	}
	rows, err := kv.GetKVScope(agIneligibleScope)
	if err != nil {
		log.Warn("ag_quota", "restoreAntigravityIneligible failed (non-fatal)", "error", err)
		return 0
	}
	now := agNow()
	kept := 0
	for connectionID, raw := range rows {
		var row agIneligibleRow
		if json.Unmarshal([]byte(raw), &row) != nil || !row.Flagged {
			continue
		}
		flaggedAt := now
		if row.FlaggedAt > 0 {
			flaggedAt = time.UnixMilli(row.FlaggedAt).UTC()
		}
		if now.Sub(flaggedAt) > AgIneligibleTTL {
			_ = kv.DeleteKV(agIneligibleScope, connectionID)
			continue
		}
		agIneligibleMu.Lock()
		agIneligibleCache[connectionID] = AntigravityIneligibleEntry{FlaggedAt: flaggedAt, ReasonCode: row.ReasonCode}
		agIneligibleMu.Unlock()
		kept++
	}
	if kept > 0 {
		log.Warn("ag_quota", "restored flagged Antigravity accounts from DB", "count", kept)
	}
	return kept
}

// agPersistIneligible mirrors upstream persistEntry: the flag survives a restart,
// and clearing it removes the row. Fail-open.
func agPersistIneligible(connectionID string, entry *AntigravityIneligibleEntry) {
	kv := kvStore()
	if kv == nil {
		return
	}
	if entry == nil {
		if err := kv.DeleteKV(agIneligibleScope, connectionID); err != nil {
			log.Warn("ag_quota", "clear persisted ineligible flag failed", "connection", shortConnID(connectionID), "error", err)
		}
		return
	}
	raw, err := json.Marshal(agIneligibleRow{
		Flagged:    true,
		FlaggedAt:  entry.FlaggedAt.UnixMilli(),
		ReasonCode: entry.ReasonCode,
	})
	if err != nil {
		return
	}
	if err := kv.SetKV(agIneligibleScope, connectionID, string(raw)); err != nil {
		log.Warn("ag_quota", "persist ineligible flag failed", "connection", shortConnID(connectionID), "error", err)
	}
}

// agPruneIneligibleLocked drops expired entries. A map still at the cap after
// that is dropped whole rather than grown: the cache is advisory, and an empty
// one fails open.
func agPruneIneligibleLocked(now time.Time) {
	for id, entry := range agIneligibleCache {
		if now.Sub(entry.FlaggedAt) > AgIneligibleTTL {
			delete(agIneligibleCache, id)
		}
	}
	if len(agIneligibleCache) >= agIneligibleMaxEntries {
		agIneligibleCache = make(map[string]AntigravityIneligibleEntry)
	}
}
