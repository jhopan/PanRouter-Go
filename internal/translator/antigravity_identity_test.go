// Antigravity prompt identity and session id — measured, not guessed.
//
// Field evidence (2026-11-17, `v1internal:generateContent` on
// daily-cloudcode-pa, one account, one model, only the system prompt varying —
// MITM-captured through mitmdump, the same account served a 200 for a neutral
// prompt seconds before each 429):
//
//	"You are Claude Code."                                              -> 429 RESOURCE_EXHAUSTED
//	"you are claude code, anthropic's official cli for claude."          -> 429 RESOURCE_EXHAUSTED
//	"You are Claude Code, Anthropic's official CLI for Claude."          -> 429 RESOURCE_EXHAUSTED
//	"You are an assistant built as Anthropic's official CLI for Claude." -> 200
//	"You are a Claude agent, built on Anthropic's Claude Agent SDK."     -> 200
//	"x-anthropic-billing-header: ..."                                    -> 200
//	"You like the name Claude." / "You are opencode ..."                 -> 200
//
// So the fingerprint is the identity phrase itself, not the client it came from,
// and the 429 is a filtering verdict rather than quota: that account's weekly
// bucket still showed 61% remaining while it refused.
package translator

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"regexp"
	"testing"
)

func TestRewriteBrandingText_ClaudeCodeIdentityIsNeutralised(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{name: "the exact sentence", input: "You are Claude Code, Anthropic's official CLI for Claude."},
		{name: "lowercased", input: "you are claude code, anthropic's official cli for claude."},
		{name: "no trailing clause", input: "You are Claude Code."},
		{name: "inside a longer prompt", input: "You are Claude Code, Anthropic's official CLI for Claude.\nKeep answers short."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rewriteBrandingText(tt.input, true)
			if regexp.MustCompile(`(?i)you are claude code`).MatchString(got) {
				t.Errorf("identity phrase survived: %q -> %q", tt.input, got)
			}
			if !regexp.MustCompile(`(?i)you are an ai assistant`).MatchString(got) {
				t.Errorf("no neutral replacement in %q", got)
			}
			if tt.name == "inside a longer prompt" && !regexp.MustCompile(`Keep answers short`).MatchString(got) {
				t.Errorf("the rest of the prompt was dropped: %q", got)
			}
		})
	}
}

func TestRewriteBrandingText_UserTextStaysUntouched(t *testing.T) {
	// A user message that quotes the phrase is the user's own content; only the
	// system instruction is what the backend fingerprints.
	const userText = "You are Claude Code, Anthropic's official CLI for Claude."
	if got := rewriteBrandingText(userText, false); got != userText {
		t.Errorf("user text was rewritten: %q", got)
	}
}

func TestWrapForAntigravity_NeutralisesClaudeCodeIdentity(t *testing.T) {
	geminiBody := []byte(`{"system_instruction":{"role":"user","parts":[{"text":"You are Claude Code, Anthropic's official CLI for Claude.\nKeep answers short."}]},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	wrapped, err := WrapForAntigravity(geminiBody, "proj-1", "gemini-3.8-flash-medium")
	if err != nil {
		t.Fatalf("WrapForAntigravity: %v", err)
	}
	if regexp.MustCompile(`(?i)you are claude code`).Match(wrapped) {
		t.Errorf("the identity sentence reached the wire: %s", wrapped)
	}
	if !regexp.MustCompile(`Keep answers short`).Match(wrapped) {
		t.Errorf("the rest of the system prompt was dropped: %s", wrapped)
	}
}

func TestAntigravityNumericSessionID(t *testing.T) {
	// A numeric id passes through, which is what the Hub itself emits.
	if got := antigravityNumericSessionID("-3750763034362895579"); got != "-3750763034362895579" {
		t.Errorf("numeric id changed: %q", got)
	}
	if got := antigravityNumericSessionID("42"); got != "42" {
		t.Errorf("numeric id changed: %q", got)
	}

	// Anything else folds onto a stable signed number: the same conversation
	// keeps the same id, and the raw value is not echoed.
	const seeded = "aicode-consumers"
	first := antigravityNumericSessionID(seeded)
	if !regexp.MustCompile(`^-\d+$`).MatchString(first) {
		t.Fatalf("derived id = %q, want -<digits>", first)
	}
	if again := antigravityNumericSessionID(seeded); again != first {
		t.Errorf("derived id is not stable: %q vs %q", first, again)
	}
	if other := antigravityNumericSessionID("other-project"); other == first {
		t.Error("two seeds share one id")
	}
	if got := antigravityNumericSessionID(""); got != "" {
		t.Errorf("empty input = %q, want empty", got)
	}
}

func TestWrapForAntigravity_SessionIDIsNumericNotTheProjectName(t *testing.T) {
	geminiBody := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	wrapped, err := WrapForAntigravity(geminiBody, "aicode-consumers", "gemini-3.8-flash-medium")
	if err != nil {
		t.Fatalf("WrapForAntigravity: %v", err)
	}
	var envelope struct {
		Request jsontext.Value `json:"request"`
	}
	if err := json.Unmarshal(wrapped, &envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	var inner map[string]any
	if err := json.Unmarshal(envelope.Request, &inner); err != nil {
		t.Fatalf("decode inner request: %v", err)
	}
	sess, _ := inner["sessionId"].(string)
	if sess == "aicode-consumers" {
		t.Fatalf("sessionId is the project name, want a numeric id: %s", wrapped)
	}
	if !regexp.MustCompile(`^-\d+$`).MatchString(sess) {
		t.Errorf("sessionId = %q, want -<digits>", sess)
	}
}
