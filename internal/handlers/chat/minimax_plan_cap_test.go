// MiniMax Code plan exhaustion — parity with open-sse/config/errorConfig.js:120-129
// and tests/unit/minimax-plan-cap.test.js.
//
// The plan window announces a reset, but the operator asked for a flat 24-hour
// block: a plan that ran out while 470k tokens of conversation were being sent is
// not worth retrying in a few hours, and every retry is a real request.
package chat

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"panrouter/proxy/internal/db"
	"panrouter/proxy/internal/providers"
)

// TestMinimaxPlanCapIs24Hours pins the flat block for both regions, and that no
// other provider inherits it.
func TestMinimaxPlanCapIs24Hours(t *testing.T) {
	body := `{"error":{"message":"usage limit reached: insufficient balance (1008)"}}`

	for _, provider := range []string{"minimax-code", "minimax-code-global"} {
		cls := providers.ClassifyErrorForProvider(http.StatusTooManyRequests, body, 0, provider)
		if !cls.Quota {
			t.Errorf("%s: classified %+v, want a quota verdict", provider, cls)
		}
		if got, want := cls.CooldownMs, int(24*time.Hour/time.Millisecond); got != want {
			t.Errorf("%s: cooldown = %dms, want the 24h plan block %dms", provider, got, want)
		}

		// The ceiling must not clamp the 24h block: a session-scoped provider keeps
		// upstream's generous default ceiling, not this repo's generic two hours.
		cooldown := retryableCooldownSec(http.StatusTooManyRequests,
			time.Duration(cls.CooldownMs)*time.Millisecond,
			&upstreamError{StatusCode: http.StatusTooManyRequests, Body: []byte(body)}, provider)
		if got, want := cooldown, int(24*time.Hour/time.Second); got != want {
			t.Errorf("%s: cooldown after the ceiling = %ds, want %ds", provider, got, want)
		}
	}

	// Another provider's "insufficient balance" keeps the shorter generic quota
	// cooldown: the 24h block is the operator's decision for MiniMax Code.
	other := providers.ClassifyErrorForProvider(http.StatusTooManyRequests, body, 0, "openai")
	if other.CooldownMs >= int(24*time.Hour/time.Millisecond) {
		t.Errorf("openai classified %+v, want the generic quota cooldown", other)
	}
}

// TestMinimaxPlanVerdictIgnoresOtherRateLimits: an unrelated 429 must not park the
// account for a day.
func TestMinimaxPlanVerdictIgnoresOtherRateLimits(t *testing.T) {
	tests := []struct {
		status int
		text   string
		want   bool
	}{
		{status: http.StatusTooManyRequests, text: "usage limit reached: insufficient balance (1008)", want: true},
		{status: http.StatusTooManyRequests, text: "usage limit reached", want: true},
		{status: http.StatusTooManyRequests, text: "too many requests", want: false},
		{status: http.StatusBadRequest, text: "insufficient balance", want: false},
	}
	for _, tt := range tests {
		if got := MinimaxPlanExhaustedVerdict(tt.status, tt.text); got != tt.want {
			t.Errorf("verdict(%d, %q) = %v, want %v", tt.status, tt.text, got, tt.want)
		}
	}
}

// TestHandleAccountFallback_MinimaxPlanParksForADay proves the wiring: the refusal
// parks the account for the plan window and records the block, so the NEXT request
// skips it before going upstream.
func TestHandleAccountFallback_MinimaxPlanParksForADay(t *testing.T) {
	installMinimaxCapKV(t)

	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"base_resp":{"status_code":1008,"status_msg":"insufficient balance"}}`))
	}))
	defer upstream.Close()

	database, cleanup := setupChatTestDB(t)
	defer cleanup()
	if _, err := database.Exec(`DELETE FROM providerConnections`); err != nil {
		t.Fatalf("clear seeded connections: %v", err)
	}
	seedConnDB(t, database, "minimax-code-global", "mm-global", "sk-mm", upstream.URL)

	repo := db.NewRepo(database)
	h := NewChatHandler(repo)

	rec := httptest.NewRecorder()
	err := h.handleAccountFallback(context.Background(), rec, "minimax-code-global", "MiniMax-M3", "",
		[]byte(`{"model":"MiniMax-M3","messages":[{"role":"user","content":"hi"}]}`), false, false, "/v1/chat/completions")
	if err == nil {
		t.Fatal("the refusal must surface")
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("upstream requests = %d, want 1", got)
	}

	// The account is blocked, not merely cooled down.
	if skip, _, reason := ShouldSkipMiniMaxAccount("mm-global"); !skip {
		t.Fatalf("the plan block was not recorded (reason %q)", reason)
	}

	// A second turn skips the account before dialling it.
	rec = httptest.NewRecorder()
	err = h.handleAccountFallback(context.Background(), rec, "minimax-code-global", "MiniMax-M3", "",
		[]byte(`{"model":"MiniMax-M3","messages":[{"role":"user","content":"hi again"}]}`), false, false, "/v1/chat/completions")
	if err == nil {
		t.Fatal("with only a blocked account the turn must fail")
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("upstream requests = %d, want still 1: a blocked account must not be re-dialled", got)
	}
}
