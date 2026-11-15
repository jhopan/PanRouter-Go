//go:build integration

package integration

import (
	"net/http"
	"strings"
	"testing"

	json "encoding/json/v2"

	"panrouter/proxy/internal/providers"
)

// kiroEventFrame wraps one Kiro EventStream frame.
func kiroEventFrame(eventType, payload string) []byte {
	return providers.EncodeEventFrame(map[string]string{":event-type": eventType}, []byte(payload))
}

// TestKiroClaudeWireEndToEnd proves the direct claude→kiro route through the real
// router: a /v1/messages client asks for a Kiro model, the gateway builds the
// conversationState envelope (system material inside the first user turn, no
// top-level instructions), and the client gets Claude frames back rather than
// OpenAI chunks.
//
// This is the wiring the unit tests cannot see: the route table, the provider
// resolution for `kr/`, the executor dispatch, and the response translation.
func TestKiroClaudeWireEndToEnd(t *testing.T) {
	env := newEnv(t)

	up := env.NewUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(kiroEventFrame("reasoningContentEvent", `{"text":"considering"}`))
		_, _ = w.Write(kiroEventFrame("assistantResponseEvent", `{"content":"Hello from Kiro"}`))
		_, _ = w.Write(kiroEventFrame("messageStopEvent", `{"stopReason":"end_turn"}`))
	})
	env.AddConnection(t, "kiro-wire-1", "kiro", "Kiro account", up, "kiro-access-token")

	body := claudeMessagesBody("kr/claude-sonnet-4.5", true)
	body["system"] = "You answer in one sentence."
	res := env.Post(t, "/v1/messages", body, WithHeader("anthropic-version", "2023-06-01"))
	if res.Status != http.StatusOK {
		t.Fatalf("status = %d, want 200\nbody: %s", res.Status, truncate(res.Body))
	}

	// ── what the provider received ───────────────────────────────────────────
	var envelope map[string]any
	if err := json.Unmarshal(up.Last(t).Body, &envelope); err != nil {
		t.Fatalf("upstream body is not JSON: %v\n%s", err, truncate(up.Last(t).Body))
	}
	state, _ := envelope["conversationState"].(map[string]any)
	if state == nil {
		t.Fatalf("upstream body has no conversationState: %v", envelope)
	}
	if id, _ := state["conversationId"].(string); id == "" {
		t.Error("conversationState.conversationId is empty")
	}
	current, _ := state["currentMessage"].(map[string]any)
	user, _ := current["userInputMessage"].(map[string]any)
	content, _ := user["content"].(string)
	// The router pivots `claude → kiro` through OpenAI for this lane, so the
	// system prompt arrives as the `<instructions>` block the OpenAI converter
	// writes. What matters to the gateway contract is that it is INSIDE the user
	// turn: a top-level systemPrompt is rejected with REQUEST_BODY_INVALID.
	if !strings.Contains(content, "You answer in one sentence.") {
		t.Errorf("the client's system prompt must travel in the first user turn, got %q", content)
	}
	if !strings.Contains(content, "Summarize the Go memory model.") && !strings.Contains(content, "memory") {
		t.Errorf("the user message is missing from the envelope, got %q", content)
	}
	if got, _ := user["modelId"].(string); got != "claude-sonnet-4.5" {
		t.Errorf("modelId = %q, want the upstream alias with the kr/ prefix and any suffix stripped", got)
	}
	if got, _ := user["origin"].(string); got != "AI_EDITOR" {
		t.Errorf("origin = %q, want AI_EDITOR", got)
	}
	if _, topLevel := envelope["systemPrompt"]; topLevel {
		t.Error("a top-level systemPrompt is rejected by the gateway; it must travel in the user turn")
	}

	// ── what the client received ─────────────────────────────────────────────
	out := string(res.Body)
	for _, want := range []string{`"type":"message_start"`, "Hello from Kiro", `"type":"message_stop"`, "thinking_delta"} {
		if !strings.Contains(out, want) {
			t.Errorf("Claude stream missing %s\nbody: %s", want, truncate(res.Body))
		}
	}
	if strings.Contains(out, `"object":"chat.completion.chunk"`) {
		t.Errorf("OpenAI chunk shape leaked to a Claude client: %s", truncate(res.Body))
	}
}

// TestKiroOpenAIWireEndToEnd is the same route for a Chat Completions client: the
// envelope is identical, but the response stays OpenAI-shaped.
func TestKiroOpenAIWireEndToEnd(t *testing.T) {
	env := newEnv(t)
	up := env.NewUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(kiroEventFrame("assistantResponseEvent", `{"content":"Hi there"}`))
		_, _ = w.Write(kiroEventFrame("metricsEvent", `{"metricsEvent":{"inputTokens":11,"outputTokens":7}}`))
		_, _ = w.Write(kiroEventFrame("messageStopEvent", `{"stopReason":"end_turn"}`))
	})
	env.AddConnection(t, "kiro-wire-2", "kiro", "Kiro account", up, "kiro-access-token")

	res := env.Post(t, "/v1/chat/completions", ChatBody("kr/claude-sonnet-4.5", false))
	if res.Status != http.StatusOK {
		t.Fatalf("status = %d, want 200\nbody: %s", res.Status, truncate(res.Body))
	}
	var completion struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	res.Decode(t, &completion)
	if completion.Object != "chat.completion" {
		t.Errorf("object = %q, want chat.completion", completion.Object)
	}
	if len(completion.Choices) != 1 || completion.Choices[0].Message.Content != "Hi there" {
		t.Fatalf("choices = %+v, want the Kiro answer", completion.Choices)
	}
	if completion.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want stop", completion.Choices[0].FinishReason)
	}
}
