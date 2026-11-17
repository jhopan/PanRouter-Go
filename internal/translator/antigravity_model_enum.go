package translator

// The shipped Antigravity client's thinking dialect and output budget, per
// model family.
//
// Source: the language server the IDE ships
// (AppData/Local/Programs/antigravity/resources/bin/language_server.exe, Hub
// 2.32.1), started with the Hub launcher's argument set and asked for one turn
// per model. The capture of 2026-11-17 shows:
//
//	gemini-3.8-flash-tiered   65536  thinkingLevel HIGH
//	gemini-3.8-flash-high     65536  thinkingLevel HIGH
//	gemini-3.8-flash-medium   65536  thinkingLevel MEDIUM
//	gemini-3.8-flash-low      65536  thinkingLevel LOW
//	gemini-3-flash            65536  thinkingLevel HIGH
//	gemini-2.5-flash          65535  thinkingLevel LOW
//	gemini-2.5-flash-lite     65535  thinkingLevel LOW
//	gemini-3.5-flash-lite     65535  thinkingLevel LOW
//	claude-sonnet-4-6         64000  thinkingBudget 1024
//	claude-opus-4-6-thinking  64000  thinkingBudget 1024
//	gpt-oss-120b-medium       32768  thinkingBudget 8192
//
// Two facts the reference does not carry, because it routes Claude through a
// different translator and never speaks the Gemini dialect for this lane:
//
//   - the Claude family on this lane speaks the BUDGET dialect, not the level
//     dialect: `thinkingConfig.thinkingBudget`, not `thinkingLevel`. A
//     thinkingLevel on a Claude model is a field Google does not read for it.
//   - the output budget is per model, and a Claude turn's budget is 1024 —
//     the LOW budget — while the level dialect never appears for Claude at all.
//
// Both were verified live: a Claude turn with `thinkingBudget: 1024` and
// `maxOutputTokens: 64000` answers 200 on the same account and model that a
// `thinkingLevel` request also accepted, so the dialect is what the client
// sends, and the budget is what it publishes.

// AntigravityModelEnum is what the shipped client publishes for a catalog
// model: the label its agent turn's `labels.model_enum` carries, and the output
// budget it sends.
type AntigravityModelEnum struct {
	// Enum is the label the client's labels bag carries.
	Enum string
	// MaxOutputTokens is the output budget the client sends for this model.
	// Zero means the catalog published none.
	MaxOutputTokens int
	// SupportsThinking reports whether the model advertises thinking.
	SupportsThinking bool
}

// antigravityThinkingDialect is the thinkingConfig shape one model family
// speaks on the Antigravity lane.
type AntigravityThinkingDialect int

const (
	// antigravityThinkingLevel is the Gemini dialect: thinkingLevel plus
	// includeThoughts.
	antigravityThinkingLevel AntigravityThinkingDialect = iota
	// antigravityThinkingBudget is the budget dialect: thinkingBudget plus
	// includeThoughts. The Claude family and gpt-oss use it.
	antigravityThinkingBudget
)

// antigravityModelWire describes one model on the Antigravity lane: the enum
// the client's labels bag carries, the output budget it publishes, and the
// thinking dialect it speaks.
type antigravityModelWire struct {
	Enum            string
	MaxOutput       int
	SupportsThink   bool
	ThinkingDialect AntigravityThinkingDialect
	// DefaultLevel is the thinking level the client's own turn carries for a
	// model whose family is level-shaped. It follows the model's own tier:
	// the 3.8 family sends the tier its id names (tiered → HIGH, medium →
	// MEDIUM, low → LOW), and the 2.5 family sends LOW.
	DefaultLevel string
	// DefaultBudget is the thinking budget the client's own turn carries for a
	// model whose family is budget-shaped: 1024 for the Claude family, 8192
	// for gpt-oss.
	DefaultBudget int
}

// antigravityShippedModels is the captured table. The ids are the WIRE ids —
// the ones the backend serves — because the client's catalog and its labels bag
// both use them.
var antigravityShippedModels = map[string]antigravityModelWire{
	"gemini-3.8-flash-tiered":   {"MODEL_PLACEHOLDER_M322", 65536, true, antigravityThinkingLevel, "high", 0},
	"gemini-3.8-flash-high":     {"MODEL_PLACEHOLDER_M318", 65536, true, antigravityThinkingLevel, "high", 0},
	"gemini-3.8-flash-medium":   {"MODEL_PLACEHOLDER_M319", 65536, true, antigravityThinkingLevel, "medium", 0},
	"gemini-3.8-flash-low":      {"MODEL_PLACEHOLDER_M320", 65536, true, antigravityThinkingLevel, "low", 0},
	"gemini-3-flash":            {"MODEL_PLACEHOLDER_M18", 65536, true, antigravityThinkingLevel, "high", 0},
	"gemini-2.5-flash":          {"MODEL_GOOGLE_GEMINI_2_5_FLASH", 65535, true, antigravityThinkingLevel, "low", 0},
	"gemini-2.5-flash-lite":     {"MODEL_GOOGLE_GEMINI_2_5_FLASH_LITE", 65535, true, antigravityThinkingLevel, "low", 0},
	"gemini-2.5-flash-thinking": {"MODEL_GOOGLE_GEMINI_2_5_FLASH_THINKING", 65535, true, antigravityThinkingLevel, "low", 0},
	"gemini-2.5-pro":            {"MODEL_GOOGLE_GEMINI_2_5_PRO", 65535, true, antigravityThinkingLevel, "low", 0},
	"gemini-3.5-flash-lite":     {"MODEL_PLACEHOLDER_M198", 65535, true, antigravityThinkingLevel, "low", 0},
	"gemini-3.1-flash-lite":     {"MODEL_PLACEHOLDER_M50", 65535, true, antigravityThinkingLevel, "low", 0},
	// The Claude family speaks the budget dialect on this lane.
	"claude-sonnet-4-6":        {"MODEL_PLACEHOLDER_M35", 64000, true, antigravityThinkingBudget, "", antigravityClaudeDefaultBudget},
	"claude-opus-4-6-thinking": {"MODEL_PLACEHOLDER_M26", 64000, true, antigravityThinkingBudget, "", antigravityClaudeDefaultBudget},
	// gpt-oss speaks the budget dialect too, with a larger budget.
	"gpt-oss-120b-medium": {"MODEL_OPENAI_GPT_OSS_120B_MEDIUM", 32768, true, antigravityThinkingBudget, "", 8192},
}

// antigravityClaudeDefaultBudget is the budget the shipped client sends on a
// Claude turn: 1024, which is the LOW budget of the shared level→budget table
// (open-sse/translator/concerns/thinking.js LEVEL_TO_BUDGET.low = 1024).
const antigravityClaudeDefaultBudget = 1024

// antigravityDefaultBudget returns the thinking budget the shipped client
// publishes for a model whose family speaks the budget dialect.
func antigravityDefaultBudget(wireModel string) int {
	entry, ok := antigravityShippedModels[wireModel]
	if !ok {
		return antigravityClaudeDefaultBudget
	}
	if entry.DefaultBudget > 0 {
		return entry.DefaultBudget
	}
	return antigravityClaudeDefaultBudget
}

// AntigravityModelEnumFor returns the wire facts the shipped client publishes
// for a model id.
func AntigravityModelEnumFor(wireModel string) (AntigravityModelEnum, bool) {
	entry, ok := antigravityShippedModels[wireModel]
	if !ok {
		return AntigravityModelEnum{}, false
	}
	return AntigravityModelEnum{
		Enum:             entry.Enum,
		MaxOutputTokens:  entry.MaxOutput,
		SupportsThinking: entry.SupportsThink,
	}, true
}

// AntigravityModelEnumName returns the labels value the client's bag carries
// for a model, or "" for a model it does not publish.
func AntigravityModelEnumName(wireModel string) string {
	entry, ok := antigravityShippedModels[wireModel]
	if !ok {
		return ""
	}
	return entry.Enum
}

// AntigravityMaxOutputTokens returns the output budget the shipped client
// publishes for a model, or 0 when it publishes none.
//
// This is the CAP, not a floor: a client that asked for fewer tokens keeps its
// own value, and one that asked for more is capped here — the same rule the
// reference applies with MAX_ANTIGRAVITY_OUTPUT_TOKENS, except that the shipped
// client's cap is per model rather than one global figure.
func AntigravityMaxOutputTokens(wireModel string) int {
	entry, ok := antigravityShippedModels[wireModel]
	if !ok {
		return 0
	}
	return entry.MaxOutput
}

// AntigravityThinkingBudgetFor returns the thinking budget the shipped client
// sends for a model that speaks the budget dialect, and ok=false for a model
// that speaks the level dialect or is unknown.
//
// The Claude family gets the LOW budget (1024) by default; the caller's own
// effort raises it, and the shipped client's own turn for gpt-oss carried 8192 —
// the MEDIUM budget — which is what an effort of "medium" maps to.
func AntigravityThinkingBudgetFor(wireModel string) (int, bool) {
	entry, ok := antigravityShippedModels[wireModel]
	if !ok || entry.ThinkingDialect != antigravityThinkingBudget {
		return 0, false
	}
	return antigravityClaudeDefaultBudget, true
}

// AntigravityThinkingDialectFor returns the dialect a model speaks on this
// lane.
func AntigravityThinkingDialectFor(wireModel string) (AntigravityThinkingDialect, bool) {
	entry, ok := antigravityShippedModels[wireModel]
	if !ok {
		return antigravityThinkingLevel, false
	}
	return entry.ThinkingDialect, true
}
