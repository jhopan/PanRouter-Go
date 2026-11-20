package db

import (
	"context"
	"database/sql"
	json "encoding/json/v2"
	"fmt"
	"strings"
	"time"

	"panrouter/proxy/internal/providers"
)

const modelLockPrefix = "modelLock_"

// modelLockDataKey builds the JSON key used to store a model lock in the
// providerConnections.data blob. Matches Next.js flat field key format
// so dashboard can read modelLock_* fields across shared DB.
func modelLockDataKey(model string) string {
	// Quote the path segment so a model containing '.', '"', '[', ']' etc.
	// is treated as a literal key name, not JSON path syntax.
	return "$.\"" + modelLockPrefix + model + "\""
}

// LockConnectionModel stores a per-connection model lock in the
// providerConnections.data JSON blob using SQLite json_set().
// Also stores backoffLevel. Matches Next.js markAccountUnavailable.
// The stored data is readable by the shared Next.js dashboard.
func (r *Repo) LockConnectionModel(connID, model string, durationSec int, backoffLevel int) error {
	lockedUntil := time.Now().UTC().Add(time.Duration(durationSec) * time.Second).Format(time.RFC3339)
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := r.db.Exec(
		`UPDATE providerConnections SET data = json_set(data, ?, ?, '$.backoffLevel', ?), updatedAt = ? WHERE id = ?`,
		modelLockDataKey(model), lockedUntil, backoffLevel, now, connID,
	)
	if err != nil {
		return fmt.Errorf("lock connection model %s/%s: %w", connID, model, err)
	}
	return nil
}

// ConnectionCooldownUntil reads the account-scoped cooldown a connection
// carries in its data blob and when it ends: the whole-account quota park the
// chat path writes for a spent-budget verdict (quotaExhaustedUntil, upstream
// src/sse/services/auth.js:552) while it is still live, the generic account
// cooldown otherwise (rateLimitedUntil). Mirrors upstream
// filterAvailableAccounts (open-sse/services/accountFallback.js:180), which
// skips any account whose rateLimitedUntil is still in the future, plus
// buildRouterQuotaState (open-sse/services/routerQuotaState.js:56), which reads
// the quota field first because a quota refusal does not have to write a model
// lock or a generic cooldown at all: that field can be the only record of the
// park.
//
// Unlike the model lock this cooldown is not keyed by model, which is what
// catches a quota spent account-wide, and it is the only quota cooldown the
// selector can consult when the request carries no model at all. An account
// parked for a rejected OAuth grant carries its own timestamp instead; read
// both through ConnectionBlockedUntil.
//
// A missing, empty, unparseable or already lapsed value reports "not in
// cooldown" so neither a malformed field nor a stale one can move an account
// in or out of rotation.
func ConnectionCooldownUntil(rawData string) (time.Time, bool) {
	return ConnectionCooldownUntilAt(rawData, time.Now())
}

// ConnectionCooldownUntilAt is ConnectionCooldownUntil with the clock injected,
// so a test can place a park either side of "now" without waiting it out.
func ConnectionCooldownUntilAt(rawData string, now time.Time) (time.Time, bool) {
	raw, ok := parseConnData(rawData)
	if !ok {
		return time.Time{}, false
	}
	// A lapsed quota park is not a park: falling back rather than returning it
	// keeps a stale date from hiding a live generic cooldown behind it.
	if until, ok := readTimestampField(raw, "quotaExhaustedUntil"); ok && until.After(now) {
		return until, true
	}
	until, ok := readTimestampField(raw, "rateLimitedUntil")
	if !ok || !until.After(now) {
		return time.Time{}, false
	}
	return until, true
}

// RecordConnectionError stores the error message, status, and backoffLevel on
// the connection row for dashboard visibility without setting an account-level
// rateLimitedUntil cooldown.
func (r *Repo) RecordConnectionError(connID string, status int, errText string, backoffLevel int) error {
	return r.RecordConnectionScopedError(connID, "", "", status, errText, backoffLevel)
}

// RecordConnectionScopedError stores the error message, status, model, and source
// on the connection row for dashboard visibility without setting rateLimitedUntil.
func (r *Repo) RecordConnectionScopedError(connID, model, source string, status int, errText string, backoffLevel int) error {
	now := time.Now().UTC().Format(time.RFC3339)
	lastError := map[string]any{
		"status":    status,
		"message":   errText,
		"timestamp": now,
	}
	if model != "" {
		lastError["model"] = model
	}
	if source != "" {
		lastError["source"] = source
	}
	payload, err := json.Marshal(lastError)
	if err != nil {
		return fmt.Errorf("marshal last error for %s: %w", connID, err)
	}
	_, err = r.db.Exec(
		`UPDATE providerConnections
		 SET data = json_set(data,
		   '$.backoffLevel', ?,
		   '$.lastError', json(?),
		   '$.status', 'error'),
		     updatedAt = ?
		 WHERE id = ?`,
		backoffLevel, string(payload), now, connID,
	)
	if err != nil {
		return fmt.Errorf("record connection error %s: %w", connID, err)
	}
	return nil
}

// LockConnectionRateLimit stores the account-scoped cooldown and the error
// that caused it, mirroring upstream applyErrorState
// (open-sse/services/accountFallback.js:216). The dashboard already reads
// rateLimitedUntil to render "reset at", but nothing in the Go port ever
// wrote it, so that field was always empty and the selector had no
// account-scoped signal to skip on.
func (r *Repo) LockConnectionRateLimit(connID string, until time.Time, backoffLevel, status int, errText string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	lastError := map[string]any{
		"status":    status,
		"message":   errText,
		"timestamp": now,
	}
	payload, err := json.Marshal(lastError)
	if err != nil {
		return fmt.Errorf("marshal last error for %s: %w", connID, err)
	}
	_, err = r.db.Exec(
		`UPDATE providerConnections
		 SET data = json_set(data,
		   '$.rateLimitedUntil', ?,
		   '$.backoffLevel', ?,
		   '$.lastError', json(?),
		   '$.status', 'error'),
		     updatedAt = ?
		 WHERE id = ?`,
		until.UTC().Format(time.RFC3339), backoffLevel, string(payload), now, connID,
	)
	if err != nil {
		return fmt.Errorf("lock connection rate limit %s: %w", connID, err)
	}
	return nil
}

// ClearConnectionRateLimit drops the account-scoped cooldown after a request
// was served, mirroring upstream resetAccountState
// (open-sse/services/accountFallback.js:198) and clearAccountError
// (src/sse/services/auth.js:620-627). Without it a recovered account would stay
// skipped until the cooldown expired on its own. The per-model locks are
// deliberately left alone: a success on one model must not unlock a model that
// is still in cooldown.
//
// The quota park goes with it: a served request is the same proof of a usable
// account whether the park was a throttle or a spent budget, and upstream drops
// quotaExhaustedUntil in the same update.
func (r *Repo) ClearConnectionRateLimit(connID string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := r.db.Exec(
		`UPDATE providerConnections
		 SET data = json_set(data,
		   '$.rateLimitedUntil', NULL,
		   '$.quotaExhaustedUntil', NULL,
		   '$.lastError', NULL,
		   '$.status', 'active',
		   '$.backoffLevel', 0),
		     updatedAt = ?
		 WHERE id = ?`,
		now, connID,
	)
	if err != nil {
		return fmt.Errorf("clear connection rate limit %s: %w", connID, err)
	}
	return nil
}

// LockConnectionQuotaExhausted parks a connection whose provider reported the
// account's quota or credit budget is spent, mirroring the quota branch of
// upstream markAccountUnavailable (src/sse/services/auth.js:539-566).
//
// It writes the account cooldown LockConnectionRateLimit writes — the selector
// already skips on that — and beside it the fields upstream records for the same
// verdict: quotaExhaustedUntil, testStatus "unavailable", the
// "Quota exhausted — resets <iso>" message, errorCode, lastErrorAt and
// backoffLevel. One statement, one reset instant for all of them, because
// upstream is explicit that the lock, the error text and the persisted
// until-date agree: "the dashboard shows one date while the router unparks the
// account at another" is the bug that comment exists for.
//
// The park is whole-account on purpose. A spent budget refuses every model on
// the connection, so nothing else there is worth dialling until the window the
// provider named.
//
// The fields live in providerConnections.data because the shared Next.js
// dashboard reads the same blob (open-sse/services/routerQuotaState.js:56,
// src/shared/utils/cooldown.js:42); no column is added. The message is stored
// in this codebase's lastError shape — an object with a message field, which
// providers.errorTextOf flattens back to the bare string upstream writes — so
// every writer of this field stays readable by the same readers.
func (r *Repo) LockConnectionQuotaExhausted(connID string, until time.Time, backoffLevel, status int) error {
	now := time.Now().UTC()
	stamp := now.Format(time.RFC3339)
	resetAt := until.UTC().Format(time.RFC3339)
	lastError, err := json.Marshal(map[string]any{
		"status":    status,
		"message":   "Quota exhausted — resets " + resetAt,
		"timestamp": stamp,
	})
	if err != nil {
		return fmt.Errorf("marshal last error for %s: %w", connID, err)
	}
	_, err = r.db.Exec(
		`UPDATE providerConnections
		 SET data = json_set(data,
		   '$.rateLimitedUntil', ?,
		   '$.quotaExhaustedUntil', ?,
		   '$.backoffLevel', ?,
		   '$.lastError', json(?),
		   '$.errorCode', ?,
		   '$.lastErrorAt', ?,
		   '$.testStatus', ?,
		   '$.status', 'error'),
		     updatedAt = ?
		 WHERE id = ?`,
		resetAt, resetAt, backoffLevel, string(lastError), status, stamp,
		providers.StatusUnavailable, stamp, connID,
	)
	if err != nil {
		return fmt.Errorf("lock connection quota exhausted %s: %w", connID, err)
	}
	return nil
}

// ClearConnectionModelLocks drops every per-model lock a connection carries,
// leaving the rest of its data alone. A renewed credential makes the old
// locks moot: without this an account that was parked for a ban or a mute
// stays parked even after the operator re-authenticates.
func (r *Repo) ClearConnectionModelLocks(ctx context.Context, connID string) error {
	var rawData string
	if err := r.db.QueryRowContext(ctx, "SELECT data FROM providerConnections WHERE id = ?", connID).Scan(&rawData); err != nil {
		return fmt.Errorf("clear connection model locks %s: %w", connID, err)
	}
	var dataMap map[string]any
	if err := json.Unmarshal([]byte(rawData), &dataMap); err != nil {
		return fmt.Errorf("clear connection model locks %s: %w", connID, err)
	}
	changed := false
	for k := range dataMap {
		if strings.HasPrefix(k, modelLockPrefix) && dataMap[k] != nil {
			dataMap[k] = nil
			changed = true
		}
	}
	if !changed {
		return nil
	}
	encoded, err := json.Marshal(dataMap)
	if err != nil {
		return fmt.Errorf("clear connection model locks %s: %w", connID, err)
	}
	return r.UpdateConnectionData(connID, string(encoded))
}

// ConnectionModelLockUntil reads the expiry timestamp of a modelLock_<model>
// field directly from connection data without a DB round-trip, used by the
// connection selector to populate cooldownUntil for Retry-After calculation.
// Returns (zero, false) when no active lock exists.
func ConnectionModelLockUntil(data, model string) (time.Time, bool) {
	if data == "" || model == "" {
		return time.Time{}, false
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(data), &raw); err != nil {
		return time.Time{}, false
	}
	key := modelLockPrefix + model
	val, ok := raw[key]
	if !ok {
		return time.Time{}, false
	}
	str, ok := val.(string)
	if !ok || str == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, str)
	if err != nil {
		return time.Time{}, false
	}
	now := time.Now().UTC()
	if !now.Before(t) {
		return time.Time{}, false // lock has already expired
	}
	return t, true
}

// IsConnectionModelLocked checks whether the given connection has an active
// modelLock_<model> field in its data JSON blob. Returns true when the
// timestamp is in the future.
func (r *Repo) IsConnectionModelLocked(connID, model string) (bool, error) {
	var rawData string
	err := r.db.QueryRow("SELECT data FROM providerConnections WHERE id = ?", connID).Scan(&rawData)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("get connection data %s: %w", connID, err)
	}

	var raw map[string]any
	if err := json.Unmarshal([]byte(rawData), &raw); err != nil {
		return false, nil // malformed data — treat as unlocked
	}

	key := modelLockPrefix + model
	val, ok := raw[key]
	if !ok {
		return false, nil
	}
	str, ok := val.(string)
	if !ok || str == "" {
		return false, nil
	}
	t, err := time.Parse(time.RFC3339, str)
	if err != nil {
		return false, nil
	}
	return time.Now().UTC().Before(t), nil
}

// UnlockConnectionModel removes a per-connection model lock by setting
// modelLock_<model> to null and resetting backoffLevel to 0.
// Matches Next.js clearAccountError behavior.
func (r *Repo) UnlockConnectionModel(connID, model string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := r.db.Exec(
		`UPDATE providerConnections SET data = json_set(data, ?, json('null'), '$.backoffLevel', 0), updatedAt = ? WHERE id = ?`,
		modelLockDataKey(model), now, connID,
	)
	if err != nil {
		return fmt.Errorf("unlock connection model %s/%s: %w", connID, model, err)
	}
	return nil
}

// ReleaseConnectionBlocks lifts every block a provider-side quota read has
// disproved: the per-model locks, the ban fields, the account cooldown, the
// error code and the backoff counter. It is the writer behind
// POST /api/usage/{connectionId}/check-release, which exists because a plan
// block (a MiniMax plan holds for 24 hours) outlives a top-up or a daily
// check-in: the provider is serving the account again while every local flag
// still says exhausted.
//
// It is deliberately not "unblock on demand" — the caller verifies the quota
// with the provider first.
//
// testStatus is set to "active" rather than removed, matching upstream's
// clearAccountError path: the dashboard reads it as the account's health.
func (r *Repo) ReleaseConnectionBlocks(connID string) error {
	var rawData string
	if err := r.db.QueryRow("SELECT data FROM providerConnections WHERE id = ?", connID).Scan(&rawData); err != nil {
		return fmt.Errorf("release connection blocks %s: %w", connID, err)
	}
	dataMap := map[string]any{}
	if strings.TrimSpace(rawData) != "" {
		if err := json.Unmarshal([]byte(rawData), &dataMap); err != nil {
			return fmt.Errorf("release connection blocks %s: %w", connID, err)
		}
	}
	for k := range dataMap {
		if strings.HasPrefix(k, modelLockPrefix) {
			dataMap[k] = nil
		}
	}
	dataMap["bannedUntil"] = nil
	dataMap["banReason"] = nil
	dataMap["rateLimitedUntil"] = nil
	dataMap["oauthLockedUntil"] = nil
	dataMap["quotaExhaustedUntil"] = nil
	dataMap["errorCode"] = nil
	dataMap["lastError"] = nil
	dataMap["lastErrorAt"] = nil
	dataMap["testStatus"] = "active"
	dataMap["backoffLevel"] = 0

	encoded, err := json.Marshal(dataMap)
	if err != nil {
		return fmt.Errorf("release connection blocks %s: %w", connID, err)
	}
	if _, err := r.db.Exec(
		"UPDATE providerConnections SET data = ?, updatedAt = ? WHERE id = ?",
		string(encoded), nowUTC(), connID,
	); err != nil {
		return fmt.Errorf("release connection blocks %s: %w", connID, err)
	}
	return nil
}

// ResetConnectionHealthState clears model locks, error codes, rate limits, and resets backoff
// on a connection when activated or re-validated (parity with #3830 / #7fee56ba).
func (r *Repo) ResetConnectionHealthState(connID string) error {
	var rawData string
	err := r.db.QueryRow("SELECT data FROM providerConnections WHERE id = ?", connID).Scan(&rawData)
	if err != nil {
		return err
	}
	var dataMap map[string]any
	if err := json.Unmarshal([]byte(rawData), &dataMap); err != nil {
		return err
	}

	dataMap["errorCode"] = nil
	dataMap["rateLimitedUntil"] = nil
	dataMap["oauthLockedUntil"] = nil
	// The quota park is a lock like the other two: leaving it behind would keep
	// an account the operator just reset out of rotation until the provider's own
	// window passes, which is exactly what the reset button is for (the Claude
	// and Codex credit-reset panels call this).
	dataMap["quotaExhaustedUntil"] = nil
	dataMap["oauthFailureCount"] = 0
	dataMap["backoffLevel"] = 0

	for k := range dataMap {
		if strings.HasPrefix(k, modelLockPrefix) {
			dataMap[k] = nil
		}
	}

	newBytes, err := json.Marshal(dataMap)
	if err != nil {
		return err
	}

	now := time.Now().UTC().Format(time.RFC3339)
	_, err = r.db.Exec("UPDATE providerConnections SET data = ?, updatedAt = ? WHERE id = ?", string(newBytes), now, connID)
	return err
}

// GetConnectionBackoffLevel reads the backoffLevel from a connection's data.
// Returns 0 when not set or on error.
func (r *Repo) GetConnectionBackoffLevel(connID string) int {
	var rawData string
	err := r.db.QueryRow("SELECT data FROM providerConnections WHERE id = ?", connID).Scan(&rawData)
	if err != nil {
		return 0
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(rawData), &raw); err != nil {
		return 0
	}
	level, ok := raw["backoffLevel"].(float64)
	if !ok {
		return 0
	}
	return int(level)
}

// IsProviderAvailable returns true if at least one active connection for the
// given provider has no active modelLock_<model>. This is the connection-based
// replacement for the old kv-based IsProviderHealthy, matching Next.js's
// isModelLockActive + filterAvailableAccounts flow.
// Returns true when no connections exist (optimistic default for no-auth providers).
func (r *Repo) IsProviderAvailable(provider, model string) bool {
	if provider == "" {
		return true
	}
	conns, err := r.GetProviderConnections(provider, true)
	if err != nil || len(conns) == 0 {
		return true
	}
	for _, c := range conns {
		locked, err := r.IsConnectionModelLocked(c.ID, model)
		if err == nil && !locked {
			return true
		}
	}
	return false
}

// ResetProviderHealth clears modelLock_* fields on connections, matching
// Next.js clearAccountError semantics.
//   - provider="" and model="" → all connections
//   - model="" → all connections for provider
//   - both set → specific provider + model on all its connections
func (r *Repo) ResetProviderHealth(provider, model string) error {
	if provider == "" && model == "" {
		return r.clearAllModelLocks()
	}
	if model == "" {
		return r.clearProviderModelLocks(provider)
	}
	return r.clearSpecificModelLock(provider, model)
}

// listConnectionData drains a providerConnections id/data query and closes
// its rows before returning. The clear*ModelLocks helpers write per row
// (clearConnectionModelLocks / Exec), and a nested pool checkout while the
// SELECT rows are still open can deadlock the pool the same way
// GetProviderConnections did: the writer waits for a connection the open
// rows will never release. Collect first, then write.
func (r *Repo) listConnectionData(query string, args ...any) ([]struct{ id, data string }, error) {
	rows, err := r.db.QueryContext(context.Background(), query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []struct{ id, data string }
	for rows.Next() {
		var row struct{ id, data string }
		if err := rows.Scan(&row.id, &row.data); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *Repo) clearAllModelLocks() error {
	pairs, err := r.listConnectionData("SELECT id, data FROM providerConnections")
	if err != nil {
		return err
	}
	for _, p := range pairs {
		if err := r.clearConnectionModelLocks(p.id, p.data); err != nil {
			return err
		}
	}
	return nil
}

func (r *Repo) clearProviderModelLocks(provider string) error {
	pairs, err := r.listConnectionData("SELECT id, data FROM providerConnections WHERE provider = ?", provider)
	if err != nil {
		return err
	}
	for _, p := range pairs {
		if err := r.clearConnectionModelLocks(p.id, p.data); err != nil {
			return err
		}
	}
	return nil
}

func (r *Repo) clearSpecificModelLock(provider, model string) error {
	key := modelLockDataKey(model)
	pairs, err := r.listConnectionData("SELECT id, data FROM providerConnections WHERE provider = ?", provider)
	if err != nil {
		return err
	}
	for _, p := range pairs {
		locked := checkConnectionModelLock(p.data, model)
		if !locked {
			continue
		}
		if _, err := r.db.Exec(
			`UPDATE providerConnections SET data = json_set(data, ?, json('null'), '$.backoffLevel', 0), updatedAt = datetime('now') WHERE id = ?`,
			key, p.id,
		); err != nil {
			return fmt.Errorf("clear lock %s on %s: %w", key, p.id, err)
		}
	}
	return nil
}

func (r *Repo) clearConnectionModelLocks(id, data string) error {
	fields := listModelLockJSONKeys(data)
	if len(fields) == 0 {
		return nil
	}
	q := "UPDATE providerConnections SET data = json_set(data"
	args := make([]any, 0, len(fields)*2+2)
	for _, f := range fields {
		q += ", ?, json('null')"
		args = append(args, "$.\""+f+"\"")
	}
	q += ", '$.backoffLevel', 0), updatedAt = datetime('now') WHERE id = ?"
	args = append(args, id)
	_, err := r.db.Exec(q, args...)
	return err
}

// checkConnectionModelLock parses connection data to check if modelLock_<model> is active.
func checkConnectionModelLock(data string, model string) bool {
	var raw map[string]any
	if err := json.Unmarshal([]byte(data), &raw); err != nil {
		return false
	}
	key := modelLockPrefix + model
	val, ok := raw[key]
	if !ok {
		return false
	}
	str, ok := val.(string)
	if !ok || str == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, str)
	if err != nil {
		return false
	}
	return time.Now().UTC().Before(t)
}

// listModelLockJSONKeys returns all top-level JSON keys starting with modelLockPrefix.
func listModelLockJSONKeys(data string) []string {
	var raw map[string]any
	if err := json.Unmarshal([]byte(data), &raw); err != nil {
		return nil
	}
	var keys []string
	for k := range raw {
		if len(k) > len(modelLockPrefix) && k[:len(modelLockPrefix)] == modelLockPrefix {
			keys = append(keys, k)
		}
	}
	return keys
}
