package translator

import (
	json "encoding/json/v2"
	"strings"
)

// The Antigravity lane's prompting shape, as the SHIPPED client sends it.
//
// The authority is the language server the IDE installs
// (AppData/Local/Programs/antigravity/resources/bin/language_server.exe, Hub
// 2.32.1), MITM-captured while it answered a turn on 2026-11-17. Its agent
// turn carries:
//
//   - a system instruction whose FIRST section is the client's own identity:
//
//     <identity>
//     You are Antigravity, a powerful agentic AI assistant designed by the Google Deepmind team.
//     You are working with a USER to solve their task.
//     The USER will send you requests, which you must always prioritize addressing. User requests are enclosed within <USER_REQUEST> tags.
//     </identity>
//
//   - a user turn whose text is wrapped in those tags:
//
//     <USER_REQUEST>
//     Say exactly: hi
//     </USER_REQUEST>
//
// Why this matters beyond fidelity: Google's backend fingerprints the CALLER'S
// IDENTITY in the system prompt. Measured live on one account and one model,
// only the system prompt varying:
//
//	"You are Claude Code."                                              -> 429 RESOURCE_EXHAUSTED
//	"you are claude code, anthropic's official cli for claude."          -> 429 RESOURCE_EXHAUSTED
//	"You are Claude Code, Anthropic's official CLI for Claude."          -> 429 RESOURCE_EXHAUSTED
//	"You are an assistant built as Anthropic's official CLI for Claude." -> 200
//	"You are a Claude agent, built on Anthropic's Claude Agent SDK."     -> 200
//	"x-anthropic-billing-header: ..."                                    -> 200
//	"You like the name Claude." / "You are opencode ..."                 -> 200
//
// The same account served a 200 for a neutral prompt seconds before each
// refusal, and its weekly bucket still showed 61% remaining — so the 429 is a
// filtering verdict, not quota. Sending the lane's OWN identity first, the way
// the shipped client does, is what keeps a request that carries a client's
// system prompt from being read as a competing client.
//
// The upstream reference does NOT inject a prompt of its own here
// (tests/translator/bugs-antigravity.test.js: "does not inject the legacy
// Antigravity default system prompt"), and it rewrites competing branding
// instead (ANTIGRAVITY_PROMPT_REWRITES). Both are kept: the rewrite stays for
// the sentences upstream names, and this identity block is what the client
// itself sends.

// AntigravityIdentityPrompt is the identity block the shipped client opens
// its system instruction with.
const AntigravityIdentityPrompt = "<identity>\n" +
	"You are Antigravity, a powerful agentic AI assistant designed by the Google Deepmind team.\n" +
	"You are working with a USER to solve their task.\n" +
	"The USER will send you requests, which you must always prioritize addressing. User requests are enclosed within <USER_REQUEST> tags.\n" +
	"</identity>"

// antigravityUserRequestOpen and antigravityUserRequestClose are the tags the
// shipped client wraps a user turn in.
const (
	antigravityUserRequestOpen  = "<USER_REQUEST>\n"
	antigravityUserRequestClose = "\n</USER_REQUEST>"
)

// antigravityApplyPromptShape puts the lane's prompting shape on the request:
// the identity block opens the system instruction, and a user turn's text is
// wrapped in the tags the client uses.
//
// The caller's own system text is KEPT, after the identity block: the client
// sends its identity plus whatever the workspace adds, and dropping the
// caller's instructions would change what the model is asked to do.
//
// A user turn that carries a functionResponse is left alone: it is the answer
// to a tool call, not a request from the user, and wrapping it would put the
// tags inside the tool result.
//
// The system instruction also moves to the key the Cloud Code API publishes —
// `systemInstruction`, camelCase, as the shipped client and the reference both
// send it. The plain Gemini API accepts the snake_case spelling too, but this
// lane speaks the Cloud Code dialect, and the envelope around it
// (generationConfig, toolConfig, sessionId) is camelCase as well.
func antigravityApplyPromptShape(geminiBody []byte) []byte {
	var m map[string]any
	if err := json.Unmarshal(geminiBody, &m); err != nil {
		return geminiBody
	}
	changed := false

	// The system instruction: the identity block first, the caller's own text
	// after it.
	if si, ok := m["system_instruction"].(map[string]any); ok {
		parts, _ := si["parts"].([]any)
		if len(parts) > 0 {
			texts := make([]string, 0, len(parts))
			for _, raw := range parts {
				p, _ := raw.(map[string]any)
				if p == nil {
					continue
				}
				if t, ok := p["text"].(string); ok {
					texts = append(texts, t)
				}
			}
			if len(texts) > 0 {
				joined := strings.Join(texts, "\n")
				if !strings.HasPrefix(joined, "<identity>") {
					si["parts"] = []any{map[string]any{"text": AntigravityIdentityPrompt + joined}}
					changed = true
				}
			}
		}
		delete(m, "system_instruction")
		m["systemInstruction"] = si
		changed = true
	}

	// The user turn: the request tags.
	if contents, ok := m["contents"].([]any); ok {
		for _, raw := range contents {
			c, _ := raw.(map[string]any)
			if c == nil {
				continue
			}
			role, _ := c["role"].(string)
			if role != "user" {
				continue
			}
			parts, _ := c["parts"].([]any)
			for i, rawPart := range parts {
				p, _ := rawPart.(map[string]any)
				if p == nil {
					continue
				}
				if _, isResponse := p["functionResponse"]; isResponse {
					continue
				}
				text, ok := p["text"].(string)
				if !ok || text == "" {
					continue
				}
				if strings.HasPrefix(text, antigravityUserRequestOpen) {
					continue
				}
				p["text"] = antigravityUserRequestOpen + text + antigravityUserRequestClose
				parts[i] = p
				changed = true
			}
			c["parts"] = parts
		}
		m["contents"] = contents
	}

	if !changed {
		return geminiBody
	}
	out, err := json.Marshal(m, json.Deterministic(true))
	if err != nil {
		return geminiBody
	}
	return out
}
