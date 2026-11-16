package chat

import (
	"strings"
	"testing"

	json "panrouter/proxy/internal/fastjson"
)

// CodeBuddy cold-account trim — parity with open-sse/services/messageTrimmer.js and
// tests/unit/ban-protections.test.js.
//
// The failure it prevents: a rotation mid-request lands on an account with no cache
// for the conversation, so the whole history is uploaded in one request. That is
// the payload shape the gateway reads as automation.

func codebuddyMessage(role, content string) map[string]any {
	return map[string]any{"role": role, "content": content}
}

func TestCodebuddyEstimateTokens(t *testing.T) {
	// Upstream's own case: 400 characters of prose is 100 tokens.
	if got := codebuddyEstimateTokens([]any{codebuddyMessage("user", strings.Repeat("a", 400))}); got != 100 {
		t.Errorf("estimate = %d, want 100", got)
	}
	// Typed blocks count their text, and a tool call carries a flat cost.
	messages := []any{
		map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": strings.Repeat("b", 40)}}},
		map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": "c1"}}},
	}
	if got := codebuddyEstimateTokens(messages); got < 100 {
		t.Errorf("estimate = %d, want at least the 400 tool-call characters plus the block", got)
	}
}

func TestCodebuddyTrimLeavesASmallHistoryAlone(t *testing.T) {
	messages := []any{codebuddyMessage("system", "sys"), codebuddyMessage("user", "hi")}
	kept, result := codebuddyTrimMessagesToBudget(messages, 100_000)
	if result.removed != 0 {
		t.Errorf("removed = %d, want 0", result.removed)
	}
	if len(kept) != len(messages) {
		t.Errorf("kept = %d messages, want the history untouched", len(kept))
	}
}

func TestCodebuddyTrimKeepsTheSystemPrompt(t *testing.T) {
	messages := []any{
		codebuddyMessage("system", "IMPORTANT"),
		codebuddyMessage("user", strings.Repeat("x", 80_000)),
		codebuddyMessage("assistant", strings.Repeat("y", 80_000)),
		codebuddyMessage("user", "NEWEST"),
	}
	kept, result := codebuddyTrimMessagesToBudget(messages, 20_000)
	if result.removed == 0 {
		t.Fatal("an 160k-character history must be trimmed to a 20k-token budget")
	}
	if result.after >= result.before {
		t.Errorf("after = %d, want below before = %d", result.after, result.before)
	}
	first, _ := kept[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "IMPORTANT" {
		t.Errorf("the system prompt must lead the kept history, got %#v", kept[0])
	}
	// The newest turn always survives: an empty conversation is worse than a long one.
	last, _ := kept[len(kept)-1].(map[string]any)
	if last["content"] != "NEWEST" {
		t.Errorf("the newest message was dropped: %#v", kept[len(kept)-1])
	}
}

func TestCodebuddyTrimNeverStartsOnADanglingToolResult(t *testing.T) {
	messages := []any{
		codebuddyMessage("system", "sys"),
		map[string]any{"role": "assistant", "content": strings.Repeat("a", 40_000), "tool_calls": []any{map[string]any{"id": "c1"}}},
		map[string]any{"role": "tool", "content": "the answer", "tool_call_id": "c1"},
		codebuddyMessage("user", "NEWEST"),
	}
	kept, result := codebuddyTrimMessagesToBudget(messages, 1_000)
	if result.removed == 0 {
		t.Fatal("expected the oversized history to be trimmed")
	}
	for i := 0; i < len(kept); i++ {
		message, _ := kept[i].(map[string]any)
		if role, _ := message["role"].(string); role == "tool" {
			// A tool result may only follow an assistant turn that carries the call.
			prev, _ := kept[i-1].(map[string]any)
			if _, hasCalls := prev["tool_calls"]; !hasCalls {
				t.Fatalf("history starts with a dangling tool result: %#v", kept)
			}
		}
	}
}

// TestCodebuddyTrimOnlyOnRotation pins the caller's guard: same account, no trim;
// a different account, the body is trimmed once and the conversation stays valid.
func TestCodebuddyTrimOnlyOnRotation(t *testing.T) {
	history := []any{codebuddyMessage("system", "sys")}
	for i := 0; i < 40; i++ {
		history = append(history, codebuddyMessage("user", strings.Repeat("x", 40_000)))
	}
	body, err := json.Marshal(map[string]any{"model": "claude-opus-4-6", "messages": history})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}

	// The first attempt has no previous account: nothing is trimmed.
	if trimmed, changed := codebuddyColdAccountTrim("codebuddy-intl", "m", body, "", "conn-a"); changed {
		t.Error("the first attempt must not be trimmed")
		_ = trimmed
	}
	// Still the same account: the account has the cache, so the history stays.
	if _, changed := codebuddyColdAccountTrim("codebuddy-intl", "m", body, "conn-a", "conn-a"); changed {
		t.Error("a same-account retry must not trim")
	}
	// A rotation to another account trims.
	trimmed, changed := codebuddyColdAccountTrim("codebuddy-intl", "m", body, "conn-a", "conn-b")
	if !changed {
		t.Fatal("a rotation to a cold account must trim the history")
	}
	var decoded struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(trimmed, &decoded); err != nil {
		t.Fatalf("trimmed body is not JSON: %v", err)
	}
	if len(decoded.Messages) >= len(history) {
		t.Errorf("trimmed to %d messages, want fewer than %d", len(decoded.Messages), len(history))
	}
	if role, _ := decoded.Messages[0]["role"].(string); role != "system" {
		t.Errorf("the system prompt must survive the trim, got %v", decoded.Messages[0])
	}

	// The other lane is out of scope: upstream guards on codebuddy-intl only.
	if _, changed := codebuddyColdAccountTrim("codebuddy-cn", "m", body, "conn-a", "conn-b"); changed {
		t.Error("the CN lane must not be trimmed")
	}
}
