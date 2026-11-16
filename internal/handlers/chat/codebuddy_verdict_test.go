package chat

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"panrouter/proxy/internal/db"
	"panrouter/proxy/internal/providers"
)

// codebuddyCompletionJSON is the healthy answer the second account serves.
const codebuddyCompletionJSON = `{"id":"chatcmpl-1","object":"chat.completion","created":0,` +
	`"model":"claude-opus-4-6","choices":[{"index":0,` +
	`"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],` +
	`"usage":{"prompt_tokens":5,"completion_tokens":7,"total_tokens":12}}`

// CodeBuddy's two non-cooldown verdicts, through the real fallback loop.
//
// Ported from open-sse/executors/codebuddy-intl.js parseError and
// src/sse/handlers/chat.js: a request-level refusal must not lock or rotate an
// account, and a provider ban must park the account while the rest of the pool
// keeps its chance.

// TestHandleAccountFallback_RequestLevelRefusalDoesNotRotate pins 11128: the
// account is healthy and the request shape is not, so the second account must not
// be dialled and no lock may be written. Before this, the refusal escalated to a
// 1800s model lock on a healthy account.
func TestHandleAccountFallback_RequestLevelRefusalDoesNotRotate(t *testing.T) {
	var first, second atomic.Int32
	var paths []string
	upstreamA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		first.Add(1)
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"code":11128,"msg":"request illegal"}`))
	}))
	t.Cleanup(func() { t.Logf("upstreamA paths: %v", paths) })
	defer upstreamA.Close()
	upstreamB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		second.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(codebuddyCompletionJSON))
	}))
	defer upstreamB.Close()

	database, cleanup := setupChatTestDB(t)
	defer cleanup()
	if _, err := database.Exec(`DELETE FROM providerConnections`); err != nil {
		t.Fatalf("clear seeded connections: %v", err)
	}
	seedConnDB(t, database, "codebuddy-intl", "cb-intl-a", "sk-a", upstreamA.URL)
	seedConnDB(t, database, "codebuddy-intl", "cb-intl-b", "sk-b", upstreamB.URL)

	repo := db.NewRepo(database)
	h := NewChatHandler(repo)

	rec := httptest.NewRecorder()
	err := h.handleAccountFallback(context.Background(), rec, "codebuddy-intl", "claude-opus-4-6", "",
		[]byte(`{"model":"claude-opus-4-6","messages":[{"role":"user","content":"hi"}]}`),
		false, false, "/v1/chat/completions")
	if err == nil {
		t.Fatal("a refused request must surface, not be answered by another account")
	}
	if got := first.Load(); got != 1 {
		t.Errorf("first account dialled %d times, want 1", got)
	}
	if got := second.Load(); got != 0 {
		t.Errorf("second account dialled %d times, want 0 — a request-level refusal fails on every account", got)
	}

	locked, lerr := repo.IsConnectionModelLocked("cb-intl-a", "claude-opus-4-6")
	if lerr != nil {
		t.Fatalf("IsConnectionModelLocked: %v", lerr)
	}
	if locked {
		t.Error("a request-level refusal must not lock the healthy account")
	}
	var raw string
	if qerr := database.QueryRow(`SELECT data FROM providerConnections WHERE id = 'cb-intl-a'`).Scan(&raw); qerr != nil {
		t.Fatalf("read connection data: %v", qerr)
	}
	if until, ok := db.ConnectionCooldownUntil(raw); ok {
		t.Errorf("account parked until %v on a request-level refusal: %s", until, raw)
	}
}

// TestHandleAccountFallback_AccountBanParksAndKeepsRotating pins 11140: the
// account is parked for the ban window, the NEXT account is dialled, and the
// request still succeeds. Stopping the rotation after a ban failed a request the
// pool could serve.
func TestHandleAccountFallback_AccountBanParksAndKeepsRotating(t *testing.T) {
	var first, second atomic.Int32
	upstreamA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		first.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"code":11140,"msg":"request illegal","displayMsg":{"en":"Your account has been restricted due to unusual activity"}}`))
	}))
	defer upstreamA.Close()
	upstreamB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		second.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(codebuddyCompletionJSON))
	}))
	defer upstreamB.Close()

	database, cleanup := setupChatTestDB(t)
	defer cleanup()
	if _, err := database.Exec(`DELETE FROM providerConnections`); err != nil {
		t.Fatalf("clear seeded connections: %v", err)
	}
	seedConnDB(t, database, "codebuddy-intl", "cb-ban-a", "sk-a", upstreamA.URL)
	seedConnDB(t, database, "codebuddy-intl", "cb-ban-b", "sk-b", upstreamB.URL)

	repo := db.NewRepo(database)
	h := NewChatHandler(repo)

	started := time.Now()
	rec := httptest.NewRecorder()
	err := h.handleAccountFallback(context.Background(), rec, "codebuddy-intl", "claude-opus-4-6", "",
		[]byte(`{"model":"claude-opus-4-6","messages":[{"role":"user","content":"hi"}]}`),
		false, false, "/v1/chat/completions")
	if err != nil {
		t.Fatalf("the pool had a healthy account, so the turn must succeed: %v", err)
	}
	if got := second.Load(); got != 1 {
		t.Errorf("second account dialled %d times, want 1 — a ban must not end the rotation", got)
	}
	// The rotation is spaced (upstream ACCOUNT_ROTATION_DELAY_MS): answering a ban
	// by dialling the next account in the same millisecond is the automation
	// pattern the ban was issued for.
	if elapsed := time.Since(started); elapsed < accountRotationDelay {
		t.Errorf("rotation took %v, want at least %v between a ban and the next account", elapsed, accountRotationDelay)
	}

	var raw string
	if qerr := database.QueryRow(`SELECT data FROM providerConnections WHERE id = 'cb-ban-a'`).Scan(&raw); qerr != nil {
		t.Fatalf("read connection data: %v", qerr)
	}
	if !strings.Contains(raw, "bannedUntil") || !strings.Contains(raw, "unusual activity") {
		t.Errorf("the ban was not recorded with its reason: %s", raw)
	}
	// The dashboard renders lastError and errorCode, so a ban has to carry both —
	// otherwise the operator sees an account in error with no cause.
	if !strings.Contains(raw, "Banned by the provider until") {
		t.Errorf("the ban is not visible to the dashboard: %s", raw)
	}
	if !strings.Contains(raw, `"errorCode":403`) {
		t.Errorf("the upstream status is missing from the row: %s", raw)
	}
	until, ok := db.ConnectionBlockedUntil(raw)
	if !ok {
		t.Fatalf("banned account is not excluded from rotation: %s", raw)
	}
	if wait := time.Until(until); wait < providers.BannedAccountCooldown-time.Minute {
		t.Errorf("banned for %v, want the %v ban window", wait, providers.BannedAccountCooldown)
	}
}
