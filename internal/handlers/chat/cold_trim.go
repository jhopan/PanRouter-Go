// CodeBuddy Intl cold-account trim — port of
// open-sse/services/messageTrimmer.js and its caller in src/sse/handlers/chat.js.
//
// Why this exists: CodeBuddy's prompt cache is bound to the ACCOUNT. When the
// router rotates to a different account mid-request, that account has no cache for
// this conversation, so the FULL history is uploaded — 412k tokens in one request.
// That payload shape is what gets accounts flagged as automation, and it is also
// why a cold account is slower than a warm one.
//
// So: when an attempt lands on a different account than the one that just ran, the
// oldest messages are dropped until the estimated token count fits the budget. The
// conversation stays coherent — the system prompt and the recent turns are kept —
// and the cold request is no longer a 412k-token upload. It fires once per
// request; trimming again would keep eating history.
//
// The estimate is deliberately crude (characters/4): it is only deciding whether to
// trim, and it never touches the body when nothing needs trimming.
package chat

import (
	"fmt"

	json "panrouter/proxy/internal/fastjson"
	"panrouter/proxy/internal/log"
	"panrouter/proxy/internal/providers"
)

const (
	// codebuddyColdTrimBudgetTokens is upstream TRIM_BUDGET_TOKENS.
	codebuddyColdTrimBudgetTokens = 200_000
	// codebuddyCharsPerToken is the crude prose+code ratio the estimate uses.
	codebuddyCharsPerToken = 4
	// codebuddyTrimmedMessageChars and codebuddyTrimmedToolCallChars keep the
	// accounting for parts whose real cost cannot be known from the body (a base64
	// image is far more than its JSON length) from reading as free.
	codebuddyTrimmedMessageChars  = 4000
	codebuddyTrimmedToolCallChars = 400
)

// codebuddyTrimResult reports what a trim did, in the vocabulary upstream logs.
type codebuddyTrimResult struct {
	removed int
	before  int
	after   int
}

// codebuddyEstimateTokens is the port of estimateTokens: characters/4 over the
// message list, with a flat cost for tool calls and images.
func codebuddyEstimateTokens(messages []any) int {
	return (codebuddyMessageChars(messages) + codebuddyCharsPerToken - 1) / codebuddyCharsPerToken
}

// codebuddyMessageChars counts the characters one message contributes.
func codebuddyMessageChars(messages []any) int {
	chars := 0
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		chars += codebuddyContentChars(message["content"])
		if id, ok := message["tool_call_id"].(string); ok {
			chars += len(id)
		}
		if _, hasCalls := message["tool_calls"]; hasCalls {
			chars += codebuddyTrimmedToolCallChars
		}
	}
	return chars
}

// codebuddyContentChars counts one content payload: a string, typed blocks, or a
// single part.
func codebuddyContentChars(content any) int {
	switch value := content.(type) {
	case string:
		return len(value)
	case []any:
		chars := 0
		for _, raw := range value {
			switch part := raw.(type) {
			case string:
				chars += len(part)
			case map[string]any:
				if text, ok := part["text"].(string); ok {
					chars += len(text)
					continue
				}
				// A base64 image is far more than its JSON length in tokens, but the
				// real cost cannot be known without decoding it; count it so a history
				// full of images is trimmed too.
				if _, hasImage := part["image_url"]; hasImage {
					chars += codebuddyTrimmedMessageChars
				} else if _, hasImage := part["image"]; hasImage {
					chars += codebuddyTrimmedMessageChars
				}
			}
		}
		return chars
	default:
		return 0
	}
}

// codebuddyTrimMessagesToBudget drops the oldest non-system messages until the
// history fits budgetTokens.
//
// The system prompt (and any leading system/developer turns) is always kept — it
// carries the instructions. Whole turns are removed from the front so the
// conversation never starts mid-tool-call: a dropped assistant turn takes its tool
// results with it, and the newest message survives even a nonsensical budget.
func codebuddyTrimMessagesToBudget(messages []any, budgetTokens int) ([]any, codebuddyTrimResult) {
	before := codebuddyEstimateTokens(messages)
	result := codebuddyTrimResult{before: before, after: before}
	if budgetTokens <= 0 || before <= budgetTokens {
		return messages, result
	}

	head := 0
	for head < len(messages) {
		message, ok := messages[head].(map[string]any)
		if !ok {
			break
		}
		role, _ := message["role"].(string)
		if role != "system" && role != "developer" {
			break
		}
		head++
	}
	systemPart := messages[:head]
	rest := messages[head:]

	used := codebuddyMessageChars(rest)
	budgetChars := budgetTokens * codebuddyCharsPerToken
	start := 0
	for start < len(rest)-1 && used > budgetChars {
		if message, ok := rest[start].(map[string]any); ok {
			used -= codebuddyContentChars(message["content"])
			if id, ok := message["tool_call_id"].(string); ok {
				used -= len(id)
			}
			if _, hasCalls := message["tool_calls"]; hasCalls {
				used -= codebuddyTrimmedToolCallChars
			}
		}
		start++
		// Never leave a dangling assistant tool_call without its tool result.
		for start < len(rest) {
			next, ok := rest[start].(map[string]any)
			if !ok {
				break
			}
			if role, _ := next["role"].(string); role != "tool" {
				break
			}
			used -= codebuddyContentChars(next["content"])
			start++
		}
	}

	kept := append(append([]any{}, systemPart...), rest[start:]...)
	result.removed = len(rest) - len(rest[start:])
	result.after = codebuddyEstimateTokens(kept)
	return kept, result
}

// codebuddyColdAccountTrim is the caller's half: it trims the body's messages when
// an attempt lands on a different account than the one that just ran, and reports
// whether it changed anything.
//
// Only codebuddy-intl is in scope (upstream's own guard): the CN lane's cache
// behaviour differs, and widening this would rewrite histories for lanes that never
// asked for it.
func codebuddyColdAccountTrim(provider, model string, body []byte, previousConnID, connID string) ([]byte, bool) {
	// The lane guard lives here as well as at the call site: the CN lane's cache
	// behaviour differs, and a future caller must not rewrite its histories by
	// accident.
	if providers.ResolveAlias(provider) != "codebuddy-intl" {
		return body, false
	}
	if previousConnID == "" || previousConnID == connID || len(body) == 0 {
		return body, false
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return body, false
	}
	messages, ok := parsed["messages"].([]any)
	if !ok || len(messages) == 0 {
		return body, false
	}
	trimmed, result := codebuddyTrimMessagesToBudget(messages, codebuddyColdTrimBudgetTokens)
	if result.removed == 0 {
		return body, false
	}
	parsed["messages"] = trimmed
	encoded, err := json.Marshal(parsed)
	if err != nil {
		return body, false
	}
	log.Warn("CHAT", fmt.Sprintf("[%s/%s] cold account (%s) — trimmed %d oldest messages, %d → %d tokens",
		provider, model, shortConnID(connID), result.removed, result.before, result.after))
	return encoded, true
}
