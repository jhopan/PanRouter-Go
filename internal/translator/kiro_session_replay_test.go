// Kiro session replay — parity with open-sse/utils/kiroSessionReplay.js.
//
// The contract is a byte-identical opening turn across the turns of one session.
// That is not cosmetic: the opening turn carries the current-time context, and a
// gateway that rebuilds it on every request gives up the upstream prompt cache for
// the whole conversation.
package translator

import (
	"strconv"
	"strings"
	"testing"
)

// kiroReplayFirstTurn renders the first user turn of a converted conversation.
func kiroReplayFirstTurn(t *testing.T, history []map[string]any, current map[string]any) string {
	t.Helper()
	for _, turn := range history {
		if user, ok := turn["userInputMessage"].(map[string]any); ok {
			content, _ := user["content"].(string)
			return content
		}
	}
	content, _ := current["content"].(string)
	return content
}

func TestKiroSessionReplayFreezesTheOpeningTurn(t *testing.T) {
	ClearKiroSessionReplayStore()
	model := "claude-sonnet-4.5"
	systemPart := "<thinking_mode>enabled</thinking_mode>"

	firstHistory, firstCurrent := kiroApplySessionReplay("conv-1", "conn-1", model, systemPart, "[Context: Current time is 2026-11-16T00:00:00Z]",
		[]map[string]any{{"userInputMessage": map[string]any{"content": "hello"}}},
		map[string]any{"userInputMessage": map[string]any{"content": "hello"}})

	// The opening turn carries both parts, and the current turn is the same turn.
	frozen := kiroReplayFirstTurn(t, firstHistory, firstCurrent)
	if !strings.Contains(frozen, systemPart) || !strings.Contains(frozen, "2026-11-16T00:00:00Z") {
		t.Fatalf("the frozen turn must carry the system material and the time context, got %q", frozen)
	}

	// A later turn: history now holds the first exchange, and the current turn is
	// new. The opening turn must be replayed verbatim, while the current turn gets
	// the fresh time.
	second := []map[string]any{
		{"userInputMessage": map[string]any{"content": "hello"}},
		{"assistantResponseMessage": map[string]any{"content": "hi"}},
	}
	replayHistory, replayCurrent := kiroApplySessionReplay("conv-1", "conn-1", model, systemPart, "[Context: Current time is 2026-11-16T00:05:00Z]",
		second, map[string]any{"userInputMessage": map[string]any{"content": "next question"}})

	replayed := kiroReplayFirstTurn(t, replayHistory, replayCurrent)
	if replayed != frozen {
		t.Errorf("the opening turn changed between requests, so the cache prefix moved:\nfirst  %q\nsecond %q", frozen, replayed)
	}
	currentUser, _ := replayCurrent["userInputMessage"].(map[string]any)
	currentContent, _ := currentUser["content"].(string)
	if !strings.Contains(currentContent, "2026-11-16T00:05:00Z") {
		t.Errorf("the current turn must carry the fresh time context, got %q", currentContent)
	}
	if strings.Contains(currentContent, systemPart) {
		t.Errorf("the system material must not be re-injected into the current turn, got %q", currentContent)
	}
}

func TestKiroSessionReplayScopesByModelAndSystemPart(t *testing.T) {
	ClearKiroSessionReplayStore()
	history := []map[string]any{{"userInputMessage": map[string]any{"content": "hello"}}}
	kiroApplySessionReplay("conv-2", "conn-1", "claude-sonnet-4.5", "system A", "[t1]", history, nil)

	// A different model is a different cache prefix upstream; replaying the old
	// opening turn would present a conversation the model never saw.
	rebuiltHistory, _ := kiroApplySessionReplay("conv-2", "conn-1", "claude-opus-5", "system A", "[t2]", history,
		map[string]any{"userInputMessage": map[string]any{"content": "hello"}})
	if opening := kiroReplayFirstTurn(t, rebuiltHistory, nil); !strings.Contains(opening, "system A") || !strings.Contains(opening, "[t2]") {
		t.Errorf("a model change must rebuild the opening turn, got %q", opening)
	}
}

func TestKiroSessionReplayNeedsAConversationID(t *testing.T) {
	ClearKiroSessionReplayStore()
	history := []map[string]any{{"userInputMessage": map[string]any{"content": "hello"}}}
	kiroApplySessionReplay("", "conn-1", "claude-sonnet-4.5", "system A", "[t1]", history, nil)
	_, current := kiroApplySessionReplay("", "conn-1", "claude-sonnet-4.5", "system A", "[t9]", history,
		map[string]any{"userInputMessage": map[string]any{"content": "hello"}})
	user, _ := current["userInputMessage"].(map[string]any)
	content, _ := user["content"].(string)
	if !strings.Contains(content, "[t9]") {
		t.Errorf("without a conversation id there is nothing to replay, got %q", content)
	}
}

func TestKiroSessionReplayKeepsAToolResultOpening(t *testing.T) {
	ClearKiroSessionReplayStore()
	// The opening turn already answers a tool call, so the frozen message cannot
	// take its place: a synthetic user turn goes in front of it.
	history := []map[string]any{{
		"userInputMessage": map[string]any{
			"content": "",
			"userInputMessageContext": map[string]any{
				"toolResults": []any{map[string]any{"toolUseId": "t1", "content": []any{map[string]any{"text": "ok"}}}},
			},
		},
	}}
	firstHistory, _ := kiroApplySessionReplay("conv-3", "conn-1", "claude-sonnet-4.5", "system A", "[t1]", history,
		map[string]any{"userInputMessage": map[string]any{"content": "hello"}})
	if len(firstHistory) < 2 {
		t.Fatalf("expected a synthesized opening turn plus the original, got %d", len(firstHistory))
	}
	opening, _ := firstHistory[0]["userInputMessage"].(map[string]any)
	if content, _ := opening["content"].(string); !strings.Contains(content, "system A") {
		t.Errorf("the synthesized opening turn must carry the system material, got %q", content)
	}
	second, _ := firstHistory[1]["userInputMessage"].(map[string]any)
	if results := kiroToolResults(second); len(results) != 1 {
		t.Errorf("the tool result must stay on its own turn, got %d", len(results))
	}
}

func TestKiroSessionReplayStoreStaysBounded(t *testing.T) {
	ClearKiroSessionReplayStore()
	t.Cleanup(ClearKiroSessionReplayStore)
	history := []map[string]any{{"userInputMessage": map[string]any{"content": "hello"}}}
	for i := range kiroSessionStartMaxEntries + 25 {
		kiroApplySessionReplay(kiroSessionKey("conn-1", "conv-"+strconv.Itoa(i)), "conn-1", "claude-sonnet-4.5", "system A", "[t]", history, nil)
	}
	kiroSessionMu.Lock()
	size := len(kiroSessionStarts)
	kiroSessionMu.Unlock()
	if size > kiroSessionStartMaxEntries {
		t.Errorf("store holds %d sessions, want at most %d", size, kiroSessionStartMaxEntries)
	}
}
