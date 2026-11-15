// Kiro canonicalizer — parity with upstream
// tests/unit/kiro-conversation-canonicalization.test.js.
//
// Each case is a body CodeWhisperer answers with 400 REQUEST_BODY_INVALID, and
// each repair below is what turns it into a shippable conversation. The point is
// not tidiness: one structural defect fails the whole request, and the account
// pays the cooldown for a body it could have fixed locally.
package translator

import (
	"strings"
	"testing"
)

// kiroTestSpecs normalizes a catalog of tool definitions the way the request path
// does, so names match what the specs declare.
func kiroTestSpecs(names ...string) ([]any, map[string]string) {
	tools := make([]map[string]any, 0, len(names))
	for _, name := range names {
		tools = append(tools, map[string]any{
			"name":         name,
			"description":  "Tool " + name,
			"input_schema": map[string]any{"type": "object", "properties": map[string]any{}},
		})
	}
	specs, nameMap := kiroNormalizeToolSpecs(tools)
	return specs, nameMap
}

func kiroTestUser(content string, results ...map[string]any) map[string]any {
	return map[string]any{"userInputMessage": kiroTestUserMessage(content, results...)}
}

// kiroTestUserMessage is the inner userInputMessage the translator carries as the
// current message.
func kiroTestUserMessage(content string, results ...map[string]any) map[string]any {
	msg := map[string]any{"content": content, "modelId": "claude-opus-5"}
	if len(results) > 0 {
		raw := make([]any, len(results))
		for i, result := range results {
			raw[i] = result
		}
		msg["userInputMessageContext"] = map[string]any{"toolResults": raw}
	}
	return msg
}

func kiroTestAssistant(content string, uses ...map[string]any) map[string]any {
	msg := map[string]any{"content": content}
	if len(uses) > 0 {
		raw := make([]any, len(uses))
		for i, use := range uses {
			raw[i] = use
		}
		msg["toolUses"] = raw
	}
	return map[string]any{"assistantResponseMessage": msg}
}

func kiroTestCall(id, name string, input map[string]any) map[string]any {
	if input == nil {
		input = map[string]any{}
	}
	return map[string]any{"toolUseId": id, "name": name, "input": input}
}

func kiroTestResult(id, value string) map[string]any {
	return map[string]any{"toolUseId": id, "status": "success", "content": []any{map[string]any{"text": value}}}
}

func TestKiroCanonicalizeKeepsCompleteParallelPairs(t *testing.T) {
	specs, nameMap := kiroTestSpecs("first", "second")
	history, current, _, valid, errs := KiroCanonicalizeConversation(
		[]map[string]any{
			kiroTestUser("start"),
			kiroTestAssistant("run",
				kiroTestCall("t1", "first", map[string]any{"n": 1}),
				kiroTestCall("t2", "second", map[string]any{"n": 2})),
		},
		kiroTestUserMessage("continue", kiroTestResult("t1", "one"), kiroTestResult("t2", "two")),
		"claude-opus-5", specs, nameMap,
	)
	if !valid {
		t.Fatalf("canonical conversation invalid: %v", errs)
	}
	calls, _ := history[1]["assistantResponseMessage"].(map[string]any)["toolUses"].([]any)
	results := kiroToolResults(current)
	if len(calls) != 2 || len(results) != 2 {
		t.Fatalf("expected both pairs kept, got %d calls / %d results", len(calls), len(results))
	}
	for i, want := range []string{"t1", "t2"} {
		call, _ := calls[i].(map[string]any)
		result, _ := results[i].(map[string]any)
		if call["toolUseId"] != want || result["toolUseId"] != want {
			t.Errorf("pair %d = %v / %v, want %s", i, call["toolUseId"], result["toolUseId"], want)
		}
	}
}

func TestKiroCanonicalizeFlattensUnansweredCall(t *testing.T) {
	specs, nameMap := kiroTestSpecs("first", "second")
	history, current, repairs, valid, errs := KiroCanonicalizeConversation(
		[]map[string]any{
			kiroTestUser("start"),
			kiroTestAssistant("run",
				kiroTestCall("t1", "first", nil),
				kiroTestCall("t2", "second", nil)),
		},
		kiroTestUserMessage("continue", kiroTestResult("t1", "one")),
		"claude-opus-5", specs, nameMap,
	)
	if !valid {
		t.Fatalf("canonical conversation invalid: %v", errs)
	}
	assistant, _ := history[1]["assistantResponseMessage"].(map[string]any)
	calls, _ := assistant["toolUses"].([]any)
	if len(calls) != 1 {
		t.Fatalf("expected the unanswered call to be dropped from toolUses, got %d", len(calls))
	}
	call, _ := calls[0].(map[string]any)
	if call["toolUseId"] != "t1" {
		t.Errorf("kept call = %v, want t1", call["toolUseId"])
	}
	content, _ := assistant["content"].(string)
	if !strings.Contains(content, "[Tool call: second(") {
		t.Errorf("flattened call missing from content: %q", content)
	}
	if repairs.MissingResults != 1 {
		t.Errorf("MissingResults = %d, want 1", repairs.MissingResults)
	}
	_ = current
}

func TestKiroCanonicalizeFlattensOrphanResults(t *testing.T) {
	specs, nameMap := kiroTestSpecs("first")
	history, current, repairs, valid, errs := KiroCanonicalizeConversation(
		[]map[string]any{
			kiroTestUser("start"),
			kiroTestAssistant("run", kiroTestCall("t1", "first", nil)),
			kiroTestUser("result missing here"),
			kiroTestAssistant("later"),
		},
		kiroTestUserMessage("late result", kiroTestResult("t1", "too late")),
		"claude-opus-5", specs, nameMap,
	)
	if !valid {
		t.Fatalf("canonical conversation invalid: %v", errs)
	}
	if got := kiroStructuredIDs(history, current); strings.Contains(got, "t1") {
		t.Errorf("a non-adjacent result must not survive as structure: %s", got)
	}
	assistant, _ := history[1]["assistantResponseMessage"].(map[string]any)
	if content, _ := assistant["content"].(string); !strings.Contains(content, "[Tool call:") {
		t.Errorf("orphaned call was not flattened into the assistant turn: %q", content)
	}
	if content, _ := current["content"].(string); !strings.Contains(content, "too late") {
		t.Errorf("orphaned result was not flattened into the user turn: %q", content)
	}
	if repairs.OrphanResults == 0 {
		t.Error("the repair counter must report the flattened result")
	}
}

func TestKiroCanonicalizeRemapsDuplicateToolIDs(t *testing.T) {
	specs, nameMap := kiroTestSpecs("first", "second")
	history, current, _, valid, errs := KiroCanonicalizeConversation(
		[]map[string]any{
			kiroTestUser("start"),
			kiroTestAssistant("run",
				kiroTestCall("duplicate", "first", nil),
				kiroTestCall("duplicate", "second", nil)),
		},
		kiroTestUserMessage("continue", kiroTestResult("duplicate", "one"), kiroTestResult("duplicate", "two")),
		"claude-opus-5", specs, nameMap,
	)
	if !valid {
		t.Fatalf("canonical conversation invalid: %v", errs)
	}
	calls, _ := history[1]["assistantResponseMessage"].(map[string]any)["toolUses"].([]any)
	results := kiroToolResults(current)
	if len(calls) != 2 || len(results) != 2 {
		t.Fatalf("expected two repaired pairs, got %d calls / %d results", len(calls), len(results))
	}
	if calls[0].(map[string]any)["toolUseId"] == calls[1].(map[string]any)["toolUseId"] {
		t.Error("duplicate ids must be remapped apart")
	}
	for i := range calls {
		call, _ := calls[i].(map[string]any)
		result, _ := results[i].(map[string]any)
		if call["toolUseId"] != result["toolUseId"] {
			t.Errorf("pair %d id mismatch: call %v result %v", i, call["toolUseId"], result["toolUseId"])
		}
	}
}

func TestKiroCanonicalizeFlattensToolsWithoutSpecs(t *testing.T) {
	// A body that calls a tool the request never declared cannot be repaired
	// structurally: Kiro would reject the unknown name, so both sides flatten.
	history, current, _, valid, errs := KiroCanonicalizeConversation(
		[]map[string]any{
			kiroTestUser("start"),
			kiroTestAssistant("run", kiroTestCall("t1", "unknown_tool", nil)),
		},
		kiroTestUserMessage("continue", kiroTestResult("t1", "one")),
		"claude-opus-5", nil, nil,
	)
	if !valid {
		t.Fatalf("conversation must be repairable by flattening: %v", errs)
	}
	assistant, _ := history[1]["assistantResponseMessage"].(map[string]any)
	if _, ok := assistant["toolUses"]; ok {
		t.Error("a call with no matching spec must not stay structured")
	}
	if content, _ := assistant["content"].(string); !strings.Contains(content, "[Tool call: unknown_tool(") {
		t.Errorf("flattened call missing: %q", content)
	}
	if len(kiroToolResults(current)) != 0 {
		t.Error("the result of a flattened call must not stay structured")
	}
}

func TestKiroCanonicalizeFillsAnEmptyCurrentMessage(t *testing.T) {
	// An empty current message would make the model answer "nothing to
	// continue", so the placeholder is what keeps the turn meaningful — and the
	// conversation valid without a repair the caller has to handle.
	history, current, _, valid, errs := KiroCanonicalizeConversation(
		[]map[string]any{kiroTestAssistant("orphan")},
		map[string]any{"content": "   "},
		"claude-opus-5", nil, nil,
	)
	if !valid {
		t.Fatalf("conversation invalid: %v", errs)
	}
	if content, _ := current["content"].(string); content != kiroEmptyUserPlaceholder {
		t.Errorf("current content = %q, want %q", content, kiroEmptyUserPlaceholder)
	}
	// A leading assistant turn cannot open a conversation: a user turn is put in
	// front of it, and the placeholder current message closes it.
	if len(history) != 2 {
		t.Fatalf("history = %d turns, want the synthesized user turn plus the assistant", len(history))
	}
	if _, ok := history[0]["userInputMessage"]; !ok {
		t.Errorf("the conversation must start with a user turn: %v", history[0])
	}
}

// kiroStructuredIDs renders the surviving toolUseId values, so a test can assert
// that a repair removed the structure rather than merely moving it.
func kiroStructuredIDs(history []map[string]any, current map[string]any) string {
	var b strings.Builder
	for _, turn := range history {
		assistant, _ := turn["assistantResponseMessage"].(map[string]any)
		if uses, ok := assistant["toolUses"].([]any); ok {
			for _, use := range uses {
				call, _ := use.(map[string]any)
				b.WriteString(kiroRawID(call["toolUseId"]))
				b.WriteByte(' ')
			}
		}
	}
	for _, result := range kiroToolResults(current) {
		entry, _ := result.(map[string]any)
		b.WriteString(kiroRawID(entry["toolUseId"]))
		b.WriteByte(' ')
	}
	return b.String()
}
