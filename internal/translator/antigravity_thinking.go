package translator

import (
	json "encoding/json/v2"
	"strings"
)

// AntigravityThinkingFor writes the thinking intent for one Antigravity turn,
// from the client's own request.
//
// The level is the canonical form and the wire form follows the model's family:
// the Gemini families carry thinkingLevel, the Claude family and gpt-oss carry
// thinkingBudget (the shipped client's own turns, Hub 2.32.1, captured
// 2026-11-17).
//
// The level comes from the client's own effort when it asked for one, and from
// the model's own tier otherwise: `gemini-3.8-flash-high` names the tier the
// shipped client sends (HIGH), and a tiered id names the top tier (the client
// sends HIGH for gemini-3.8-flash-tiered).
//
// openaiBody is the client's request as it arrived; geminiBody is the Gemini
// body it was translated into. Both are needed: the intent lives in the
// client's body (reasoning_effort, thinking, thinking_budget) and the config
// lives in the Gemini body.
func AntigravityThinkingFor(geminiBody, openaiBody []byte, wireModel string) []byte {
	_, known := AntigravityThinkingDialectFor(wireModel)
	if !known {
		return geminiBody
	}
	effort := antigravityRequestedEffort(openaiBody)
	level, _ := antigravityThinkingChoice(wireModel, effort)

	var m map[string]any
	if err := json.Unmarshal(geminiBody, &m); err != nil {
		return geminiBody
	}
	gc, _ := m["generationConfig"].(map[string]any)
	if gc == nil {
		gc = map[string]any{}
	}
	gc["thinkingConfig"] = map[string]any{
		"thinkingLevel":   level,
		"includeThoughts": level != "minimal",
	}
	m["generationConfig"] = gc
	out, err := json.Marshal(m, json.Deterministic(true))
	if err != nil {
		return geminiBody
	}
	return out
}

// antigravityApplyWireThinking puts the thinkingConfig on the wire in the
// dialect the model family speaks.
//
// The shipped client always sends one on an agent turn, and it is the model's
// own published shape:
//
//	gemini-3.8-flash-tiered   {"thinkingLevel":"HIGH","includeThoughts":true}
//	gemini-3.8-flash-medium   {"thinkingLevel":"MEDIUM","includeThoughts":true}
//	gemini-3.8-flash-low      {"thinkingLevel":"LOW","includeThoughts":true}
//	gemini-2.5-flash          {"thinkingLevel":"LOW","includeThoughts":true}
//	claude-sonnet-4-6         {"thinkingBudget":1024,"includeThoughts":true}
//	claude-opus-4-6-thinking  {"thinkingBudget":1024,"includeThoughts":true}
//	gpt-oss-120b-medium       {"thinkingBudget":8192,"includeThoughts":true}
//
// The Claude family therefore speaks the BUDGET dialect on this lane, not the
// level dialect the Gemini families use. A translator that emits
// `thinkingLevel` for a Claude model sends a field the backend does not read
// for it, and the turn runs without the reasoning budget it asked for.
//
// A body with no thinking at all gets the model's own published default, which
// is what the shipped client sends for that model.
func antigravityApplyWireThinking(geminiBody []byte, wireModel string) []byte {
	dialect, known := AntigravityThinkingDialectFor(wireModel)
	if !known {
		return geminiBody
	}
	var m map[string]any
	if err := json.Unmarshal(geminiBody, &m); err != nil {
		return geminiBody
	}
	gc, _ := m["generationConfig"].(map[string]any)
	if gc == nil {
		gc = map[string]any{}
	}
	tc, _ := gc["thinkingConfig"].(map[string]any)
	if tc == nil {
		tc = map[string]any{}
	}

	switch dialect {
	case antigravityThinkingBudget:
		// The budget dialect: a level has no place here. Translate it to the
		// budget that level maps to, so a client that asked for "high" gets the
		// high budget instead of a field the backend ignores.
		if level, ok := tc["thinkingLevel"].(string); ok && level != "" {
			delete(tc, "thinkingLevel")
			tc["thinkingBudget"] = float64(antigravityBudgetForLevel(level))
		} else if _, ok := tc["thinkingBudget"]; !ok {
			// The client's own turn for this model carries its published budget
			// (1024 for the Claude family, 8192 for gpt-oss).
			tc["thinkingBudget"] = float64(antigravityDefaultBudget(wireModel))
		}
		if _, ok := tc["includeThoughts"]; !ok {
			tc["includeThoughts"] = true
		}
	default:
		// The level dialect: a budget is not a level. Map it back so a client
		// that asked in the budget dialect gets the level it meant.
		if _, ok := tc["thinkingLevel"]; !ok {
			if budget, ok := tc["thinkingBudget"]; ok {
				delete(tc, "thinkingBudget")
				tc["thinkingLevel"] = antigravityLevelForBudget(numberValue(budget))
			} else {
				tc["thinkingLevel"] = antigravityDefaultLevel(wireModel)
			}
		}
		if _, ok := tc["includeThoughts"]; !ok {
			level, _ := tc["thinkingLevel"].(string)
			tc["includeThoughts"] = level != "minimal"
		}
	}
	gc["thinkingConfig"] = tc
	m["generationConfig"] = gc
	out, err := json.Marshal(m, json.Deterministic(true))
	if err != nil {
		return geminiBody
	}
	return out
}

// antigravityDefaultLevel returns the thinking level the shipped client sends
// for a model whose family is level-shaped.
func antigravityDefaultLevel(wireModel string) string {
	entry, ok := antigravityShippedModels[wireModel]
	if !ok || entry.DefaultLevel == "" {
		return "medium"
	}
	return entry.DefaultLevel
}

// numberValue reads a JSON number out of a decoded body.
func numberValue(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	default:
		return 0
	}
}

// antigravityRequestedEffort reads the thinking level the client asked for, in
// the shapes an OpenAI-shaped request carries it: reasoning_effort, the
// Anthropic thinking object, and a bare thinking_budget.
func antigravityRequestedEffort(openaiBody []byte) string {
	var body struct {
		ReasoningEffort string `json:"reasoning_effort"`
		Thinking        *struct {
			Type         string `json:"type"`
			BudgetTokens int    `json:"budget_tokens"`
		} `json:"thinking"`
		ThinkingBudget *int `json:"thinking_budget"`
	}
	if err := json.Unmarshal(openaiBody, &body); err != nil {
		return ""
	}
	if body.ReasoningEffort != "" {
		return strings.ToLower(strings.TrimSpace(body.ReasoningEffort))
	}
	if body.Thinking != nil {
		switch body.Thinking.Type {
		case "disabled":
			return "none"
		case "enabled":
			return antigravityLevelForBudget(float64(body.Thinking.BudgetTokens))
		}
	}
	if body.ThinkingBudget != nil {
		return antigravityLevelForBudget(float64(*body.ThinkingBudget))
	}
	return ""
}

// antigravityThinkingChoice resolves the level and the budget one turn carries.
//
// The client's effort wins when it asked for one. Otherwise the model's own
// tier decides: the tiered ids name it (`gemini-3.8-flash-high` → high), and a
// tiered id names the top tier the shipped client sends. A model that names no
// tier keeps the level the client published for it.
func antigravityThinkingChoice(wireModel, effort string) (level string, budget int) {
	entry, known := antigravityShippedModels[wireModel]
	if !known {
		return "medium", antigravityClaudeDefaultBudget
	}
	effort = strings.ToLower(strings.TrimSpace(effort))
	if effort == "none" || effort == "off" {
		// Thinking off is a floor, not a switch: the Gemini families clamp it
		// onto their lowest tier (a "minimal" level is a 400 on Gemini 3.8).
		return "low", antigravityBudgetForLevel("low")
	}
	if effort != "" {
		return antigravityNormalizeLevel(effort), antigravityBudgetForLevel(effort)
	}
	level = entry.DefaultLevel
	if level == "" {
		level = "medium"
	}
	return level, antigravityBudgetForLevel(level)
}

// antigravityNormalizeLevel maps an effort onto a level the Gemini families
// advertise (minimal|low|medium|high): xhigh and max are the wire alias for
// high, and minimal is the alias for low.
func antigravityNormalizeLevel(effort string) string {
	switch effort {
	case "xhigh", "max":
		return "high"
	case "minimal":
		return "low"
	case "low", "medium", "high":
		return effort
	default:
		return "medium"
	}
}

// antigravityBudgetForLevel maps a thinking level onto the budget the shared
// level→budget table gives it (open-sse/translator/concerns/thinking.js
// LEVEL_TO_BUDGET). An unknown level falls back to the LOW budget, which is
// what the shipped client sends for a Claude turn.
func antigravityBudgetForLevel(level string) int {
	switch level {
	case "none":
		return 0
	case "minimal":
		return 512
	case "medium":
		return 8192
	case "high", "xhigh", "max":
		return 24576
	default:
		return antigravityClaudeDefaultBudget
	}
}

// antigravityLevelForBudget maps a budget back onto the level that produced
// it, using the shared thresholds (a budget at or below 1024 is low, at or
// below 8192 is medium, above that is high).
func antigravityLevelForBudget(budget float64) string {
	switch {
	case budget <= 0:
		return "none"
	case budget <= 1024:
		return "low"
	case budget <= 8192:
		return "medium"
	default:
		return "high"
	}
}
