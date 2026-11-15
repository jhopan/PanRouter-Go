package providers

import (
	"testing"
	"time"
)

// CodeBuddy Intl's refusal vocabulary, ported from
// open-sse/config/errorConfig.js:153-174 and the intl executor's parseError.
//
// The three verdicts are deliberately different, and mixing them up is what the
// upstream fixes were about:
//
//	11140 / "unusual activity" / "restricted due to" → the ACCOUNT is banned
//	11128 "request illegal"                           → the REQUEST is refused
//	"request illegal" with no code                    → the account is flagged
//	"conversation id missing"                         → a per-request throttle
//
// A ban earned a two-second backoff, and a refused request parked a healthy
// account for 1800s, when these were one verdict.
func TestClassifyErrorForProvider_CodebuddyBans(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{name: "the 11140 code", text: `{"code":11140,"msg":"request illegal","displayMsg":{"en":"Your account has been restricted due to unusual activity"}}`},
		{name: "the restriction sentence", text: "Your account has been restricted due to unusual activity"},
		{name: "restricted due to, alone", text: "restricted due to"},
		{name: "the Chinese wording", text: "账号存在异常行为,已被限制使用"},
		{name: "the traditional wording", text: "帳號存在異常行為"},
		{name: "a bare request illegal", text: "request illegal"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cls := ClassifyErrorForProvider(403, tt.text, 0, "codebuddy-intl")
			if !cls.Banned {
				t.Fatalf("classified %+v, want a ban", cls)
			}
			if cls.Fatal {
				t.Errorf("a ban is not a fatal request verdict: %+v", cls)
			}
			// A ban has no reset: parking it on a timer is the retry pattern that
			// earned it. The caller uses BannedAccountCooldown instead.
			if cls.CooldownMs != 0 {
				t.Errorf("cooldown = %d, want none (the ban window is the caller's)", cls.CooldownMs)
			}
		})
	}
}

func TestClassifyErrorForProvider_CodebuddyRequestRefused(t *testing.T) {
	// 11128 is channel verification refusing the request shape. The account is
	// healthy, so locking it is the bug the fatal flag exists to prevent.
	cls := ClassifyErrorForProvider(403, `{"code":11128,"msg":"request illegal"}`, 0, "codebuddy-intl")
	if !cls.Fatal || !cls.RequestRejected {
		t.Fatalf("classified %+v, want fatal request rejection", cls)
	}
	if cls.Banned {
		t.Errorf("11128 must not ban the account: %+v", cls)
	}
}

func TestClassifyErrorForProvider_CodebuddyBanBeatsTheRefusal(t *testing.T) {
	// The 11140 body also says "request illegal": the ban row has to win, or the
	// account keeps its 2s backoff and is re-dialled all month.
	cls := ClassifyErrorForProvider(403, `{"code":11140,"msg":"request illegal"}`, 0, "codebuddy-intl")
	if !cls.Banned {
		t.Fatalf("11140 body classified %+v, want the ban verdict", cls)
	}
}

func TestClassifyErrorForProvider_CodebuddyTooLarge(t *testing.T) {
	for _, text := range []string{
		`{"code":11115,"msg":"prompt is too long: 1054321 tokens > 1048576 maximum"}`,
		"content too long",
		"内容过长,请精简或新建任务",
		"內容過長",
	} {
		cls := ClassifyErrorForProvider(400, text, 0, "codebuddy-intl")
		if !cls.Fatal || !cls.RequestTooLarge {
			t.Errorf("%q classified %+v, want fatal request-too-large", text, cls)
		}
	}
}

func TestClassifyErrorForProvider_ConversationIDMissingBacksOff(t *testing.T) {
	// Nothing about the account is wrong, but the gateway throttles the caller:
	// this one keeps the exponential backoff.
	cls := ClassifyErrorForProvider(400, "conversation id missing", 0, "codebuddy-intl")
	if !cls.ShouldFallback || cls.CooldownMs != BackoffConfig.BaseMs || cls.NewBackoffLevel != 1 {
		t.Errorf("classified %+v, want backoff at base level 1", cls)
	}
}

// The same texts stay request-scoped everywhere else: no cooldown, no verdict.
func TestClassifyErrorForProvider_OtherProvidersStayRequestScoped(t *testing.T) {
	for _, provider := range []string{"openai", "codebuddy-cn", ""} {
		cls := ClassifyErrorForProvider(400, "request illegal", 0, provider)
		if cls.ShouldFallback || cls.CooldownMs != 0 || cls.Banned || cls.Fatal {
			t.Errorf("provider %q classified %+v, want request-scoped", provider, cls)
		}
	}
	// The legacy signature has no provider context, so the scoped rules
	// cannot fire there either.
	if cls := ClassifyError(400, "request illegal", 0); cls.ShouldFallback || cls.Banned {
		t.Errorf("provider-agnostic classify fell back: %+v", cls)
	}
}

// A malformed request is the caller's bug, and it carries no CodeBuddy code.
func TestClassifyErrorForProvider_MalformedCodebuddyRequest(t *testing.T) {
	cls := ClassifyErrorForProvider(400, "first message is not system prompt", 0, "codebuddy-intl")
	if cls.ShouldFallback || cls.CooldownMs != 0 || cls.Banned || cls.Fatal {
		t.Errorf("malformed request classified %+v, want request-scoped", cls)
	}
}

// The combo member id may arrive as the cbai alias; the rules still match.
func TestClassifyErrorForProvider_CbaiAliasResolves(t *testing.T) {
	cls := ClassifyErrorForProvider(403, "request illegal", 0, "cbai")
	if !cls.Banned {
		t.Errorf("cbai alias classified %+v, want the ban verdict", cls)
	}
}

func TestBannedAccountCooldownIsThirtyDays(t *testing.T) {
	// Upstream BANNED_ACCOUNT_COOLDOWN_MS: long enough that a manually appealed
	// account comes back on its own, short enough that a permanent row does not
	// need operator surgery.
	if want := 30 * 24 * time.Hour; BannedAccountCooldown != want {
		t.Errorf("BannedAccountCooldown = %v, want %v", BannedAccountCooldown, want)
	}
}

func TestClassifyError_CodebuddyCodes(t *testing.T) {
	cls1 := ClassifyError(400, "11158: model_not_in_plan", 0)
	if !cls1.ShouldFallback {
		t.Error("expected fallback for 11158")
	}
	cls2 := ClassifyError(400, "14018: credits exhausted", 0)
	if !cls2.ShouldFallback || cls2.CooldownMs != cooldownLong {
		t.Errorf("14018 classified %+v, want cooldownLong", cls2)
	}
	// The generic 11140 row still parks as an address refusal for providers with
	// no ban rule of their own; the intl-scoped row above wins for CodeBuddy.
	cls3 := ClassifyError(400, "11140: account has been restricted due to unusual activity", 0)
	if !cls3.ShouldFallback || cls3.CooldownMs != cooldownAddressRefused {
		t.Errorf("11140 classified %+v, want cooldownAddressRefused", cls3)
	}
}
