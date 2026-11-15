// Kiro request intent — parity with upstream open-sse/config/kiroConstants.js
// and tests/unit/thinking-levels-kiro.test.js.
//
// Three things reach the wire from a client's reasoning request, and each has its
// own rule: the magic system-prompt prefix (thinking), the agentic protocol
// prompt, and the native `additionalModelRequestFields` envelope the newer Claude
// and GPT models take. Getting the combination wrong is not cosmetic — the prefix
// is what switches reasoning on, and a model that receives both a prefix and a
// native effort is asked for reasoning twice.
package executor

import (
	"encoding/json"
	"strings"
	"testing"
)

// kiroEnvelope builds the upstream body for one client request and decodes what
// the executor produced.
func kiroEnvelope(t *testing.T, model, body string) map[string]any {
	t.Helper()
	out, err := kiroUpstreamBody(&Request{
		Body:      []byte(body),
		ModelName: model,
	})
	if err != nil {
		t.Fatalf("kiroUpstreamBody: %v", err)
	}
	var envelope map[string]any
	if err := json.Unmarshal(out, &envelope); err != nil {
		t.Fatalf("envelope is not JSON: %v", err)
	}
	return envelope
}

// kiroFirstUserContent reads the content of the first user turn.
func kiroFirstUserContent(t *testing.T, envelope map[string]any) string {
	t.Helper()
	state, _ := envelope["conversationState"].(map[string]any)
	current, _ := state["currentMessage"].(map[string]any)
	user, _ := current["userInputMessage"].(map[string]any)
	content, _ := user["content"].(string)
	if content == "" {
		if history, ok := state["history"].([]any); ok && len(history) > 0 {
			first, _ := history[0].(map[string]any)
			firstUser, _ := first["userInputMessage"].(map[string]any)
			content, _ = firstUser["content"].(string)
		}
	}
	return content
}

func kiroModelID(t *testing.T, envelope map[string]any) string {
	t.Helper()
	state, _ := envelope["conversationState"].(map[string]any)
	current, _ := state["currentMessage"].(map[string]any)
	user, _ := current["userInputMessage"].(map[string]any)
	id, _ := user["modelId"].(string)
	return id
}

func TestKiroIntentStripsBothSuffixes(t *testing.T) {
	// `-thinking-agentic` ends in `-agentic`: stripping `-thinking` first would
	// leave `-thinking` in the upstream id and Kiro answers 400 INVALID_MODEL_ID.
	envelope := kiroEnvelope(t, "kr/claude-sonnet-4.5-thinking-agentic", `{"messages":[{"role":"user","content":"hi"}]}`)

	if got := kiroModelID(t, envelope); got != "claude-sonnet-4.5" {
		t.Errorf("modelId = %q, want claude-sonnet-4.5 with both fictions stripped", got)
	}
	content := kiroFirstUserContent(t, envelope)
	if !strings.Contains(content, "<thinking_mode>enabled</thinking_mode>") {
		t.Errorf("the -thinking suffix must switch reasoning on via the magic prefix, got %q", content)
	}
	if !strings.Contains(content, "CHUNKED WRITE PROTOCOL") {
		t.Errorf("the -agentic suffix must carry the chunked-write prompt, got %q", content)
	}
}

func TestKiroIntentThinkingBudgetFromBody(t *testing.T) {
	envelope := kiroEnvelope(t, "claude-sonnet-4.5", `{"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":8000}}`)
	content := kiroFirstUserContent(t, envelope)
	if !strings.Contains(content, "<max_thinking_length>8000</max_thinking_length>") {
		t.Errorf("the client's budget must reach the prefix, got %q", content)
	}
}

func TestKiroIntentThinkingDisabled(t *testing.T) {
	envelope := kiroEnvelope(t, "claude-sonnet-4.5-thinking", `{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"none"}`)
	content := kiroFirstUserContent(t, envelope)
	if strings.Contains(content, "<thinking_mode>") {
		t.Errorf("an explicit none must win over the suffix, got %q", content)
	}
}

func TestKiroIntentClaudeEffortEnvelope(t *testing.T) {
	envelope := kiroEnvelope(t, "claude-opus-5.5", `{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"max"}`)
	fields, ok := envelope["additionalModelRequestFields"].(map[string]any)
	if !ok {
		t.Fatalf("claude 4.6+ must carry the native effort envelope, got %v", envelope["additionalModelRequestFields"])
	}
	outputConfig, _ := fields["output_config"].(map[string]any)
	if outputConfig["effort"] != "high" {
		t.Errorf("effort = %v, want max folded onto high", outputConfig["effort"])
	}
	thinking, _ := fields["thinking"].(map[string]any)
	if thinking["type"] != "adaptive" {
		t.Errorf("thinking.type = %v, want adaptive", thinking["type"])
	}
	// Claude takes the prefix AND the envelope: only the GPT path suppresses the
	// prefix for a native effort.
	if content := kiroFirstUserContent(t, envelope); !strings.Contains(content, "<thinking_mode>") {
		t.Errorf("claude reasoning still needs the magic prefix, got %q", content)
	}
}

func TestKiroIntentLegacyClaudeSendsNoEnvelope(t *testing.T) {
	// Kiro rejected additionalModelRequestFields on legacy 4.5 models in live smoke.
	envelope := kiroEnvelope(t, "claude-sonnet-4.5", `{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`)
	if _, ok := envelope["additionalModelRequestFields"]; ok {
		t.Error("claude-sonnet-4.5 must not receive additionalModelRequestFields")
	}
}

func TestKiroIntentGPTEffortSuppressesThePrefix(t *testing.T) {
	envelope := kiroEnvelope(t, "gpt-5.6-sol", `{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"max"}`)
	fields, ok := envelope["additionalModelRequestFields"].(map[string]any)
	if !ok {
		t.Fatalf("the GPT path must carry reasoning.effort, got %v", envelope["additionalModelRequestFields"])
	}
	reasoning, _ := fields["reasoning"].(map[string]any)
	if reasoning["effort"] != "xhigh" {
		t.Errorf("effort = %v, want max folded onto xhigh", reasoning["effort"])
	}
	if content := kiroFirstUserContent(t, envelope); strings.Contains(content, "<thinking_mode>") {
		t.Errorf("a native GPT effort must not also inject the magic prefix, got %q", content)
	}
}

func TestKiroIntentModelLevelSuffix(t *testing.T) {
	// `model(high)` is the client-side override dialect: it must land in the body
	// the thinking resolver reads, not in the upstream model id.
	envelope := kiroEnvelope(t, "claude-sonnet-4.5(high)", `{"messages":[{"role":"user","content":"hi"}]}`)
	if got := kiroModelID(t, envelope); got != "claude-sonnet-4.5" {
		t.Errorf("modelId = %q, want the suffix consumed", got)
	}
	content := kiroFirstUserContent(t, envelope)
	if !strings.Contains(content, "<thinking_mode>enabled</thinking_mode>") {
		t.Errorf("the (level) override must enable reasoning, got %q", content)
	}
}

func TestKiroIntentBetaHeaderEnablesReasoning(t *testing.T) {
	// Claude Code asks for interleaved thinking with a beta header, not a field.
	body, err := kiroUpstreamBody(&Request{
		Body:      []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
		ModelName: "claude-sonnet-4.5",
		Headers:   map[string][]string{"Anthropic-Beta": {"interleaved-thinking-2025-05-14"}},
	})
	if err != nil {
		t.Fatalf("kiroUpstreamBody: %v", err)
	}
	if !strings.Contains(string(body), "<thinking_mode>enabled</thinking_mode>") {
		t.Errorf("the beta header must switch reasoning on, got %s", body)
	}
}
