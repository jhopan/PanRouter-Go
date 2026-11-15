// Antigravity exhausted-bucket persistence — the KV half of
// src/sse/services/antigravityQuota.js (persistExhausted /
// removePersistedIfHealthy / loadPersistedAntigravityQuota).
//
// Only a bucket reading 0% with a FUTURE resetAt is worth a row: it can block
// the account for days or weeks and must survive a restart. Strike blocks (15m,
// synthetic) and healthy readings stay in memory — persisting them would keep a
// stale block alive across an update.
package chat

import (
	"time"

	json "panrouter/proxy/internal/fastjson"
	"panrouter/proxy/internal/log"
)

// AgQuotaScope is the KV scope holding the persisted exhausted buckets.
const AgQuotaScope = "ag_quota"

// agNow is the clock every Antigravity TTL decision reads: the eligibility
// flag's 15m window, the exhausted buckets' reset comparison and the
// re-verification cooldown. A var so tests move time instead of sleeping.
var agNow = func() time.Time { return time.Now().UTC() }

// agPersistedQuota is one persisted exhausted bucket. resetAt is RFC3339 text,
// the same shape the Node writer stores, so either side can read the row.
type agPersistedQuota struct {
	RemainingPercentage float64 `json:"remainingPercentage"`
	ResetAt             string  `json:"resetAt"`
}

// persistAntigravityExhausted mirrors upstream persistExhausted: write the
// exhausted buckets of a reading to the connection's KV row. Fail-open — a KV
// write failure must never block the request path.
func persistAntigravityExhausted(connectionID string, quotas map[string]AntigravityModelQuota) {
	kv := kvStore()
	if kv == nil || connectionID == "" {
		return
	}
	exhausted := agExhaustedBuckets(quotas, agNow())
	if len(exhausted) == 0 {
		return
	}
	raw, err := json.Marshal(exhausted)
	if err != nil {
		return
	}
	if err := kv.SetKV(AgQuotaScope, connectionID, string(raw)); err != nil {
		log.Warn("ag_quota", "persist exhausted buckets failed", "connection", shortConnID(connectionID), "error", err)
	}
}

// removeAntigravityPersistedIfHealthy mirrors upstream removePersistedIfHealthy:
// once nothing is exhausted the row goes away, so a recovered account does not
// come back blocked after the next restart.
func removeAntigravityPersistedIfHealthy(connectionID string, quotas map[string]AntigravityModelQuota) {
	kv := kvStore()
	if kv == nil || connectionID == "" {
		return
	}
	if len(agExhaustedBuckets(quotas, agNow())) > 0 {
		return
	}
	if err := kv.DeleteKV(AgQuotaScope, connectionID); err != nil {
		log.Warn("ag_quota", "clear persisted exhausted buckets failed", "connection", shortConnID(connectionID), "error", err)
	}
}

// LoadPersistedAntigravityQuota restores the persisted exhausted buckets into the
// in-memory cache at startup. Expired entries are dropped, and a row that ends up
// empty is deleted. Returns how many buckets were restored. Fail-open: a KV error
// never prevents startup.
func LoadPersistedAntigravityQuota() int {
	kv := kvStore()
	if kv == nil {
		return 0
	}
	rows, err := kv.GetKVScope(AgQuotaScope)
	if err != nil {
		log.Warn("ag_quota", "loadPersistedAntigravityQuota failed (non-fatal)", "error", err)
		return 0
	}
	now := agNow()
	restored := 0
	for connectionID, raw := range rows {
		var stored map[string]agPersistedQuota
		if json.Unmarshal([]byte(raw), &stored) != nil || len(stored) == 0 {
			continue
		}
		live := make(map[string]AntigravityModelQuota, len(stored))
		for model, entry := range stored {
			resetAt, err := time.Parse(time.RFC3339, entry.ResetAt)
			if err != nil {
				continue
			}
			if entry.RemainingPercentage != 0 || !resetAt.UTC().After(now) {
				continue
			}
			live[model] = AntigravityModelQuota{RemainingPercentage: 0, ResetAt: resetAt.UTC()}
		}
		if len(live) == 0 {
			// Everything in the row expired — clean it up instead of reloading
			// it on every boot.
			if err := kv.DeleteKV(AgQuotaScope, connectionID); err != nil {
				log.Warn("ag_quota", "drop expired quota row failed", "connection", shortConnID(connectionID), "error", err)
			}
			continue
		}
		agQuotaMu.Lock()
		agQuotaCache[connectionID] = live
		agQuotaMu.Unlock()
		restored += len(live)
		log.Info("ag_quota", "restored exhausted bucket(s) from DB",
			"connection", shortConnID(connectionID), "count", len(live))
	}
	return restored
}

// agExhaustedBuckets picks the entries worth persisting: 0% with a reset still
// ahead of us. A 0% whose reset has passed is not a block — it is a stale
// reading, and writing it would freeze the account on the next boot.
func agExhaustedBuckets(quotas map[string]AntigravityModelQuota, now time.Time) map[string]agPersistedQuota {
	out := make(map[string]agPersistedQuota)
	for model, entry := range quotas {
		if entry.RemainingPercentage != 0 || entry.ResetAt.IsZero() || !entry.ResetAt.After(now) {
			continue
		}
		out[model] = agPersistedQuota{
			RemainingPercentage: entry.RemainingPercentage,
			ResetAt:             entry.ResetAt.UTC().Format(time.RFC3339),
		}
	}
	return out
}

// antigravityQuotaCacheSnapshot copies the cached buckets of one connection. The
// error path persists what the cache currently holds (upstream:
// `quotaCache.get(connectionId) || {}`), which includes strike-synthesized
// entries.
func antigravityQuotaCacheSnapshot(connectionID string) map[string]AntigravityModelQuota {
	agQuotaMu.RLock()
	defer agQuotaMu.RUnlock()
	cached, ok := agQuotaCache[connectionID]
	if !ok {
		return nil
	}
	out := make(map[string]AntigravityModelQuota, len(cached))
	for model, entry := range cached {
		out[model] = entry
	}
	return out
}
