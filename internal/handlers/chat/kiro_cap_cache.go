// Kiro monthly-cap pre-filter — the Go port of open-sse/services/kiroCapCache.js.
//
// Why: Kiro's monthly cap (`reason: "MONTHLY_REQUEST_COUNT"` on a 402) refuses
// EVERY model on that account. Relying on the 402 alone means every request
// still travels upstream, burns a round-trip, and only then learns the account is
// capped — for every model, again and again, until the month turns over. Kiro's
// usage API already reports the numbers (usageBreakdownList[].currentUsage,
// usageLimit, nextDateReset), so the reading is remembered per connection and the
// account is skipped BEFORE the request goes out.
//
// A cap that lasts until next month is useless if it disappears on restart, so an
// exhausted entry is persisted to the kv table (scope kiro_cap, one row per
// connection, same shape the Node writer stores) and restored at startup.
// Entries whose reset already passed are dropped on load.
//
// The cache is advisory: a missing, expired or unreadable entry means "no
// opinion" and the account stays eligible. Nothing here ever blocks on a guess.
package chat

import (
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	json "panrouter/proxy/internal/fastjson"
	"panrouter/proxy/internal/log"
	"panrouter/proxy/internal/providers"
)

// KiroCapScope is the kv scope holding persisted monthly caps.
const KiroCapScope = "kiro_cap"

// kiroCapProbeTTL expires a reading whose window could not be confirmed. It must
// NOT expire a cap that is still in effect: that mistake took a monthly-capped
// account back into rotation five minutes after the 402 (upstream
// kiro-cap-ttl.test.js, which is the regression suite for exactly this).
const kiroCapProbeTTL = 5 * time.Minute

// kiroCapEntry is one connection's remembered reading. Times are unix
// milliseconds so the persisted row is byte-compatible with the Node writer,
// which stores numbers.
type kiroCapEntry struct {
	Exhausted   bool   `json:"exhausted"`
	CappedUntil int64  `json:"cappedUntil"`
	ResetAt     int64  `json:"resetAt"`
	Source      string `json:"source"`
	// CheckedAt is when the reading was taken. Deliberately not persisted: a
	// restored cap is authoritative by its reset date, not by its age.
	CheckedAt int64 `json:"-"`
}

var (
	kiroCapMu    sync.RWMutex
	kiroCapCache = map[string]kiroCapEntry{}

	// kiroNow is the clock every cap decision reads, so tests move time instead
	// of sleeping.
	kiroNow = func() time.Time { return time.Now().UTC() }
)

// ResetKiroCapCache forgets every in-memory entry (tests, and a settings change
// that reconnects the accounts).
func ResetKiroCapCache() {
	kiroCapMu.Lock()
	defer kiroCapMu.Unlock()
	kiroCapCache = map[string]kiroCapEntry{}
}

// KiroCapSnapshot copies one connection's entry for tests and diagnostics.
func KiroCapSnapshot(connectionID string) (kiroCapEntry, bool) {
	kiroCapMu.RLock()
	defer kiroCapMu.RUnlock()
	entry, ok := kiroCapCache[connectionID]
	return entry, ok
}

// ClearKiroCap forgets one connection: after it was reconnected, or when a
// reading proves it healthy. The persisted row goes with it.
func ClearKiroCap(connectionID string) {
	if connectionID == "" {
		return
	}
	kiroCapMu.Lock()
	delete(kiroCapCache, connectionID)
	kiroCapMu.Unlock()
	kiroPersist(connectionID, kiroCapEntry{})
}

// kiroPersist writes (or removes) the persisted row for one connection. Fail-open:
// a kv error must never touch the request path.
func kiroPersist(connectionID string, entry kiroCapEntry) {
	kv := kvStore()
	if kv == nil || connectionID == "" {
		return
	}
	if !entry.Exhausted || entry.CappedUntil == 0 {
		if err := kv.DeleteKV(KiroCapScope, connectionID); err != nil {
			log.Debug("KIRO_QUOTA", "cap row delete failed", "conn", connectionID, "error", err)
		}
		return
	}
	encoded, err := json.Marshal(entry)
	if err != nil {
		return
	}
	if err := kv.SetKV(KiroCapScope, connectionID, string(encoded)); err != nil {
		log.Debug("KIRO_QUOTA", "cap row write failed", "conn", connectionID, "error", err)
	}
}

// LoadPersistedKiroCaps restores persisted caps at startup. An entry whose reset
// already passed is dropped — the month rolled over while the process was down —
// so a long-dead cap never blocks an account. Returns how many were restored.
func LoadPersistedKiroCaps() int {
	kv := kvStore()
	if kv == nil {
		return 0
	}
	rows, err := kv.GetKVScope(KiroCapScope)
	if err != nil {
		return 0 // fail-open: no persisted caps is the same as none
	}
	now := kiroNow().UnixMilli()
	restored := 0
	for connectionID, raw := range rows {
		var entry kiroCapEntry
		if err := json.Unmarshal([]byte(raw), &entry); err != nil {
			_ = kv.DeleteKV(KiroCapScope, connectionID)
			continue
		}
		if !entry.Exhausted || entry.CappedUntil <= now {
			_ = kv.DeleteKV(KiroCapScope, connectionID)
			continue
		}
		if entry.ResetAt == 0 {
			entry.ResetAt = entry.CappedUntil
		}
		if entry.Source == "" {
			entry.Source = "persisted"
		}
		entry.CheckedAt = now
		kiroCapMu.Lock()
		kiroCapCache[connectionID] = entry
		kiroCapMu.Unlock()
		restored++
	}
	return restored
}

// RecordKiroUsage stores a usage reading. The cap that matters is the monthly
// request count, which Kiro reports as a resource bucket: the first exhausted
// bucket names the reset, otherwise the nearest future reset is remembered so a
// later 402 can use it. A reading that shows quota again clears the cap.
func RecordKiroUsage(connectionID string, quotas map[string]any) {
	if connectionID == "" || len(quotas) == 0 {
		return
	}
	now := kiroNow().UnixMilli()
	var resetAt int64
	exhausted := false
	for _, raw := range quotas {
		bucket, ok := raw.(map[string]any)
		if !ok || kiroUnlimited(bucket) {
			continue
		}
		total, _ := kiroNumber(bucket["total"])
		used, _ := kiroNumber(bucket["used"])
		remaining, hasRemaining := kiroNumber(bucket["remaining"])
		if !hasRemaining {
			remaining = total - used
		}
		bucketReset := kiroResetMillis(bucket["resetAt"])
		if total > 0 && remaining <= 0 {
			exhausted = true
			if bucketReset > 0 && (resetAt == 0 || bucketReset < resetAt) {
				resetAt = bucketReset
			}
		}
	}
	if resetAt == 0 {
		for _, raw := range quotas {
			bucket, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if t := kiroResetMillis(bucket["resetAt"]); t > now && (resetAt == 0 || t < resetAt) {
				resetAt = t
			}
		}
	}
	entry := kiroCapEntry{Exhausted: exhausted, ResetAt: resetAt, CheckedAt: now, Source: "usage"}
	if exhausted && resetAt > 0 {
		entry.CappedUntil = resetAt
	}
	kiroCapMu.Lock()
	kiroCapCache[connectionID] = entry
	kiroCapMu.Unlock()
	kiroPersist(connectionID, entry)
}

// ShouldSkipKiroAccount reports whether the account must stay out of rotation.
// skip=false means "no opinion", never "healthy".
//
// A cap still in effect is authoritative: the probe TTL only expires an entry
// whose window could not be confirmed. Clearing here on age alone re-admitted a
// monthly-capped account five minutes after the 402, and the router then burned
// a request per model on it for the rest of the month.
func ShouldSkipKiroAccount(connectionID string) (skip bool, until time.Time, reason string) {
	now := kiroNow()
	kiroCapMu.Lock()
	defer kiroCapMu.Unlock()
	entry, ok := kiroCapCache[connectionID]
	if !ok {
		return false, time.Time{}, ""
	}
	if entry.Exhausted && entry.CappedUntil > now.UnixMilli() {
		return true, time.UnixMilli(entry.CappedUntil).UTC(), "monthly cap reached"
	}
	if entry.CappedUntil > 0 && entry.CappedUntil <= now.UnixMilli() {
		// The reset has passed: the cap is over, drop the entry.
		delete(kiroCapCache, connectionID)
		return false, time.Time{}, ""
	}
	if now.UnixMilli()-entry.CheckedAt > kiroCapProbeTTL.Milliseconds() {
		return false, time.Time{}, ""
	}
	return false, time.Time{}, ""
}

// MarkKiroCapped marks an account capped from a 402/429 body, using the usage
// API's reset when it is known and the calendar boundary of the next UTC month
// otherwise.
func MarkKiroCapped(connectionID string, resetAt *time.Time) {
	if connectionID == "" {
		return
	}
	now := kiroNow()
	until := nextUTCMonthStart(now)
	if resetAt != nil && !resetAt.IsZero() {
		until = resetAt.UTC()
	} else {
		kiroCapMu.RLock()
		existing, ok := kiroCapCache[connectionID]
		kiroCapMu.RUnlock()
		if ok && existing.ResetAt > now.UnixMilli() {
			until = time.UnixMilli(existing.ResetAt).UTC()
		}
	}
	source := "calendar"
	if resetAt != nil && !resetAt.IsZero() {
		source = "usage"
	}
	entry := kiroCapEntry{
		Exhausted:   true,
		CappedUntil: until.UnixMilli(),
		ResetAt:     until.UnixMilli(),
		CheckedAt:   now.UnixMilli(),
		Source:      source,
	}
	kiroCapMu.Lock()
	kiroCapCache[connectionID] = entry
	kiroCapMu.Unlock()
	kiroPersist(connectionID, entry)
}

// nextUTCMonthStart is the calendar fallback: the first instant of the next UTC
// month, mirroring Date.UTC(year, month+1, 1).
func nextUTCMonthStart(now time.Time) time.Time {
	utc := now.UTC()
	return time.Date(utc.Year(), utc.Month()+1, 1, 0, 0, 0, 0, time.UTC)
}

// KiroMonthlyVerdict reports whether a failure is Kiro's monthly cap rather than
// a throttle. The body is `{"message":"You have reached the limit.","reason":
// "MONTHLY_REQUEST_COUNT"}` on a 402 (or a 429 from a gateway in front of it),
// and the words carry no "quota" keyword — which is why it fell through to the
// generic two-hour cooldown and the account was re-dialled every couple of
// minutes for the rest of the month (upstream auth.js:416-419).
func KiroMonthlyVerdict(provider string, status int, errorText string) bool {
	if providers.ResolveAlias(provider) != "kiro" {
		return false
	}
	if status != http.StatusPaymentRequired && status != http.StatusTooManyRequests {
		return false
	}
	lower := strings.ToLower(errorText)
	return strings.Contains(lower, "monthly_request_count") ||
		strings.Contains(lower, "you have reached the limit")
}

// KiroMonthlyReset is when a capped account comes back: the usage API's
// nextDateReset when a reading already knows it, else the first instant of the
// next UTC month.
func KiroMonthlyReset(connectionID string, now time.Time) time.Time {
	kiroCapMu.RLock()
	entry, ok := kiroCapCache[connectionID]
	kiroCapMu.RUnlock()
	if ok && entry.ResetAt > now.UnixMilli() {
		return time.UnixMilli(entry.ResetAt).UTC()
	}
	return nextUTCMonthStart(now)
}

// kiroUnlimited reads the bucket's unlimited flag, which means "no cap to hit".
func kiroUnlimited(bucket map[string]any) bool {
	switch v := bucket["unlimited"].(type) {
	case bool:
		return v
	}
	return false
}

// kiroNumber reads a numeric field, accepting the protobuf-json {val: n}
// envelope Kiro uses in some responses.
func kiroNumber(value any) (float64, bool) {
	switch n := value.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case string:
		if f, err := strconv.ParseFloat(n, 64); err == nil {
			return f, true
		}
	case map[string]any:
		if inner, ok := n["val"]; ok {
			return kiroNumber(inner)
		}
	}
	return 0, false
}

// kiroResetMillis reads an ISO reset into unix milliseconds, 0 when absent or
// unparseable.
func kiroResetMillis(value any) int64 {
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
