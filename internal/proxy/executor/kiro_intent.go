// Kiro request intent — port of open-sse/config/kiroConstants.js: the model
// suffix fiction, the thinking budget, the agentic system prompt and the native
// effort fields Kiro accepts.
//
// Kiro's data plane has no `thinking` field and no `reasoning_effort`. Reasoning
// is switched on by a magic system-prompt prefix, agentic behaviour by another
// prompt, and the newer Claude/GPT models additionally accept
// `additionalModelRequestFields` (a native effort envelope). All three travel
// inside the conversationState payload, so they belong to the translator's
// options rather than to the executor's headers.
package executor

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"panrouter/proxy/internal/thinking"
)

const (
	// kiroThinkingBudgetDefault is the budget the magic prefix asks for when a
	// client enables reasoning without naming one.
	kiroThinkingBudgetDefault = 16000
	// kiroThinkingBudgetCeiling is what buildThinkingSystemPrefix clamps to.
	kiroThinkingBudgetCeiling = 32000

	kiroAgenticSuffix  = "-agentic"
	kiroThinkingSuffix = "-thinking"
)

// kiroAgenticSystemPrompt is the chunked-write protocol the `-agentic` models
// carry. Large single writes are what make Kiro's server time out, so the prompt
// is part of the model's behaviour, not decoration.
const kiroAgenticSystemPrompt = `# CRITICAL: CHUNKED WRITE PROTOCOL (MANDATORY)

You MUST follow these rules for ALL file operations. Violation causes server timeouts and task failure.

## ABSOLUTE LIMITS
- **MAXIMUM 350 LINES** per single write/edit operation - NO EXCEPTIONS
- **RECOMMENDED 300 LINES** or less for optimal performance
- **NEVER** write entire files in one operation if >300 lines

## MANDATORY CHUNKED WRITE STRATEGY

### For NEW FILES (>300 lines total):
1. FIRST: Write initial chunk (first 250-300 lines) using write_to_file/fsWrite
2. THEN: Append remaining content in 250-300 line chunks using file append operations
3. REPEAT: Continue appending until complete

### For EDITING EXISTING FILES:
1. Use surgical edits (apply_diff/targeted edits) - change ONLY what's needed
2. NEVER rewrite entire files - use incremental modifications
3. Split large refactors into multiple small, focused edits

### For LARGE CODE GENERATION:
1. Generate in logical sections (imports, types, functions separately)
2. Write each section as a separate operation
3. Use append operations for subsequent sections

## EXAMPLES OF CORRECT BEHAVIOR

CORRECT: Writing a 600-line file
- Operation 1: Write lines 1-300 (initial file creation)
- Operation 2: Append lines 301-600

CORRECT: Editing multiple functions
- Operation 1: Edit function A
- Operation 2: Edit function B
- Operation 3: Edit function C

WRONG: Writing 500 lines in single operation -> TIMEOUT
WRONG: Rewriting entire file to change 5 lines -> TIMEOUT
WRONG: Generating massive code blocks without chunking -> TIMEOUT

## WHY THIS MATTERS
- Server has 2-3 minute timeout for operations
- Large writes exceed timeout and FAIL completely
- Chunked writes are FASTER and more RELIABLE
- Failed writes waste time and require retry

REMEMBER: When in doubt, write LESS per operation. Multiple small operations > one large operation.`

// kiroIntent is the resolved request: the upstream model id plus the two suffix
// fictions, which are stripped before the request leaves this process.
type kiroIntent struct {
	Upstream string
	Agentic  bool
	Thinking bool
	Override *thinking.Intent
}

// kiroResolveIntent consumes the `-agentic` / `-thinking` suffixes and the
// generic `model(level)` override.
//
// The order matters: `claude-sonnet-4.5-thinking-agentic` ends in `-agentic`, so
// stripping `-thinking` first leaves `-thinking` in the upstream id and Kiro
// answers 400 INVALID_MODEL_ID. Upstream strips agentic, then thinking, then
// folds the Anthropic spelling onto the dotted alias.
func kiroResolveIntent(model string) kiroIntent {
	clean, override := thinking.ParseSuffix(model)
	intent := kiroIntent{Override: override}
	upstream := clean
	if strings.HasSuffix(upstream, kiroAgenticSuffix) {
		intent.Agentic = true
		upstream = strings.TrimSuffix(upstream, kiroAgenticSuffix)
	}
	if strings.HasSuffix(upstream, kiroThinkingSuffix) {
		intent.Thinking = true
		upstream = strings.TrimSuffix(upstream, kiroThinkingSuffix)
	}
	intent.Upstream = normalizeKiroUpstreamModelId(upstream)
	return intent
}

// kiroContentPrefix composes the system material that rides inside the first user
// turn: the thinking switch, the agentic protocol, then the client's own prefix.
// The CodeWhisperer surface rejects a top-level systemPrompt, so this is the only
// place a system prompt can travel.
func kiroContentPrefix(intent kiroIntent, body map[string]any, headers http.Header, clientPrefix string) string {
	parts := make([]string, 0, 3)
	if budget := kiroThinkingBudget(intent, body, headers); budget != nil {
		// A GPT model that carries its effort natively must not also get the magic
		// prefix: the two together ask for reasoning twice and the CLI does not do
		// both (upstream usesKiroNativeGptEffort).
		if !kiroUsesNativeGPTEffort(body, intent.Upstream) {
			parts = append(parts, kiroThinkingSystemPrefix(*budget))
		}
	}
	if intent.Agentic {
		parts = append(parts, kiroAgenticSystemPrompt)
	}
	if strings.TrimSpace(clientPrefix) != "" {
		parts = append(parts, clientPrefix)
	}
	return strings.Join(parts, "\n\n")
}

// kiroThinkingBudget resolves the budget a client asked for, or nil when
// reasoning is off.
func kiroThinkingBudget(intent kiroIntent, body map[string]any, headers http.Header) *int {
	effective := body
	if intent.Override != nil {
		effective = kiroApplyThinkingOverride(body, intent.Override)
	}
	if cfg := thinking.Extract(effective); cfg != nil {
		if cfg.Mode == thinking.ModeNone {
			return nil
		}
		if cfg.Mode == thinking.ModeLevel && cfg.Level == "disabled" {
			return nil
		}
		if cfg.Mode == thinking.ModeBudget {
			return new(cfg.Budget)
		}
		if cfg.Mode == thinking.ModeLevel {
			if budget, ok := thinking.EffortToBudget(cfg.Level); ok {
				return new(budget)
			}
			return new(kiroThinkingBudgetDefault)
		}
		return new(kiroThinkingBudgetDefault)
	}
	// Claude Code asks for interleaved thinking with a beta header rather than a
	// body field, and some clients enable reasoning through a prompt tag.
	if h := headerValue(headers, "anthropic-beta"); strings.Contains(strings.ToLower(h), "interleaved-thinking") {
		return new(kiroThinkingBudgetDefault)
	}
	if kiroBodyMentionsThinkingMode(body) {
		return new(kiroThinkingBudgetDefault)
	}
	// The `-thinking` suffix is the last signal: the client named the variant, so
	// it wants reasoning even though the body carries no field.
	if intent.Thinking || strings.Contains(strings.ToLower(intent.Upstream), "thinking") || strings.Contains(strings.ToLower(intent.Upstream), "-reason") {
		return new(kiroThinkingBudgetDefault)
	}
	return nil
}

// kiroApplyThinkingOverride folds a `model(level)` suffix into the body without
// mutating the caller's map.
func kiroApplyThinkingOverride(body map[string]any, override *thinking.Intent) map[string]any {
	if override == nil {
		return body
	}
	next := make(map[string]any, len(body)+2)
	for k, v := range body {
		next[k] = v
	}
	if override.Mode == thinking.ModeBudget {
		delete(next, "output_config")
		delete(next, "reasoning_effort")
		delete(next, "reasoning")
		next["thinking"] = map[string]any{"type": "enabled", "budget_tokens": override.Budget}
		return next
	}
	effort := string(override.Mode)
	if override.Mode == thinking.ModeLevel {
		effort = override.Level
	}
	outputConfig, _ := body["output_config"].(map[string]any)
	merged := make(map[string]any, len(outputConfig)+1)
	for k, v := range outputConfig {
		merged[k] = v
	}
	merged["effort"] = effort
	next["output_config"] = merged
	return next
}

// kiroThinkingSystemPrefix is the magic turn-on prefix (CLIProxyAPIPlus shape).
func kiroThinkingSystemPrefix(budget int) string {
	if budget <= 0 {
		budget = kiroThinkingBudgetDefault
	}
	if budget > kiroThinkingBudgetCeiling {
		budget = kiroThinkingBudgetCeiling
	}
	return "<thinking_mode>enabled</thinking_mode>\n<max_thinking_length>" + strconv.Itoa(budget) + "</max_thinking_length>"
}

var (
	kiroGpt56Pattern  = regexp.MustCompile(`(?:^|[/.])gpt[/.]5[/.]6(?:[/.]|$)`)
	kiroClaudePattern = regexp.MustCompile(`(?:^|[/.])claude(?:[/.][a-z]+)*[/.](\d+)(?:[/.](\d+))?(?:[/.]|$)`)
)

// kiroEffortPath names the native effort envelope a model understands:
// "reasoning" for GPT 5.6, "output_config" for Claude 4.6+, and "" for the rest.
// Legacy Claude 4.5 models rejected additionalModelRequestFields in live smoke.
func kiroEffortPath(model string) string {
	normalized := strings.ReplaceAll(strings.ToLower(model), "-", ".")
	if kiroGpt56Pattern.MatchString(normalized) {
		return "reasoning"
	}
	if !strings.Contains(normalized, "claude") {
		return ""
	}
	match := kiroClaudePattern.FindStringSubmatch(normalized)
	if match == nil {
		return ""
	}
	major, _ := strconv.Atoi(match[1])
	minor := -1
	if match[2] != "" {
		minor, _ = strconv.Atoi(match[2])
	}
	dateSuffixMinor := minor >= 1000
	if major < 4 || (major == 4 && (minor < 0 || minor <= 5 || dateSuffixMinor)) {
		return ""
	}
	return "output_config"
}

// kiroAdditionalFields builds `additionalModelRequestFields`, or nil when the
// model takes none or the client asked for no effort.
func kiroAdditionalFields(body map[string]any, model string) map[string]any {
	switch kiroEffortPath(model) {
	case "reasoning":
		if effort, ok := kiroGPTEffortLevel(body); ok {
			return map[string]any{"reasoning": map[string]any{"effort": effort}}
		}
	case "output_config":
		if effort, ok := kiroEffortLevel(body); ok {
			return map[string]any{
				"thinking":      map[string]any{"type": "adaptive", "display": "summarized"},
				"output_config": map[string]any{"effort": effort},
			}
		}
	}
	return nil
}

// kiroUsesNativeGPTEffort reports whether the body already names a GPT effort,
// which is what suppresses the magic prefix.
func kiroUsesNativeGPTEffort(body map[string]any, model string) bool {
	if kiroEffortPath(model) != "reasoning" {
		return false
	}
	_, ok := kiroGPTEffortLevel(body)
	return ok
}

// kiroEffortLevel reads the Claude-side effort: xhigh/max fold onto high.
func kiroEffortLevel(body map[string]any) (string, bool) {
	raw := kiroRawEffort(body)
	if raw == "" {
		return "", false
	}
	switch normalized := strings.ToLower(raw); normalized {
	case "none", "off", "disabled":
		return "", false
	case "xhigh", "max":
		return "high", true
	case "low", "medium", "high":
		return normalized, true
	}
	return "", false
}

// kiroGPTEffortLevel reads the GPT-side effort: max maps to xhigh, and there is
// no explicit "none" wire value in the CLI.
func kiroGPTEffortLevel(body map[string]any) (string, bool) {
	raw := kiroRawEffort(body)
	if raw == "" {
		return "", false
	}
	switch normalized := strings.ToLower(raw); normalized {
	case "max":
		return "xhigh", true
	case "low", "medium", "high", "xhigh":
		return normalized, true
	}
	return "", false
}

// kiroRawEffort reads the effort field in every client dialect, first match wins.
func kiroRawEffort(body map[string]any) string {
	if oc, ok := body["output_config"].(map[string]any); ok {
		if s, ok := oc["effort"].(string); ok && s != "" {
			return s
		}
	}
	if s, ok := body["reasoning_effort"].(string); ok && s != "" {
		return s
	}
	if r, ok := body["reasoning"].(map[string]any); ok {
		if s, ok := r["effort"].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// kiroBodyMentionsThinkingMode detects the legacy <thinking_mode> prompt tag.
func kiroBodyMentionsThinkingMode(body map[string]any) bool {
	messages, _ := body["messages"].([]any)
	for _, raw := range messages {
		msg, _ := raw.(map[string]any)
		if content, ok := msg["content"].(string); ok && strings.Contains(content, "<thinking_mode>") {
			return true
		}
	}
	return false
}

// headerValue reads a header case-insensitively from an inbound request.
func headerValue(headers http.Header, name string) string {
	if headers == nil {
		return ""
	}
	return headers.Get(name)
}
