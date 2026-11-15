package providers

import "testing"

// TestClassifyErrorForProvider_AntigravityTransientBodies ports
// ANTIGRAVITY_TRANSIENT_ERROR_PATTERNS and isTransientAntigravityError
// (open-sse/executors/antigravity.js:26-42, :545-549). Upstream retries these
// faults on the BODY text, whatever status carried them: "high traffic" and an
// agent turn Google terminated are what it sees on a plain 400, so a status-only
// classifier surfaces them to the client without trying another account.
//
// Fails before the change: classCapacity covered "capacity" and one spelling of
// "temporarily unavailable" only, so every phrase below fell through to the
// request-scoped 4xx verdict (ShouldFallback false).
func TestClassifyErrorForProvider_AntigravityTransientBodies(t *testing.T) {
	bodies := []string{
		"The model is currently experiencing high traffic. Please try again.",
		"Agent execution terminated due to error.",
		"agent terminated due to error",
		"The service is temporarily unavailable.",
		"upstream timeout while reading response",
		"stream ended before completion",
		"stream closed unexpectedly",
		"stream terminated",
		"stream interrupted by the provider",
		"empty response from the model",
	}

	for _, body := range bodies {
		t.Run(body, func(t *testing.T) {
			cls := ClassifyErrorForProvider(400, body, 0, "antigravity")
			if !cls.ShouldFallback {
				t.Fatalf("400 %q classified %+v, want a transient fallback", body, cls)
			}
			if cls.NewBackoffLevel != 1 {
				t.Errorf("backoff level = %d, want 1 (transient, not a permanent block)", cls.NewBackoffLevel)
			}
		})
	}
}

// TestClassifyErrorForProvider_TransientBodiesStayAntigravityScoped pins the
// provider scoping: the same prose on another lane is not evidence that its
// fault is transient, and the shared 4xx rule (no account fault) must keep
// answering request-scoped.
func TestClassifyErrorForProvider_TransientBodiesStayAntigravityScoped(t *testing.T) {
	for _, provider := range []string{"openai", "codebuddy-intl", "gemini", ""} {
		cls := ClassifyErrorForProvider(400, "upstream timeout: stream ended unexpectedly", 0, provider)
		if cls.ShouldFallback {
			t.Errorf("provider %q classified %+v, want request-scoped", provider, cls)
		}
	}
}

// TestClassifyErrorForProvider_AntigravityTransientKeepsRateLimitSemantics pins
// the boundary the new rows must not cross: they are text rules, so a 429 keeps
// its own verdict. The CodeBuddy stop-the-rotation rule keys on the status (see
// stopsRotationOnRateLimit in internal/handlers/chat), which no body phrase can
// widen — this asserts the classifier half of that contract.
func TestClassifyErrorForProvider_AntigravityTransientKeepsRateLimitSemantics(t *testing.T) {
	for _, provider := range []string{"antigravity", "codebuddy-intl", "codebuddy-cn"} {
		cls := ClassifyErrorForProvider(429, "high traffic: stream ended", 0, provider)
		if !cls.ShouldFallback || cls.CooldownMs != BackoffConfig.BaseMs || cls.NewBackoffLevel != 1 {
			t.Errorf("provider %q 429 classified %+v, want the standard rate-limit backoff", provider, cls)
		}
	}
}
