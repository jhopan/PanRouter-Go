// Claude → Kiro direct route — parity with
// open-sse/translator/request/claude-to-kiro.js and
// tests/translator/claude-kiro-direct.test.js.
//
// The point of the direct route is fidelity: a pivot through OpenAI loses the
// `system` block array, a screenshot inside a tool result, and Anthropic's tool
// ids. Each test below pins one of those, plus the envelope shape the gateway
// validates.
package translator

import (
	"strings"
	"testing"
)

func kiroClaudeEnvelope(t *testing.T, body string, opts KiroTranslateOptions) map[string]any {
	t.Helper()
	if opts.Model == "" {
		opts.Model = "claude-sonnet-4.5"
	}
	out, err := ClaudeToKiro([]byte(body), opts)
	if err != nil {
		t.Fatalf("ClaudeToKiro: %v", err)
	}
	return decodeKiro(t, out)
}

func kiroUserContentOf(t *testing.T, envelope map[string]any) map[string]any {
	t.Helper()
	state := envelope["conversationState"].(map[string]any)
	current := state["currentMessage"].(map[string]any)
	return current["userInputMessage"].(map[string]any)
}

func TestClaudeToKiroCarriesTheSystemBlocks(t *testing.T) {
	envelope := kiroClaudeEnvelope(t, `{
		"system": [{"type":"text","text":"You are a careful engineer."},{"type":"text","text":"Answer briefly."}],
		"messages": [{"role":"user","content":[{"type":"text","text":"hello"}]}]
	}`, KiroTranslateOptions{ContentPrefix: "<thinking_mode>enabled</thinking_mode>"})

	content, _ := kiroUserContentOf(t, envelope)["content"].(string)
	if !strings.Contains(content, "You are a careful engineer.") || !strings.Contains(content, "Answer briefly.") {
		t.Errorf("the system blocks must travel in the first user turn, got %q", content)
	}
	if !strings.Contains(content, "<thinking_mode>enabled</thinking_mode>") {
		t.Errorf("the thinking prefix must stay in front of the system text, got %q", content)
	}
}

func TestClaudeToKiroKeepsToolPairsAndIds(t *testing.T) {
	envelope := kiroClaudeEnvelope(t, `{
		"tools": [{"name":"read_file","description":"Read","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}],
		"messages": [
			{"role":"user","content":[{"type":"text","text":"read a.txt"}]},
			{"role":"assistant","content":[{"type":"tool_use","id":"toolu_01","name":"read_file","input":{"path":"a.txt"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01","content":"contents"}]}
		]
	}`, KiroTranslateOptions{})

	state := envelope["conversationState"].(map[string]any)
	history := state["history"].([]any)
	if len(history) < 2 {
		t.Fatalf("history = %d turns, want the opening pair", len(history))
	}
	assistant := history[1].(map[string]any)["assistantResponseMessage"].(map[string]any)
	uses, _ := assistant["toolUses"].([]any)
	if len(uses) != 1 {
		t.Fatalf("toolUses = %v, want one", assistant["toolUses"])
	}
	call := uses[0].(map[string]any)
	if call["toolUseId"] != "toolu_01" || call["name"] != "read_file" {
		t.Errorf("call = %v, want the client's own id and name", call)
	}
	results := kiroToolResults(kiroUserContentOf(t, envelope))
	if len(results) != 1 {
		t.Fatalf("toolResults = %v, want one", results)
	}
	if id, _ := results[0].(map[string]any)["toolUseId"].(string); id != "toolu_01" {
		t.Errorf("result id = %q, want toolu_01", id)
	}
	ctx := kiroUserContentOf(t, envelope)["userInputMessageContext"].(map[string]any)
	tools, _ := ctx["tools"].([]any)
	if len(tools) != 1 {
		t.Errorf("the catalog must ride on the current message, got %v", ctx["tools"])
	}
}

func TestClaudeToKiroImagesAndToolScreenshots(t *testing.T) {
	envelope := kiroClaudeEnvelope(t, `{
		"tools": [{"name":"screenshot","input_schema":{"type":"object","properties":{}}}],
		"messages": [
			{"role":"user","content":[
				{"type":"text","text":"look"},
				{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"AAAA"}}
			]},
			{"role":"assistant","content":[{"type":"tool_use","id":"toolu_02","name":"screenshot","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_02","content":[
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"BBBB"}},
				{"type":"text","text":"captured"}
			]}]}
		]
	}`, KiroTranslateOptions{})

	// The prompt image rides on the opening (history) turn; the screenshot rides
	// on the current one.
	state := envelope["conversationState"].(map[string]any)
	history := state["history"].([]any)
	opening := history[0].(map[string]any)["userInputMessage"].(map[string]any)
	promptImages, _ := opening["images"].([]any)
	if len(promptImages) != 1 {
		t.Fatalf("opening images = %v, want the prompt image", opening["images"])
	}
	if format := promptImages[0].(map[string]any)["format"]; format != "jpeg" {
		t.Errorf("image format = %v, want the media subtype", format)
	}
	user := kiroUserContentOf(t, envelope)
	images, _ := user["images"].([]any)
	if len(images) != 1 {
		t.Fatalf("current images = %v, want the tool screenshot", user["images"])
	}
	results := kiroToolResults(user)
	if len(results) != 1 {
		t.Fatalf("toolResults = %v, want one", results)
	}
	parts, _ := results[0].(map[string]any)["content"].([]any)
	text := parts[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "captured") {
		t.Errorf("the tool result must keep its text, got %q", text)
	}
}

func TestClaudeToKiroToolResultWithoutTextNamesTheImage(t *testing.T) {
	envelope := kiroClaudeEnvelope(t, `{
		"tools": [{"name":"screenshot","input_schema":{"type":"object","properties":{}}}],
		"messages": [
			{"role":"assistant","content":[{"type":"tool_use","id":"toolu_03","name":"screenshot","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_03","content":[
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"CCCC"}}
			]}]}
		]
	}`, KiroTranslateOptions{})
	results := kiroToolResults(kiroUserContentOf(t, envelope))
	if len(results) != 1 {
		t.Fatalf("toolResults = %v, want one", results)
	}
	parts, _ := results[0].(map[string]any)["content"].([]any)
	if text, _ := parts[0].(map[string]any)["text"].(string); text != "(image attached)" {
		t.Errorf("an image-only tool result reads %q, want the placeholder", text)
	}
}

func TestClaudeToKiroInferenceConfig(t *testing.T) {
	envelope := kiroClaudeEnvelope(t, `{"max_tokens":8192,"temperature":0.2,"top_p":0.9,
		"messages":[{"role":"user","content":"hi"}]}`, KiroTranslateOptions{})
	inference, _ := envelope["inferenceConfig"].(map[string]any)
	if tokens, _ := inference["maxTokens"].(float64); tokens != 8192 {
		t.Errorf("maxTokens = %v, want the client's 8192", inference["maxTokens"])
	}
	if temperature, _ := inference["temperature"].(float64); temperature != 0.2 {
		t.Errorf("temperature = %v, want 0.2", inference["temperature"])
	}
	if topP, _ := inference["topP"].(float64); topP != 0.9 {
		t.Errorf("sampling params = %v", inference)
	}

	// Kiro always wants a maxTokens; the Anthropic default is what upstream sends.
	defaulted := kiroClaudeEnvelope(t, `{"messages":[{"role":"user","content":"hi"}]}`, KiroTranslateOptions{})
	inference, _ = defaulted["inferenceConfig"].(map[string]any)
	if tokens, _ := inference["maxTokens"].(float64); int(tokens) != kiroDefaultMaxTokens {
		t.Errorf("default maxTokens = %v, want %d", inference["maxTokens"], kiroDefaultMaxTokens)
	}
}

func TestClaudeToKiroRestoresSanitizedToolNames(t *testing.T) {
	nameMap := map[string]string{}
	kiroClaudeEnvelope(t, `{
		"tools": [{"name":"mcp.server.do_thing","input_schema":{"type":"object","properties":{}}}],
		"messages": [{"role":"user","content":"hi"}]
	}`, KiroTranslateOptions{ToolNameMap: nameMap})

	// The gateway only accepts a sanitized name; the client can only dispatch the
	// name it declared, so the reverse map has to exist.
	found := false
	for sanitized, original := range nameMap {
		if original == "mcp.server.do_thing" && strings.Contains(sanitized, "mcp") {
			found = true
		}
	}
	if !found {
		t.Errorf("reverse tool-name map missing the original name: %v", nameMap)
	}
}
