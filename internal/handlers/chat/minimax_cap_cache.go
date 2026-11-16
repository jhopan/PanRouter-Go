// MiniMax Code plan-cap cache — port of open-sse/services/minimaxCapCache.js.
//
// Why: a MiniMax Code plan that runs out mid-conversation (the 470k-token turn that
// produced this feature) answers "usage limit reached: insufficient balance (1008)",
// and retrying it in a few hours burns a real request each time. The operator asked
// for a flat 24-hour block: the account stays parked for the rest of the working day,
// and the usage tracker overwrites the entry as soon as the plan refills.
//
// The cache is advisory: a missing or stale entry means "no opinion" and the account
// stays eligible. Nothing here blocks on a guess.
package chat

import (
	"strconv"
	"strings"
	"sync"
	"time"

	json "panrouter/proxy/internal/fastjson"
	"panrouter/proxy/internal/log"
)

const (
	// MinimaxCapScope is the kv scope holding persisted plan blocks.
	MinimaxCapScope = "minimax_cap"
	// minimaxCapReadingTTL expires a HEALTHY reading fast, so a fresh one takes
	// over; an exhausted entry keeps its own 24h window instead.
	minimaxCapReadingTTL = 5 * time.Minute
	// minimaxExhaustedTTL is how long an exhausted account stays skipped.
	minimaxExhaustedTTL = 24 * time.Hour
)

// minimaxCapEntry is one connection's remembered reading. Times are unix
// milliseconds so the persisted row matches the Node writer's shape.
type minimaxCapEntry struct {
	Exhausted bool  `json:"exhausted"`
	ResetAt   int64 `json:"resetAt"`
	CheckedAt int64 `json:"checkedAt"`
}

var (
	minimaxCapMu    sync.RWMutex
	minimaxCapCache = map[string]minimaxCapEntry{}

	// minimaxNow is the clock every decision reads, so tests move time instead of
	// sleeping.
	minimaxNow = func() time.Time { return time.Now().UTC() }
)

// ResetMiniMaxCapCache forgets every in-memory entry (tests, settings change).
func ResetMiniMaxCapCache() {
	minimaxCapMu.Lock()
	defer minimaxCapMu.Unlock()
	minimaxCapCache = map[string]minimaxCapEntry{}
}

// MinimaxCapSnapshot copies one connection's entry, for tests and diagnostics.
func MinimaxCapSnapshot(connectionID string) (minimaxCapEntry, bool) {
	minimaxCapMu.RLock()
	defer minimaxCapMu.RUnlock()
	entry, ok := minimaxCapCache[connectionID]
	return entry, ok
}

// ClearMiniMaxCap forgets one connection: a successful request, a token change, or
// a manual reset. The persisted row goes with it.
func ClearMiniMaxCap(connectionID string) {
	if connectionID == "" {
		return
	}
	minimaxCapMu.Lock()
	delete(minimaxCapCache, connectionID)
	minimaxCapMu.Unlock()
	minimaxPersist(connectionID, minimaxCapEntry{})
}

// minimaxPersist writes (or removes) the persisted row. Fail-open: a kv error must
// never touch the request path.
func minimaxPersist(connectionID string, entry minimaxCapEntry) {
	kv := kvStore()
	if kv == nil || connectionID == "" {
		return
	}
	if !entry.Exhausted {
		if err := kv.DeleteKV(MinimaxCapScope, connectionID); err != nil {
			log.Debug("minimax_cap", "block row delete failed", "conn", connectionID, "error", err)
		}
		return
	}
	encoded, err := json.Marshal(entry)
	if err != nil {
		return
	}
	if err := kv.SetKV(MinimaxCapScope, connectionID, string(encoded)); err != nil {
		log.Debug("minimax_cap", "block row write failed", "conn", connectionID, "error", err)
	}
}

// LoadPersistedMiniMaxCaps restores persisted blocks at startup. An entry whose 24h
// window closed while the process was down is dropped, so a stale block never
// outlives its window. Returns how many were restored.
func LoadPersistedMiniMaxCaps() int {
	kv := kvStore()
	if kv == nil {
		return 0
	}
	rows, err := kv.GetKVScope(MinimaxCapScope)
	if err != nil {
		return 0 // fail-open: no persisted blocks is the same as none
	}
	now := minimaxNow().UnixMilli()
	restored := 0
	for connectionID, raw := range rows {
		var entry minimaxCapEntry
		if err := json.Unmarshal([]byte(raw), &entry); err != nil {
			_ = kv.DeleteKV(MinimaxCapScope, connectionID)
			continue
		}
		if !entry.Exhausted || entry.CheckedAt+minimaxExhaustedTTL.Milliseconds() <= now {
			_ = kv.DeleteKV(MinimaxCapScope, connectionID)
			continue
		}
		minimaxCapMu.Lock()
		minimaxCapCache[connectionID] = entry
		minimaxCapMu.Unlock()
		restored++
	}
	return restored
}

// RecordMiniMaxUsage stores a usage reading.
//
// A window is exhausted when its remaining percentage is 0 (or the tracker says the
// interval is spent) AND it has not reset yet. A credit balance of zero is also
// exhausted, with no reset to wait for: the user has to top up. A fresh reading
// OVERRIDES an exhausted entry, so a refilled plan becomes eligible immediately
// instead of waiting out the 24h block.
func RecordMiniMaxUsage(connectionID string, quotas map[string]any) {
	if connectionID == "" || len(quotas) == 0 {
		return
	}
	now := minimaxNow().UnixMilli()
	exhausted := false
	var resetAt int64

	for _, raw := range quotas {
		quota, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		// Credit balance: zero means the account cannot pay at all.
		if isCredit, _ := quota["isCreditBalance"].(bool); isCredit {
			balance, _ := minimaxNumber(quota["total"])
			if balance <= 0 {
				exhausted = true
			}
			continue
		}
		percent, hasPercent := minimaxNumber(quota["remainingPercentage"])
		if !hasPercent {
			percent, hasPercent = minimaxNumber(quota["remaining"])
		}
		windowReset := minimaxResetMillis(quota["resetAt"])
		if hasPercent && percent <= 0 && (windowReset == 0 || windowReset > now) {
			exhausted = true
			if windowReset > 0 && (resetAt == 0 || windowReset < resetAt) {
				resetAt = windowReset
			}
			continue
		}
		// Remember the nearest future reset so a later 429 can use it.
		if windowReset > now && resetAt == 0 {
			resetAt = windowReset
		}
	}

	entry := minimaxCapEntry{Exhausted: exhausted, ResetAt: resetAt, CheckedAt: now}
	minimaxCapMu.Lock()
	minimaxCapCache[connectionID] = entry
	minimaxCapMu.Unlock()
	minimaxPersist(connectionID, entry)
}

// ShouldSkipMiniMaxAccount reports whether the account must stay out of rotation.
// skip=false means "no opinion", never "healthy".
//
// An exhausted account keeps its 24h block (a reading that cannot be confirmed must
// not re-admit it); a healthy one goes stale after the 5-minute probe TTL so a fresh
// reading can take over.
func ShouldSkipMiniMaxAccount(connectionID string) (skip bool, until *time.Time, reason string) {
	if connectionID == "" {
		return false, nil, ""
	}
	now := minimaxNow()
	minimaxCapMu.Lock()
	defer minimaxCapMu.Unlock()
	entry, ok := minimaxCapCache[connectionID]
	if !ok {
		return false, nil, ""
	}
	ttl := minimaxCapReadingTTL
	if entry.Exhausted {
		ttl = minimaxExhaustedTTL
	}
	if now.UnixMilli()-entry.CheckedAt > ttl.Milliseconds() {
		if entry.Exhausted {
			// Past the reading TTL but inside the 24h block: the window was never
			// confirmed to have reopened, so the account stays out.
			return true, nil, "plan exhausted"
		}
		return false, nil, ""
	}
	if !entry.Exhausted {
		return false, nil, ""
	}
	if entry.ResetAt == 0 {
		// No reset time: the credit balance is empty and the user has to top up.
		// Nothing to wait for, but the 5-minute TTL would re-probe every five
		// minutes and each attempt is a real request, so it stays parked.
		return true, nil, "credit balance exhausted"
	}
	if entry.ResetAt > now.UnixMilli() {
		reset := time.UnixMilli(entry.ResetAt).UTC()
		return true, &reset, "plan window exhausted"
	}
	// The window reset: drop the entry.
	delete(minimaxCapCache, connectionID)
	return false, nil, ""
}

// MarkMiniMaxExhausted parks an account straight from a refusal, using the known
// reset when there is one.
func MarkMiniMaxExhausted(connectionID string, resetAt *time.Time) {
	if connectionID == "" {
		return
	}
	now := minimaxNow()
	entry := minimaxCapEntry{Exhausted: true, CheckedAt: now.UnixMilli()}
	if resetAt != nil && !resetAt.IsZero() {
		entry.ResetAt = resetAt.UTC().UnixMilli()
	} else {
		minimaxCapMu.RLock()
		existing, ok := minimaxCapCache[connectionID]
		minimaxCapMu.RUnlock()
		if ok {
			entry.ResetAt = existing.ResetAt
		}
	}
	minimaxCapMu.Lock()
	minimaxCapCache[connectionID] = entry
	minimaxCapMu.Unlock()
	minimaxPersist(connectionID, entry)
}

// MinimaxPlanExhaustedVerdict reports whether a failure is the M Plan window or the
// credit balance running out, which is what the 24-hour block is for
// (src/sse/services/auth.js: `isMiniMaxCodeProvider && /insufficient balance|usage
// limit reached/i`).
func MinimaxPlanExhaustedVerdict(status int, errorText string) bool {
	if status != 429 {
		return false
	}
	lower := strings.ToLower(errorText)
	return strings.Contains(lower, "insufficient balance") || strings.Contains(lower, "usage limit reached")
}

// minimaxNumber reads a numeric field, tolerating the string form some payloads use.
func minimaxNumber(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case string:
		if parsed, err := strconv.ParseFloat(strings.TrimSpace(typed), 64); err == nil {
			return parsed, true
		}
	}
	return 0, false
}

// minimaxResetMillis reads an ISO reset into unix milliseconds, 0 when absent.
func minimaxResetMillis(value any) int64 {
	text, ok := value.(string)
	if !ok || text == "" {
		return 0
	}
	parsed, err := time.Parse(time.RFC3339, text)
	if err != nil {
		return 0
	}
	return parsed.UTC().UnixMilli()
}
