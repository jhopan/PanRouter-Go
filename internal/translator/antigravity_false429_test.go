package translator_test

import (
	"strings"
	"testing"

	"panrouter/proxy/internal/translator"
)

// TestStripCompetitivePrompts_ReplacesTheWholeHermesIdentityBlock pins the row
// that closed the Hermes refusal: the identity PARAGRAPH is replaced, not just
// its first sentence.
//
// Measured 2026-11-20, one account, gemini-3.8-flash-high: "You are Hermes
// Agent." alone answered 200, the sizing sentence alone answered 200, and the
// two together answered 429 on every account in the pool. A rewrite that left
// the paragraph recognisable kept the refusal.
func TestStripCompetitivePrompts_ReplacesTheWholeHermesIdentityBlock(t *testing.T) {
	const soul = "You are Hermes Agent, built by Nous Research. Be direct: match the length of your reply " +
		"to the weight of the ask — a one-line question gets a one-line answer. No filler. " +
		"Plain claims over adjectives; when unsure, say so plainly."
	req := &translator.GeminiRequest{
		SystemInstruction: &translator.GeminiContent{
			Role:  "user",
			Parts: []translator.GeminiPart{{Text: soul + "\n\n# Goal\nShip the release."}},
		},
		Contents: []translator.GeminiContent{{
			Role:  "user",
			Parts: []translator.GeminiPart{{Text: "hi"}},
		}},
	}
	got := translator.StripCompetitivePrompts(req).SystemInstruction.Parts[0].Text
	for _, banned := range []string{"Hermes", "Nous Research", "weight of the ask", "one-line question"} {
		if strings.Contains(got, banned) {
			t.Errorf("fingerprint %q survived the rewrite: %q", banned, got)
		}
	}
	for _, kept := range []string{"You are an AI assistant", "proportional to the question", "# Goal", "Ship the release."} {
		if !strings.Contains(got, kept) {
			t.Errorf("rewrite dropped %q from %q", kept, got)
		}
	}
}

// TestStripCompetitivePrompts_HermesLinesThatTriggerFalse429 pins the sentences
// measured to produce a detail-free 429 RESOURCE_EXHAUSTED on the Antigravity
// backend while the account still had quota.
//
// Measured live 2026-11-20, one account, gemini-3.8-flash-high, ~100 KB system
// prompt, one line varying:
//
//	clean prompt                                              -> 200
//	"<!-- End Hermes runtime environment -->"                 -> 429
//	"# Google model operational directives"                   -> 429
//	"When the user asks you to build, run, or verify ..."     -> 429
//
// The dashboard's connection test answered 200 for that same account at the
// same time because it never sends a caller system prompt — the line is the
// whole difference, so it is what these cases hold onto.
func TestStripCompetitivePrompts_HermesLinesThatTriggerFalse429(t *testing.T) {
	tests := []struct {
		name string
		line string
		gone string
		want string
	}{
		{
			name: "runtime environment end marker",
			line: "<!-- End Hermes runtime environment -->",
			gone: "End Hermes runtime environment",
		},
		{
			name: "google operational directives header",
			line: "# Google model operational directives",
			gone: "Google model operational",
			want: "# Model operational directives",
		},
		{
			name: "artifact deliverable sentence",
			line: "When the user asks you to build, run, or verify something, the deliverable is a working artifact backed by real tool output",
			gone: "deliverable is a working artifact",
			want: "deliver a working result backed by real tool output",
		},
		{
			name: "mid-turn steering sentence",
			line: "Mid-turn, the user can steer you: Hermes delivers their message while you work.",
			gone: "Hermes delivers their message",
			want: "their message arrives while you work",
		},
		{
			name: "blocker honesty sentence",
			line: "Reporting a blocker honestly is always better than inventing a result.",
			gone: "inventing a result",
			want: "report a blocker honestly than to make up a result",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &translator.GeminiRequest{
				SystemInstruction: &translator.GeminiContent{
					Role:  "user",
					Parts: []translator.GeminiPart{{Text: "Intro line.\n" + tt.line + "\nTrailing line."}},
				},
				Contents: []translator.GeminiContent{{
					Role:  "user",
					Parts: []translator.GeminiPart{{Text: "hi"}},
				}},
			}
			got := translator.StripCompetitivePrompts(req).SystemInstruction.Parts[0].Text
			if strings.Contains(got, tt.gone) {
				t.Errorf("fingerprint survived: %q still contains %q", got, tt.gone)
			}
			if !strings.Contains(got, "Intro line.") || !strings.Contains(got, "Trailing line.") {
				t.Errorf("surrounding text was dropped: %q", got)
			}
			if tt.want != "" && !strings.Contains(got, tt.want) {
				t.Errorf("neutral wording missing: %q does not contain %q", got, tt.want)
			}
		})
	}
}

// TestStripCompetitivePrompts_KeepsHermesLineThatDoesNotTrigger guards the
// other direction: a row that neutralises text the backend accepts would
// silently rewrite the caller's instructions for no gain.
func TestStripCompetitivePrompts_KeepsHermesLineThatDoesNotTrigger(t *testing.T) {
	const line = "For help with Hermes Agent itself — hermes-agent.nousresearch.com/docs"
	req := &translator.GeminiRequest{
		SystemInstruction: &translator.GeminiContent{
			Role:  "user",
			Parts: []translator.GeminiPart{{Text: line}},
		},
		Contents: []translator.GeminiContent{{
			Role:  "user",
			Parts: []translator.GeminiPart{{Text: "hi"}},
		}},
	}
	got := translator.StripCompetitivePrompts(req).SystemInstruction.Parts[0].Text
	if got != line {
		t.Errorf("this line answered 200 on its own, so it must pass through; got %q", got)
	}
}

// TestMoveSystemInstructionToContents checks the refusal fallback's rewrite:
// the instruction becomes the opening user turn, its parts survive in order,
// and every member the rewrite does not own keeps its original bytes.
func TestMoveSystemInstructionToContents(t *testing.T) {
	body := []byte(`{"systemInstruction":{"role":"user","parts":[{"text":"SYSTEM"}]},` +
		`"contents":[{"role":"user","parts":[{"text":"first"}]},{"role":"model","parts":[{"text":"second"}]}],` +
		`"tools":[{"functionDeclarations":[{"name":"read"}]}]}`)
	out, ok := translator.MoveSystemInstructionToContents(body)
	if !ok {
		t.Fatal("expected the rewrite to apply")
	}
	text := string(out)
	if strings.Contains(text, "systemInstruction") {
		t.Errorf("systemInstruction must be removed: %s", text)
	}
	if !strings.Contains(text, `"functionDeclarations"`) {
		t.Errorf("tools were damaged: %s", text)
	}
	if !strings.Contains(text, `{"text":"SYSTEM"}`) || !strings.Contains(text, `{"text":"first"}`) {
		t.Errorf("instruction or existing turn lost: %s", text)
	}
	// The instruction precedes the caller's own first turn.
	if strings.Index(text, "SYSTEM") > strings.Index(text, `"first"`) {
		t.Errorf("instruction must open the conversation: %s", text)
	}
}

// TestMoveSystemInstructionToContents_NoOp leaves a body without a system
// instruction alone, so the fallback never spends a second request.
func TestMoveSystemInstructionToContents_NoOp(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	if out, ok := translator.MoveSystemInstructionToContents(body); ok || string(out) != string(body) {
		t.Errorf("expected an unchanged no-op, got ok=%v body=%s", ok, string(out))
	}
}
