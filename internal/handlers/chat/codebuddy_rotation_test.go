package chat

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"panrouter/proxy/internal/db"
)

// codeBuddyFrequencyLimitBody is the 429 the provider answers with: code 6004
// plus the instant its quota refills, in its own offset.
func codeBuddyFrequencyLimitBody(resetIn time.Duration) string {
	cst := time.FixedZone("CST", 8*3600)
	resetAt := time.Now().In(cst).Add(resetIn).Format("2006-01-02 15:04:05")
	return fmt.Sprintf(`{"code":6004,"msg":"超出频率限制,恢复时间 %s UTC+8"}`, resetAt)
}

// TestHandleAccountFallback_CodeBuddyRateLimitStopsRotation pins upstream's
// rule (AGENTS.md §1): a 429 on CodeBuddy Intl gets ZERO retries, the account is
// locked until its own reset plus the ten-minute margin, and the rotation
// stops.
//
// Walking the pool here is what produced the ban: one quota refusal answered by
// 29 requests across 29 accounts in two seconds is the automation signature the
// provider reads as "restricted due to unusual activity". The control half of
// the test proves the rule is provider-scoped — another provider's 429 still
// moves to the next account, where that account is a real second chance.
func TestHandleAccountFallback_CodeBuddyRateLimitStopsRotation(t *testing.T) {
	newUpstream := func(counter *atomic.Int32) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			counter.Add(1)
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(codeBuddyFrequencyLimitBody(30 * time.Minute)))
		}))
	}

	t.Run("codebuddy-intl stops after the first refusal", func(t *testing.T) {
		var first, second atomic.Int32
		upstreamA, upstreamB := newUpstream(&first), newUpstream(&second)
		defer upstreamA.Close()
		defer upstreamB.Close()

		database, cleanup := setupChatTestDB(t)
		defer cleanup()
		if _, err := database.Exec(`DELETE FROM providerConnections`); err != nil {
			t.Fatalf("clear seeded connections: %v", err)
		}
		seedConnDB(t, database, "codebuddy-intl", "cb-first", "sk-first", upstreamA.URL)
		seedConnDB(t, database, "codebuddy-intl", "cb-second", "sk-second", upstreamB.URL)

		repo := db.NewRepo(database)
		h := NewChatHandler(repo)

		rec := httptest.NewRecorder()
		err := h.handleAccountFallback(context.Background(), rec, "codebuddy-intl", "claude-opus-4-6", "",
			[]byte(`{"model":"claude-opus-4-6","messages":[{"role":"user","content":"hi"}]}`),
			false, false, "/v1/chat/completions")
		if err == nil {
			t.Fatal("expected the 429 to surface")
		}
		if got := first.Load(); got != 1 {
			t.Errorf("first account dialled %d times, want 1", got)
		}
		if got := second.Load(); got != 0 {
			t.Errorf("second account dialled %d times, want 0 — the rotation must stop", got)
		}

		locked, lerr := repo.IsConnectionModelLocked("cb-first", "claude-opus-4-6")
		if lerr != nil {
			t.Fatalf("IsConnectionModelLocked: %v", lerr)
		}
		if !locked {
			t.Error("the refused account must be locked on the model before the pass ends")
		}

		// The lock is the announced reset plus the margin, not the classifier's
		// two-second floor: the stored cooldown must name a window about half an
		// hour out (30m + 10m).
		var raw string
		if qerr := database.QueryRow(
			`SELECT data FROM providerConnections WHERE id = 'cb-first'`).Scan(&raw); qerr != nil {
			t.Fatalf("read connection data: %v", qerr)
		}
		until, ok := db.ConnectionCooldownUntil(raw)
		if !ok {
			t.Fatalf("no rateLimitedUntil recorded on the refused account: %s", raw)
		}
		if wait := time.Until(until); wait < 35*time.Minute || wait > 45*time.Minute {
			t.Errorf("locked for %v, want the announced 30m plus the 10m margin", wait)
		}
	})

	t.Run("another provider still rotates to the next account", func(t *testing.T) {
		var first, second atomic.Int32
		upstreamA, upstreamB := newUpstream(&first), newUpstream(&second)
		defer upstreamA.Close()
		defer upstreamB.Close()

		database, cleanup := setupChatTestDB(t)
		defer cleanup()
		if _, err := database.Exec(`DELETE FROM providerConnections`); err != nil {
			t.Fatalf("clear seeded connections: %v", err)
		}
		seedConnDB(t, database, "deepseek", "ds-first", "sk-first", upstreamA.URL)
		seedConnDB(t, database, "deepseek", "ds-second", "sk-second", upstreamB.URL)

		repo := db.NewRepo(database)
		h := NewChatHandler(repo)

		rec := httptest.NewRecorder()
		_ = h.handleAccountFallback(context.Background(), rec, "deepseek", "deepseek-chat", "",
			[]byte(`{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}]}`),
			false, false, "/v1/chat/completions")

		if got := first.Load(); got != 1 {
			t.Errorf("first account dialled %d times, want 1", got)
		}
		if got := second.Load(); got != 1 {
			t.Errorf("second account dialled %d times, want 1 — the pool walk is other providers' behaviour", got)
		}
	})
}

// TestStopsRotationOnRateLimit pins the predicate itself, including the
// spellings a stored connection row may carry.
func TestStopsRotationOnRateLimit(t *testing.T) {
	rateLimited := &upstreamError{StatusCode: http.StatusTooManyRequests, Body: []byte(`{"code":6004}`)}
	serverError := &upstreamError{StatusCode: http.StatusInternalServerError}

	tests := []struct {
		name     string
		provider string
		ue       *upstreamError
		want     bool
	}{
		{name: "intl codebuddy 429", provider: "codebuddy-intl", ue: rateLimited, want: true},
		{name: "intl by alias", provider: "cbai", ue: rateLimited, want: true},
		{name: "the CN site keeps rotating", provider: "codebuddy-cn", ue: rateLimited},
		{name: "another provider keeps rotating", provider: "deepseek", ue: rateLimited},
		{name: "a 5xx is not a rate limit", provider: "codebuddy-intl", ue: serverError},
		{name: "no classified error", provider: "codebuddy-intl", ue: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stopsRotationOnRateLimit(tt.provider, tt.ue); got != tt.want {
				t.Errorf("stopsRotationOnRateLimit(%q) = %v, want %v", tt.provider, got, tt.want)
			}
		})
	}
}
