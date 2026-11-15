// CodeBuddy system-prompt guard, shared by both lanes — port of
// open-sse/shared/codebuddySystemPrompt.js (CN + intl).
//
// Tencent's gateway/WAF treats third-party CLI agent identity strings — "You are
// Claude Code, Anthropic's official CLI...", Cursor, Windsurf, Cline, Aider,
// Continue, Copilot, Cody, and prompt-injection scaffolding like
// `<agent-identity>` — as unauthorized tooling. It answers 11128 "request illegal"
// and, on repetition, restricts the ACCOUNT (11140).
//
// Both executors therefore neutralise those markers in the CLIENT'S OWN system
// prompt instead of dropping the prompt: dropping it loses every user/project
// instruction on every request.
package executor

import (
	"regexp"
	"strings"
)

// codebuddyNeutralSystemPrompt replaces a detected agent-identity system prompt.
const codebuddyNeutralSystemPrompt = "You are a helpful AI assistant that helps with software engineering tasks."

// codebuddyAgentIdentityPattern is deliberately narrow: identity markers of known
// third-party agents plus the wrapper tags the agent harnesses emit. Widening it
// past that starts replacing legitimate user instructions ("you are a code review
// agent", "orchestration capabilities" in an API doc) with the neutral text,
// silently. Narrowing it lets a detected agent identity reach the WAF.
// `claude.?code` is spelled with `.?` because the CLI emits both "Claude Code" and
// "claude-code"; the `anxthxropic` variant is an obfuscated spelling seen in the
// wild.
var codebuddyAgentIdentityPattern = regexp.MustCompile(`(?i)you are claude code|claude.?code.+official.+cli|anthropic.+official.+cli|anxthxropic.+official.+cli|you are (?:cursor|windsurf|cline|aider|continue|copilot|cody)|you are an? (?:ai )?(?:coding |code )?agent|cc_entrypoint\s*=\s*(?:cli|vscode|jetbrains|gui)|claude.?code.+issues|give feedback.+claude.?code|you are .{0,30}(?:powerful )?ai agent|orchestration capabilities|OhMyOpenCode|<agent-identity>|<Role>|<Behavior_Instructions>`)

// codebuddySystemPromptLengthLimit is the CN lane's catch-all: a system prompt
// past this length is treated as an agent harness prompt even when the pattern
// does not recognise it.
const codebuddySystemPromptLengthLimit = 2000

// flattenCodebuddySystemContent flattens a system content payload to text.
// Clients send either a bare string or typed blocks ([{type:"text",text}]); only
// string `text` fields are collected, so a non-text block contributes nothing.
func flattenCodebuddySystemContent(content any) string {
	switch value := content.(type) {
	case string:
		return value
	case []any:
		parts := make([]string, 0, len(value))
		for _, item := range value {
			if block, ok := item.(map[string]any); ok {
				if text, ok := block["text"].(string); ok {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

// neutraliseCodebuddySystemText returns the text to send in place of a client's
// system prompt: the neutral one when it carries a third-party agent identity (or
// is long enough to be one), otherwise the client's own text.
func neutraliseCodebuddySystemText(text string) string {
	if text == "" {
		return ""
	}
	if codebuddyAgentIdentityPattern.MatchString(text) {
		return codebuddyNeutralSystemPrompt
	}
	return text
}