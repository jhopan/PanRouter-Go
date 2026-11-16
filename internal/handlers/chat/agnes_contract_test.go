// Agnes AI — parity with open-sse/providers/registry/agnes.js,
// tests/unit/agnes-max-tokens-cap.test.js and tests/unit/agnes-daily-quota.test.js.
//
// Two provider-specific contracts:
//
//	max_tokens is capped at 4096 server-side (HTTP 400 above it), so the request is
//	clamped before it is sent;
//	the free text quota is a CALENDAR DAY that refills at 00:00 UTC, and its 429 is
//	a spent budget rather than a burst — retrying it two seconds later is what the
//	upstream fix was about.
package chat

import (
	"net/http"
	"testing"
	"time"

	json "panrouter/proxy/internal/fastjson"
	"panrouter/proxy/internal/providers"
	"panrouter/proxy/internal/translator"
)

// agnesDailyQuotaBody is the real 429 body (2026-11-06).
const agnesDailyQuotaBody = `{"error":{"message":"You have used up today's text quota and are now limited to 1 request every 3 minutes. Try again after 10:29:15. (request id: 20261106102716887397250vS7PBaUI)","type":"AgnesAI_error","param":"","code":"rate_limit_exceeded"}}`

func TestAgnesDailyQuotaIsASpentBudget(t *testing.T) {
	cls := providers.ClassifyErrorForProvider(http.StatusTooManyRequests, agnesDailyQuotaBody, 0, "agnes")
	if !cls.Quota {
		t.Fatalf("classified %+v, want the quota verdict", cls)
	}
	// The escalating ladder must not advance: the account is empty until tomorrow,
	// not in a burst window.
	if cls.NewBackoffLevel != 0 {
		t.Errorf("backoff level = %d, want 0", cls.NewBackoffLevel)
	}
}

func TestAgnesBurstRateLimitStaysABurst(t *testing.T) {
	burst := `{"error":{"message":"Rate limit exceeded. Too many requests.","code":"rate_limit_exceeded"}}`
	cls := providers.ClassifyErrorForProvider(http.StatusTooManyRequests, burst, 0, "agnes")
	if cls.Quota {
		t.Errorf("classified %+v, want a burst rather than a spent daily quota", cls)
	}
	// A burst is not parked for the calendar day: its cooldown stays short.
	if cls.CooldownMs >= int(time.Hour/time.Millisecond) {
		t.Errorf("classified %+v, want a short burst cooldown", cls)
	}
}

func TestAgnesDailyParkReachesTheNextUTCMidnight(t *testing.T) {
	ue := &upstreamError{StatusCode: http.StatusTooManyRequests, Body: []byte(agnesDailyQuotaBody)}
	cls := providers.ClassifyErrorForProvider(ue.StatusCode, agnesDailyQuotaBody, 0, "agnes")

	secs := (&ChatHandler{}).cooldownSecFor("conn-agnes", "agnes", ue, agnesDailyQuotaBody, cls)
	if secs <= 60 {
		t.Fatalf("cooldown = %ds, want a park that reaches the calendar reset", secs)
	}
	if max := int((24 * time.Hour) / time.Second); secs > max {
		t.Errorf("cooldown = %ds, want at most a day", secs)
	}
	unlock := time.Now().UTC().Add(time.Duration(secs) * time.Second)
	if unlock.Hour() != 0 || unlock.Minute() != 0 || unlock.Second() != 0 {
		t.Errorf("unlock at %v, want the next 00:00 UTC", unlock)
	}
}

// TestAgnesMaxTokensClamp pins the request-side cap: the endpoint answers 400 above
// 4096, so the value is lowered before it is sent.
func TestAgnesMaxTokensClamp(t *testing.T) {
	clamp := func(provider string, tokens any) map[string]any {
		body := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}, "max_tokens": tokens}
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode body: %v", err)
		}
		out, err := translator.FilterToOpenAIFormatJSON(encoded, translator.FilterOptions{
			MaxTokensCap: providers.MaxOutputCapFor(provider),
		})
		if err != nil {
			t.Fatalf("filter: %v", err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(out, &decoded); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return decoded
	}

	if got := clamp("agnes", float64(16384))["max_tokens"]; got != float64(4096) {
		t.Errorf("agnes max_tokens = %v, want the 4096 cap", got)
	}
	if got := clamp("agnes", float64(8192))["max_tokens"]; got != float64(4096) {
		t.Errorf("agnes max_tokens = %v, want the 4096 cap", got)
	}
	if got := clamp("agnes", float64(4096))["max_tokens"]; got != float64(4096) {
		t.Errorf("a value at the cap must be untouched, got %v", got)
	}
	if got := clamp("agnes", float64(2048))["max_tokens"]; got != float64(2048) {
		t.Errorf("a value below the cap must be untouched, got %v", got)
	}
	if got := clamp("openai", float64(16384))["max_tokens"]; got != float64(16384) {
		t.Errorf("another provider must keep its value, got %v", got)
	}
	if got := clamp("agnes", nil)["max_tokens"]; got != nil {
		t.Errorf("an absent max_tokens must stay absent, got %v", got)
	}
}
