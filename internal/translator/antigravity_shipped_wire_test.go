package translator_test

// The Antigravity lane's wire contract, as the SHIPPED client sends it.
//
// The authority is the language server the IDE installs
// (AppData/Local/Programs/antigravity/resources/bin/language_server.exe, Hub
// 2.32.1), started with the Hub launcher's argument set and MITM-captured
// through mitmdump while it answered one turn per model:
//
//	gemini-3.8-flash-tiered   65536  thinkingLevel HIGH   MODEL_PLACEHOLDER_M322
//	gemini-3.8-flash-high     65536  thinkingLevel HIGH   MODEL_PLACEHOLDER_M318
//	gemini-3.8-flash-medium   65536  thinkingLevel MEDIUM MODEL_PLACEHOLDER_M319
//	gemini-3.8-flash-low      65536  thinkingLevel LOW    MODEL_PLACEHOLDER_M320
//	gemini-3-flash            65536  thinkingLevel HIGH   MODEL_PLACEHOLDER_M18
//	gemini-2.5-flash          65535  thinkingLevel LOW    MODEL_GOOGLE_GEMINI_2_5_FLASH
//	gemini-2.5-flash-lite     65535  thinkingLevel LOW    MODEL_GOOGLE_GEMINI_2_5_FLASH_LITE
//	gemini-3.5-flash-lite     65535  thinkingLevel LOW    MODEL_PLACEHOLDER_M198
//	claude-sonnet-4-6         64000  thinkingBudget 1024  MODEL_PLACEHOLDER_M35
//	claude-opus-4-6-thinking  64000  thinkingBudget 1024  MODEL_PLACEHOLDER_M26
//	gpt-oss-120b-medium       32768  thinkingBudget 8192  MODEL_OPENAI_GPT_OSS_120B_MEDIUM
//
// Three facts the reference does not carry, and this port now does:
//
//   - labels.model_enum: the client sends it on every agent turn. Upstream
//     omits it because it had no mapping ("we have no mapping for it, so it is
//     omitted rather than guessed"); the mapping is not a guess — the client
//     publishes it in the catalog it fetches itself.
//   - the output budget is PER MODEL, not one global cap.
//   - the Claude family speaks the BUDGET thinking dialect on this lane
//     (thinkingBudget), not the level dialect the Gemini families use.

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"strings"
	"testing"

	"panrouter/proxy/internal/translator"
)

// wrapAntigravity builds the envelope the gateway sends and returns its inner
// request as a decoded map.
func wrapAntigravity(t *testing.T, model, inner string) map[string]any {
	t.Helper()
	wrapped, err := translator.WrapForAntigravity([]byte(inner), "aicode-consumers", model, "sess-probe")
	if err != nil {
		t.Fatalf("WrapForAntigravity(%q): %v", model, err)
	}
	var envelope struct {
		Request map[string]any `json:"request"`
	}
	if err := json.Unmarshal(wrapped, &envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	return envelope.Request
}

func TestAntigravityLabelsCarryTheShippedModelEnum(t *testing.T) {
	tests := []struct {
		model string
		want  string
	}{
		{"gemini-3.8-flash-medium", "MODEL_PLACEHOLDER_M319"},
		{"gemini-3.8-flash-high", "MODEL_PLACEHOLDER_M318"},
		{"gemini-3.8-flash-low", "MODEL_PLACEHOLDER_M320"},
		{"gemini-3.8-flash-tiered", "MODEL_PLACEHOLDER_M322"},
		{"claude-sonnet-4-6", "MODEL_PLACEHOLDER_M35"},
		{"claude-opus-4-6-thinking", "MODEL_PLACEHOLDER_M26"},
		{"gpt-oss-120b-medium", "MODEL_OPENAI_GPT_OSS_120B_MEDIUM"},
		{"gemini-2.5-flash", "MODEL_GOOGLE_GEMINI_2_5_FLASH"},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			inner := wrapAntigravity(t, tt.model, `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
			labels, _ := inner["labels"].(map[string]any)
			if labels == nil {
				t.Fatalf("no labels bag in %v", inner)
			}
			if got, _ := labels["model_enum"].(string); got != tt.want {
				t.Errorf("labels.model_enum = %q, want %q", got, tt.want)
			}
		})
	}
}

// A model the shipped client does not publish gets no enum: the bag stays the
// client's own shape rather than carrying a value nobody sent.
func TestAntigravityLabelsOmitTheEnumForAnUnpublishedModel(t *testing.T) {
	inner := wrapAntigravity(t, "gemini-9-9-imaginary", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	labels, _ := inner["labels"].(map[string]any)
	if labels == nil {
		t.Fatalf("no labels bag in %v", inner)
	}
	if got, ok := labels["model_enum"]; ok {
		t.Errorf("labels.model_enum = %v, want it absent for an unpublished model", got)
	}
}

// The output budget is the model's own published figure. The reference caps
// every model at one global number (MAX_ANTIGRAVITY_OUTPUT_TOKENS); the
// shipped client publishes a budget per model and sends that.
func TestAntigravityOutputBudgetIsPerModel(t *testing.T) {
	tests := []struct {
		model string
		want  float64
	}{
		{"gemini-3.8-flash-medium", 65536},
		{"gemini-2.5-flash", 65535},
		{"claude-sonnet-4-6", 64000},
		{"gpt-oss-120b-medium", 32768},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			inner := wrapAntigravity(t, tt.model,
				`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":200000}}`)
			gc, _ := inner["generationConfig"].(map[string]any)
			if gc == nil {
				t.Fatalf("no generationConfig in %v", inner)
			}
			if got, _ := gc["maxOutputTokens"].(float64); got != tt.want {
				t.Errorf("maxOutputTokens = %v, want %v", got, tt.want)
			}
		})
	}
}

// A budget the client asked for stays: the published figure is a CAP, not a
// floor. The shipped client sends its own figure, and a client that asked for
// less keeps its value.
func TestAntigravityKeepsASmallerClientBudget(t *testing.T) {
	inner := wrapAntigravity(t, "gemini-3.8-flash-medium",
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":4096}}`)
	gc, _ := inner["generationConfig"].(map[string]any)
	if got, _ := gc["maxOutputTokens"].(float64); got != 4096 {
		t.Errorf("maxOutputTokens = %v, want the client's own 4096", got)
	}
}

// The thinking dialect is per family: the Gemini families speak thinkingLevel,
// the Claude family and gpt-oss speak thinkingBudget.
//
// The body here carries no thinking of its own, so each model gets the
// default the shipped client publishes for it: the tier its id names for the
// Gemini families, and the published budget for the budget families.
func TestAntigravityThinkingDialectPerFamily(t *testing.T) {
	tests := []struct {
		model      string
		wantField  string
		wantValue  float64
		wantAbsent string
	}{
		{
			model:      "gemini-3.8-flash-medium",
			wantField:  "thinkingLevel",
			wantValue:  0, // a string, checked below
			wantAbsent: "thinkingBudget",
		},
		{
			model:      "claude-sonnet-4-6",
			wantField:  "thinkingBudget",
			wantValue:  1024,
			wantAbsent: "thinkingLevel",
		},
		{
			model:      "gpt-oss-120b-medium",
			wantField:  "thinkingBudget",
			wantValue:  8192,
			wantAbsent: "thinkingLevel",
		},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			inner := wrapAntigravity(t, tt.model,
				`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
			gc, _ := inner["generationConfig"].(map[string]any)
			tc, _ := gc["thinkingConfig"].(map[string]any)
			if tc == nil {
				t.Fatalf("no thinkingConfig in %v", inner)
			}
			if _, ok := tc[tt.wantAbsent]; ok {
				t.Errorf("thinkingConfig carries %q, want it absent on this family: %v", tt.wantAbsent, tc)
			}
			switch tt.wantField {
			case "thinkingBudget":
				if got, _ := tc["thinkingBudget"].(float64); got != tt.wantValue {
					t.Errorf("thinkingBudget = %v, want %v", got, tt.wantValue)
				}
			case "thinkingLevel":
				if got, _ := tc["thinkingLevel"].(string); got != "medium" {
					t.Errorf("thinkingLevel = %q, want the tier the model names (medium)", got)
				}
			}
		})
	}
}

// A budget-dialect model turns the caller's level into the budget that level
// maps to: the shared level→budget table (low 1024, medium 8192, high 24576).
func TestAntigravityClaudeBudgetFollowsTheRequestedLevel(t *testing.T) {
	tests := []struct {
		level string
		want  float64
	}{
		{"low", 1024},
		{"medium", 8192},
		{"high", 24576},
	}
	for _, tt := range tests {
		t.Run(tt.level, func(t *testing.T) {
			inner := wrapAntigravity(t, "claude-sonnet-4-6",
				`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"thinkingLevel":"`+tt.level+`","includeThoughts":true}}}`)
			gc, _ := inner["generationConfig"].(map[string]any)
			tc, _ := gc["thinkingConfig"].(map[string]any)
			if tc == nil {
				t.Fatalf("no thinkingConfig in %v", inner)
			}
			if got, _ := tc["thinkingBudget"].(float64); got != tt.want {
				t.Errorf("thinkingBudget = %v, want %v", got, tt.want)
			}
		})
	}
}

// The budget a budget-dialect model gets when the CALLER asked for no
// thinking at all is the one the shipped client publishes for it: 1024 for the
// Claude family, 8192 for gpt-oss.
//
// The wrap step alone is not enough to pin this: the production path runs the
// thinking pass FIRST (AntigravityThinkingFor), and that pass is where a
// budget-dialect model used to be folded onto the level table's default tier —
// medium, so 8192 — instead of the published budget. The unit test that
// wrapped a bare body could not see it, because the bare body carries no
// thinking for the pass to rewrite.
//
// This test runs the two steps in the order the proxy runs them.
func TestAntigravityBudgetFamilyDefaultBudgetOnTheProductionPath(t *testing.T) {
	tests := []struct {
		model string
		want  float64
	}{
		{"claude-sonnet-4-6", 1024},
		{"gpt-oss-120b-medium", 8192},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			// The proxy's order: translate, then the thinking pass, then wrap.
			geminiBody := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
			wireModel := translator.AntigravityUpstreamModel(tt.model)
			geminiBody = translator.AntigravityThinkingFor(geminiBody, []byte(`{"model":"`+tt.model+`","messages":[{"role":"user","content":"hi"}]}`), wireModel)
			wrapped, err := translator.WrapForAntigravity(geminiBody, "aicode-consumers", tt.model, "sess-probe")
			if err != nil {
				t.Fatalf("WrapForAntigravity(%q): %v", tt.model, err)
			}
			var envelope struct {
				Request map[string]any `json:"request"`
			}
			if err := json.Unmarshal(wrapped, &envelope); err != nil {
				t.Fatalf("decode envelope: %v", err)
			}
			gc, _ := envelope.Request["generationConfig"].(map[string]any)
			tc, _ := gc["thinkingConfig"].(map[string]any)
			if tc == nil {
				t.Fatalf("no thinkingConfig in %v", envelope.Request)
			}
			if _, ok := tc["thinkingLevel"]; ok {
				t.Errorf("thinkingConfig carries a level on a budget family: %v", tc)
			}
			if got, _ := tc["thinkingBudget"].(float64); got != tt.want {
				t.Errorf("thinkingBudget = %v, want %v (the budget the shipped client publishes)", got, tt.want)
			}
		})
	}
}

// The output budget on the wire is the model's published figure when the
// caller named none — 64000 for the Claude family, 32768 for gpt-oss — and
// the client's own smaller figure when it named one.
//
// The published figure is the one the shipped client sends, and the
// translator's own floor is NOT a request: the Gemini translation writes a
// floor into maxOutputTokens, and a floor mistaken for the client's number
// pinned a Claude turn to 8192 (and, once the thinking budget moved earlier,
// to 9216) instead of 64000.
func TestAntigravityOutputBudgetOnTheProductionPath(t *testing.T) {
	tests := []struct {
		name       string
		model      string
		clientBody string
		wantBudget float64
	}{
		{
			name:       "no client budget keeps the published figure",
			model:      "claude-sonnet-4-6",
			clientBody: `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`,
			wantBudget: 64000,
		},
		{
			name:       "no client budget keeps the published figure for gpt-oss",
			model:      "gpt-oss-120b-medium",
			clientBody: `{"model":"gpt-oss-120b-medium","messages":[{"role":"user","content":"hi"}]}`,
			wantBudget: 32768,
		},
		{
			name:       "a smaller client budget is kept",
			model:      "claude-sonnet-4-6",
			clientBody: `{"model":"claude-sonnet-4-6","max_tokens":4096,"messages":[{"role":"user","content":"hi"}]}`,
			wantBudget: 4096,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			geminiBody := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
			wireModel := translator.AntigravityUpstreamModel(tt.model)
			geminiBody = translator.AntigravityThinkingFor(geminiBody, []byte(tt.clientBody), wireModel)
			wrapped, err := translator.WrapForAntigravity(geminiBody, "aicode-consumers", tt.model, "sess-probe")
			if err != nil {
				t.Fatalf("WrapForAntigravity(%q): %v", tt.model, err)
			}
			var envelope struct {
				Request map[string]any `json:"request"`
			}
			if err := json.Unmarshal(wrapped, &envelope); err != nil {
				t.Fatalf("decode envelope: %v", err)
			}
			gc, _ := envelope.Request["generationConfig"].(map[string]any)
			if got, _ := gc["maxOutputTokens"].(float64); got != tt.wantBudget {
				t.Errorf("maxOutputTokens = %v, want %v", got, tt.wantBudget)
			}
		})
	}
}

// The identity bag is the client's own shape: the request id's ids, the step
// index, the used_* flags and the enum, all strings.
func TestAntigravityLabelsAreTheClientsShape(t *testing.T) {
	inner := wrapAntigravity(t, "gemini-3.8-flash-medium", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	labels, _ := inner["labels"].(map[string]any)
	for _, key := range []string{
		"cascade_id", "root_cascade_id", "trajectory_id", "request_id",
		"last_step_index", "model_enum", "used_claude", "used_claude_conservative",
		"used_non_gemini_model",
	} {
		v, ok := labels[key]
		if !ok {
			t.Errorf("labels is missing %q: %v", key, labels)
			continue
		}
		if _, ok := v.(string); !ok {
			t.Errorf("labels[%q] = %T, want a string", key, v)
		}
	}
}

// The session id is a signed number on the wire, as the client sends it.
func TestAntigravitySessionIDIsSignedNumeric(t *testing.T) {
	inner := wrapAntigravity(t, "gemini-3.8-flash-medium", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	sess, _ := inner["sessionId"].(string)
	if sess == "" {
		t.Fatalf("no sessionId in %v", inner)
	}
	if !strings.HasPrefix(sess, "-") {
		t.Errorf("sessionId = %q, want the client's signed shape", sess)
	}
	for _, r := range strings.TrimPrefix(sess, "-") {
		if r < '0' || r > '9' {
			t.Errorf("sessionId = %q, want digits", sess)
			break
		}
	}
}

// The envelope keeps the fields the client sends on the wire.
func TestAntigravityEnvelopeShape(t *testing.T) {
	wrapped, err := translator.WrapForAntigravity(
		[]byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`),
		"aicode-consumers", "gemini-3.8-flash-medium", "sess-probe")
	if err != nil {
		t.Fatalf("WrapForAntigravity: %v", err)
	}
	var envelope struct {
		Project     string         `json:"project"`
		Model       string         `json:"model"`
		UserAgent   string         `json:"userAgent"`
		RequestType string         `json:"requestType"`
		RequestID   string         `json:"requestId"`
		Request     jsontext.Value `json:"request"`
	}
	if err := json.Unmarshal(wrapped, &envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if envelope.Project != "aicode-consumers" {
		t.Errorf("project = %q", envelope.Project)
	}
	if envelope.UserAgent != "antigravity" {
		t.Errorf("userAgent = %q, want antigravity", envelope.UserAgent)
	}
	if envelope.RequestType != "agent" {
		t.Errorf("requestType = %q, want agent", envelope.RequestType)
	}
	if !strings.HasPrefix(envelope.RequestID, "agent/") {
		t.Errorf("requestId = %q, want the agent/<conv>/<ms>/<traj>/<step> shape", envelope.RequestID)
	}
}
