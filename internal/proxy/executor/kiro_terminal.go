package executor

import (
	"strings"

	json "panrouter/proxy/internal/fastjson"
)

// Kiro's stop vocabulary → the OpenAI finish reasons this gateway emits.
//
// Upstream models the same set through stopDisposition/mergeStopReason
// (open-sse/executors/kiro.js): a refused turn and a truncated one are not the
// same answer, and a turn that used a tool must say so or the client never
// dispatches the call.
const (
	kiroStopEndTurn   = "end_turn"
	kiroStopToolUse   = "tool_use"
	kiroStopMaxTokens = "max_tokens"
)

// kiroNormalizeStopReason lower-cases the reason Kiro sent and supplies the
// default when it named none: a turn that used a tool ended in tool_use, anything
// else ended normally.
func kiroNormalizeStopReason(reason string, sawToolUse bool) string {
	normalized := strings.ToLower(strings.TrimSpace(reason))
	switch normalized {
	case "":
		if sawToolUse {
			return kiroStopToolUse
		}
		return kiroStopEndTurn
	case "end_turn", "endturn", "stop":
		return kiroStopEndTurn
	case "tool_use", "tooluse", "tool_calls":
		return kiroStopToolUse
	case "max_tokens", "maxtokens", "length", "model_context_window_exceeded":
		return kiroStopMaxTokens
	default:
		return normalized
	}
}

// kiroMergeStopReason keeps the more severe verdict when several events name one.
// A later `end_turn` must not overwrite an earlier `max_tokens`: the turn really
// was cut off, and reporting a clean stop would hide it.
func kiroMergeStopReason(current, incoming string) string {
	if incoming == "" {
		return current
	}
	if current == "" {
		return incoming
	}
	return kiroMoreSevereStop(current, incoming)
}

// kiroMoreSevereStop ranks the reasons by how much they change the meaning of the
// answer: a refusal is loudest, then truncation, then a tool turn, then a plain
// stop.
func kiroMoreSevereStop(a, b string) string {
	rank := func(reason string) int {
		switch reason {
		case "refusal", "content_filter", "guardrail":
			return 4
		case kiroStopMaxTokens:
			return 3
		case "unknown_failure", "malformed_model_output", "invalid_model_output", "cancelled":
			return 2
		case kiroStopToolUse:
			return 1
		default:
			return 0
		}
	}
	if rank(b) > rank(a) {
		return b
	}
	return a
}

// kiroFinishReason maps the merged stop reason onto the OpenAI finish_reason the
// chat chunk carries.
func kiroFinishReason(stopReason string, sawToolCall bool) string {
	if sawToolCall && stopReason == "" {
		return "tool_calls"
	}
	switch stopReason {
	case kiroStopToolUse:
		return "tool_calls"
	case kiroStopMaxTokens:
		return "length"
	case "refusal", "content_filter", "guardrail":
		return "content_filter"
	default:
		if sawToolCall && stopReason == "" {
			return "tool_calls"
		}
		return "stop"
	}
}

// kiroStripThinkingMarkup removes the model's inline <thinking> wrapper from an
// assistant fragment, tracking the open state across fragments: Kiro emits the
// reasoning inside the same event, and the client would otherwise print the tags.
// Ported from the assistantResponseEvent branch of open-sse/executors/kiro.js.
func kiroStripThinkingMarkup(content string, state *kiroStreamState) string {
	const openTag, closeTag = "<thinking>", "</thinking>"
	if state.inThinking {
		end := strings.Index(content, closeTag)
		if end < 0 {
			return ""
		}
		state.inThinking = false
		return strings.TrimPrefix(content[end+len(closeTag):], "\n")
	}
	start := strings.Index(content, openTag)
	if start < 0 {
		return content
	}
	rest := content[start+len(openTag):]
	end := strings.Index(rest, closeTag)
	if end < 0 {
		state.inThinking = true
		return content[:start]
	}
	return strings.TrimPrefix(content[:start]+rest[end+len(closeTag):], "\n")
}

// kiroMetricsTokens reads Kiro's own token counts out of a metricsEvent frame.
// Kiro nests the numbers under `metricsEvent` in some builds and sends them at the
// top level in others, so both are read.
func kiroMetricsTokens(payload []byte) (input, output int, ok bool) {
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return 0, 0, false
	}
	metrics, _ := decoded["metricsEvent"].(map[string]any)
	if metrics == nil {
		metrics = decoded
	}
	input = int(kiroNumField(metrics["inputTokens"]))
	output = int(kiroNumField(metrics["outputTokens"]))
	return input, output, true
}

// kiroNumField reads a numeric JSON field.
func kiroNumField(value any) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case int:
		return float64(typed)
	case int64:
		return float64(typed)
	default:
		return 0
	}
}
