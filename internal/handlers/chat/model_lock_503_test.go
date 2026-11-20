package chat

// Tests for the 502→503+Retry-After fix when all connections carry an active
// per-model lock (parity with upstream auth.js allRateLimited → 503).
//
// Before the fix, handleAccountFallback returned a plain fmt.Errorf when
// IsProviderAvailable returned false. chat.go rendered any non-*upstreamError
// as 502 Bad Gateway. The correct contract is 503 Service Unavailable with a
// Retry-After header whose value is the remaining cooldown in seconds.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"panrouter/proxy/internal/db"
	"panrouter/proxy/internal/models"
)

// insertConnWithModelLock inserts an active connection for provider whose data
// blob carries an active modelLock_<model> timestamp.
func insertConnWithModelLock(t *testing.T, database *sql.DB, provider, connID, model string, lockUntil time.Time) {
	t.Helper()
	data := fmt.Sprintf(`{"apiKey":"sk-test","modelLock_%s":"%s"}`,
		model, lockUntil.UTC().Format(time.RFC3339))
	q := `INSERT INTO providerConnections (id, provider, authType, name, priority, isActive, data, createdAt, updatedAt)
		 VALUES (?, ?, 'apikey', 'Test', 1, 1, ?, '2026-07-18T00:00:00Z', '2026-07-18T00:00:00Z')`
	if _, err := database.Exec(q, connID, provider, data); err != nil {
		t.Fatalf("insertConnWithModelLock %s: %v", connID, err)
	}
}

// makeConnWithData constructs a minimal in-memory *models.ProviderConnection
// with a given ID, provider, and raw JSON data blob for unit-testing helpers
// that operate on connection data without a DB round-trip.
func makeConnWithData(id, provider, data string) *models.ProviderConnection {
	return &models.ProviderConnection{
		ID:       id,
		Provider: provider,
		IsActive: 1,
		Data:     data,
	}
}

// TestAllConnectionsModelLocked_Returns503WithRetryAfter is the primary
// regression test for the 502→503 fix.
//
// When every connection for a provider/model carries an active modelLock_<model>
// timestamp, handleAccountFallback must:
//   - return an *upstreamError (not a plain error)
//   - with StatusCode 503 (Service Unavailable)
//   - with a Retry-After header whose value is > 0 seconds
//   - and the response body carries a rate_limit_error type
//
// This pins the upstream contract (chat.js:326-330 → unavailableResponse(503)).
func TestAllConnectionsModelLocked_Returns503WithRetryAfter(t *testing.T) {
	database, cleanup := setupChatTestDB(t)
	defer cleanup()

	// Remove the shared seeded conn so we control the full candidate set.
	dropSeededConn(t, database)

	lockUntil := time.Now().UTC().Add(5 * time.Minute)
	insertConnWithModelLock(t, database, "deepseek", "conn-locked-1", "deepseek-chat", lockUntil)
	insertConnWithModelLock(t, database, "deepseek", "conn-locked-2", "deepseek-chat", lockUntil)

	h := NewChatHandler(db.NewRepo(database))
	body := []byte(`{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}]}`)
	rec := httptest.NewRecorder()

	err := h.handleAccountFallback(context.Background(), rec, "deepseek", "deepseek-chat", "", body, false, false, "/v1/chat/completions")
	if err == nil {
		t.Fatal("expected error when all connections have active model locks, got nil")
	}

	var ue *upstreamError
	if !errors.As(err, &ue) {
		t.Fatalf("expected *upstreamError (not plain error — that would render as 502), got %T: %v", err, err)
	}
	if ue.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("StatusCode = %d, want %d (503 Service Unavailable)", ue.StatusCode, http.StatusServiceUnavailable)
	}
	ra := ue.Header.Get("Retry-After")
	if ra == "" {
		t.Error("Retry-After header missing from 503 response")
	} else {
		secs, parseErr := strconv.Atoi(ra)
		if parseErr != nil {
			t.Errorf("Retry-After %q is not an integer: %v", ra, parseErr)
		} else if secs <= 0 {
			t.Errorf("Retry-After = %d, want > 0", secs)
		}
	}
	if ct := ue.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if len(ue.Body) == 0 {
		t.Error("body is empty")
	}
}

// TestAllConnectionsModelLocked_SingleConn verifies the same fix with a single
// connection: even one locked connection triggers 503, not 502.
func TestAllConnectionsModelLocked_SingleConn(t *testing.T) {
	database, cleanup := setupChatTestDB(t)
	defer cleanup()

	dropSeededConn(t, database)

	lockUntil := time.Now().UTC().Add(2 * time.Minute)
	insertConnWithModelLock(t, database, "deepseek", "conn-only", "deepseek-chat", lockUntil)

	h := NewChatHandler(db.NewRepo(database))
	body := []byte(`{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}]}`)
	rec := httptest.NewRecorder()

	err := h.handleAccountFallback(context.Background(), rec, "deepseek", "deepseek-chat", "", body, false, false, "/v1/chat/completions")
	if err == nil {
		t.Fatal("expected error when the only connection has an active model lock")
	}

	var ue *upstreamError
	if !errors.As(err, &ue) {
		t.Fatalf("expected *upstreamError (not plain error — that would render as 502), got %T — this is the 502 bug", err)
	}
	if ue.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("StatusCode = %d, want 503", ue.StatusCode)
	}
}

// TestAllConnectionsModelLocked_ExpiredLockNotBlocked verifies that an already-
// expired model lock does NOT trigger the early-out: the connection should be
// treated as healthy, and handleAccountFallback proceeds to the loop.
func TestAllConnectionsModelLocked_ExpiredLockNotBlocked(t *testing.T) {
	database, cleanup := setupChatTestDB(t)
	defer cleanup()

	dropSeededConn(t, database)

	// Lock timestamp in the past — already expired.
	expiredUntil := time.Now().UTC().Add(-1 * time.Minute)
	insertConnWithModelLock(t, database, "deepseek", "conn-free", "deepseek-chat", expiredUntil)

	h := NewChatHandler(db.NewRepo(database))
	body := []byte(`{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}]}`)
	rec := httptest.NewRecorder()

	err := h.handleAccountFallback(context.Background(), rec, "deepseek", "deepseek-chat", "", body, false, false, "/v1/chat/completions")
	// We expect a network error (no server listening), NOT a 503 upstreamError
	// — the expired lock must not block the healthy connection.
	if err == nil {
		t.Fatal("expected error (no upstream server), got nil")
	}
	var ue *upstreamError
	if errors.As(err, &ue) && ue.StatusCode == http.StatusServiceUnavailable {
		t.Error("got 503 for an expired model lock — the connection should not have been blocked")
	}
}

// TestConnectionModelBlockedUntil_ReturnsExpiry verifies the helper correctly
// reads the lock expiry from connection data for Retry-After calculation.
func TestConnectionModelBlockedUntil_ReturnsExpiry(t *testing.T) {
	database, cleanup := setupChatTestDB(t)
	defer cleanup()

	h := NewChatHandler(db.NewRepo(database))

	lockUntil := time.Now().UTC().Add(90 * time.Second).Truncate(time.Second)
	conn := makeConnWithData("conn-lock-test", "deepseek", fmt.Sprintf(
		`{"apiKey":"sk-test","modelLock_deepseek-chat":"%s"}`, lockUntil.Format(time.RFC3339)))

	blocked, until := h.connectionModelBlockedUntil(conn, "deepseek", "deepseek-chat")
	if !blocked {
		t.Fatal("connectionModelBlockedUntil = false, want true")
	}
	if until.IsZero() {
		t.Fatal("until is zero, want the lock expiry timestamp")
	}
	diff := until.Sub(lockUntil)
	if diff < -time.Second || diff > time.Second {
		t.Errorf("until = %v, want ~%v (diff %v)", until, lockUntil, diff)
	}
}

// TestConnectionModelBlockedUntil_ExpiredReturnsNotBlocked verifies that an
// expired lock is not reported as blocked.
func TestConnectionModelBlockedUntil_ExpiredReturnsNotBlocked(t *testing.T) {
	database, cleanup := setupChatTestDB(t)
	defer cleanup()

	h := NewChatHandler(db.NewRepo(database))

	expiredUntil := time.Now().UTC().Add(-30 * time.Second)
	conn := makeConnWithData("conn-expired", "deepseek", fmt.Sprintf(
		`{"apiKey":"sk-test","modelLock_deepseek-chat":"%s"}`, expiredUntil.Format(time.RFC3339)))

	blocked, until := h.connectionModelBlockedUntil(conn, "deepseek", "deepseek-chat")
	if blocked {
		t.Error("connectionModelBlockedUntil = true for expired lock, want false")
	}
	if !until.IsZero() {
		t.Errorf("until = %v for expired lock, want zero", until)
	}
}
