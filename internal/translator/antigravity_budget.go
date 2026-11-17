package translator

import (
	json "encoding/json/v2"
)

// antigravityApplyOutputBudget puts the output budget on the wire.
//
// The shipped client sends one on every agent turn, and it is the model's own
// published figure (65536 for the Gemini 3.8 family, 65535 for the 2.5
// family, 64000 for the Claude family, 32768 for gpt-oss). The client asks for
// its whole budget, not the client's: the budget is a cap, so a client that
// named a smaller one keeps it and a client that named none gets the model's
// published figure.
//
// Two rules make the budget legal for the backend:
//
//   - it must not exceed the model's published figure (the reference caps the
//     same way with MAX_ANTIGRAVITY_OUTPUT_TOKENS, one global number);
//   - it must be strictly greater than the thinking budget, or Claude answers
//     400 "`max_tokens` must be greater than `thinking.budget_tokens`"
//     (measured live on 2026-11-17 with a Claude turn that carried a 1024
//     thinking budget and no output budget).
func antigravityApplyOutputBudget(geminiBody []byte, wireModel string) []byte {
	published := AntigravityMaxOutputTokens(wireModel)
	if published == 0 {
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

	thinkingBudget := float64(0)
	if tc, ok := gc["thinkingConfig"].(map[string]any); ok {
		if tb, ok := tc["thinkingBudget"].(float64); ok {
			thinkingBudget = tb
		}
	}

	// The figure the client asked for is already on the body: the thinking pass
	// wrote it from the CLIENT's own request, because only that pass can tell a
	// request from the translator's floor. This pass only enforces the two
	// rules the backend imposes on it.
	budget, hasBudget := gc["maxOutputTokens"].(float64)
	if !hasBudget || budget <= 0 {
		budget = float64(published)
	}
	if budget > float64(published) {
		budget = float64(published)
	}
	if thinkingBudget > 0 && budget <= thinkingBudget {
		budget = thinkingBudget + thinkingHeadroomTokens
		if budget > float64(published) {
			budget = float64(published)
		}
	}
	gc["maxOutputTokens"] = budget
	m["generationConfig"] = gc
	out, err := json.Marshal(m, json.Deterministic(true))
	if err != nil {
		return geminiBody
	}
	return out
}
