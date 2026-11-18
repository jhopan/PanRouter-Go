package translator_test

import (
	"encoding/json"
	"strings"
	"testing"

	"panrouter/proxy/internal/translator"
)

// TestWrapForAntigravityWithIdentity pins the per-account identity branch: the
// shipped client's template picks between two sentences on the account's
// `generic-system-prompt` experiment, and the account that answered the capture
// resolves to the generic branch.
func TestWrapForAntigravityWithIdentity(t *testing.T) {
	geminiBody := []byte(`{"system_instruction":{"parts":[{"text":"You are a helpful assistant."}]},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)

	read := func(t *testing.T, generic bool) string {
		t.Helper()
		wrapped, err := translator.WrapForAntigravityWithIdentity(geminiBody, "proj-1", "claude-sonnet-4-6", generic, "sess-identity")
		if err != nil {
			t.Fatalf("WrapForAntigravityWithIdentity(%v): %v", generic, err)
		}
		inner := unwrapInnerRequest(t, wrapped)
		si, _ := inner["systemInstruction"].(map[string]any)
		parts, _ := si["parts"].([]any)
		if len(parts) == 0 {
			t.Fatalf("no systemInstruction parts: %s", string(wrapped))
		}
		first, _ := parts[0].(map[string]any)
		text, _ := first["text"].(string)
		return text
	}

	genericText := read(t, true)
	if !strings.HasPrefix(genericText, translator.AntigravityIdentityPrompt) {
		t.Errorf("generic branch text = %q, want the generic identity block", genericText[:min(120, len(genericText))])
	}

	codingText := read(t, false)
	if !strings.HasPrefix(codingText, translator.AntigravityIdentityPromptCoding) {
		t.Errorf("coding branch text = %q, want the coding identity block", codingText[:min(120, len(codingText))])
	}
	if genericText == codingText {
		t.Error("the two experiment branches produced the same system instruction")
	}
	if !strings.Contains(codingText, "Advanced Agentic Coding") {
		t.Errorf("coding branch missing its distinguishing sentence: %q", codingText[:min(160, len(codingText))])
	}

	// Both branches keep the caller's own text after the identity block, and both
	// keep the request tags the lane expects.
	for name, text := range map[string]string{"generic": genericText, "coding": codingText} {
		if !strings.Contains(text, "</identity>You are a helpful assistant.") {
			t.Errorf("%s branch did not keep the caller's text after the identity block: %q", name, text)
		}
	}
}

// TestWrapForAntigravityKeepsTheGenericBranch pins the compatibility contract:
// callers that never learned the account's experiment keep sending exactly what
// this port sent before the branch existed.
func TestWrapForAntigravityKeepsTheGenericBranch(t *testing.T) {
	geminiBody := []byte(`{"system_instruction":{"parts":[{"text":"You are a helpful assistant."}]},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	plain, err := translator.WrapForAntigravity(geminiBody, "proj-1", "claude-sonnet-4-6", "sess-compat")
	if err != nil {
		t.Fatalf("WrapForAntigravity: %v", err)
	}
	explicit, err := translator.WrapForAntigravityWithIdentity(geminiBody, "proj-1", "claude-sonnet-4-6", true, "sess-compat")
	if err != nil {
		t.Fatalf("WrapForAntigravityWithIdentity: %v", err)
	}
	var a, b map[string]any
	if err := json.Unmarshal(plain, &a); err != nil {
		t.Fatalf("unmarshal plain: %v", err)
	}
	if err := json.Unmarshal(explicit, &b); err != nil {
		t.Fatalf("unmarshal explicit: %v", err)
	}
	// requestId carries a millisecond timestamp, so compare everything else.
	delete(a, "requestId")
	delete(b, "requestId")
	if !jsonEqual(t, a, b) {
		t.Errorf("WrapForAntigravity and the generic branch disagree\n plain   = %s\n generic = %s", mustJSON(a), mustJSON(b))
	}
}

// TestWrapForAntigravityAlwaysCarriesTheIdentity pins the shape the client never
// departs from: every turn opens its system instruction with the identity block,
// including a call that forwarded no system message of its own.
func TestWrapForAntigravityAlwaysCarriesTheIdentity(t *testing.T) {
	bare := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	wrapped, err := translator.WrapForAntigravity(bare, "proj-1", "claude-sonnet-4-6", "sess-bare")
	if err != nil {
		t.Fatalf("WrapForAntigravity: %v", err)
	}
	inner := unwrapInnerRequest(t, wrapped)
	si, ok := inner["systemInstruction"].(map[string]any)
	if !ok {
		t.Fatalf("no systemInstruction on a call that forwarded none: %s", string(wrapped))
	}
	parts, _ := si["parts"].([]any)
	if len(parts) != 1 {
		t.Fatalf("systemInstruction parts = %d, want the identity block alone", len(parts))
	}
	first, _ := parts[0].(map[string]any)
	if text, _ := first["text"].(string); text != translator.AntigravityIdentityPrompt {
		t.Errorf("identity text = %q, want the identity block alone", text[:min(120, len(text))])
	}

	// An empty system instruction gets the same treatment rather than being left
	// blank.
	emptySI := []byte(`{"system_instruction":{"parts":[]},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	wrapped, err = translator.WrapForAntigravity(emptySI, "proj-1", "claude-sonnet-4-6", "sess-empty-si")
	if err != nil {
		t.Fatalf("WrapForAntigravity (empty si): %v", err)
	}
	inner = unwrapInnerRequest(t, wrapped)
	si, _ = inner["systemInstruction"].(map[string]any)
	parts, _ = si["parts"].([]any)
	first, _ = parts[0].(map[string]any)
	if text, _ := first["text"].(string); text != translator.AntigravityIdentityPrompt {
		t.Errorf("empty system instruction was not filled with the identity: %q", text[:min(120, len(text))])
	}
}

func jsonEqual(t *testing.T, a, b map[string]any) bool {
	t.Helper()
	return mustJSON(a) == mustJSON(b)
}

func mustJSON(v any) string {
	out, _ := json.Marshal(v)
	return string(out)
}
