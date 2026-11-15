package providers

import "testing"

// TestAntigravityClaudeMaxOutputFloor pins the lane-scoped floor: upstream
// resolves a Claude id on Antigravity through PATTERN_CAPABILITIES rows that
// declare flags and a thinking shape only, so its figures come from
// DEFAULT_CAPABILITIES (200000 context / 64000 output,
// open-sse/providers/capabilities.js:63). This port's shared `*claude*`
// substring row answers 8192 for the same ids, which under-states the lane by
// seven times.
//
// Fails before the change: claude-opus-4-6-thinking published 8192.
func TestAntigravityClaudeMaxOutputFloor(t *testing.T) {
	InvalidateCapabilitiesCache()
	t.Cleanup(InvalidateCapabilitiesCache)

	tests := []struct {
		name     string
		provider string
		model    string
		wantOut  int
	}{
		{name: "the lane's claude row", provider: "antigravity", model: "claude-opus-4-6-thinking", wantOut: 64000},
		{name: "the alias carries the floor too", provider: "ag", model: "claude-opus-4-6-thinking", wantOut: 64000},
		// The 5.5 rows already resolve above the floor (the `*claude*sonnet-5*`
		// pattern's 128000), so the floor leaves them alone.
		{name: "rows above the floor keep their figure", provider: "antigravity", model: "claude-sonnet-5-5", wantOut: 128000},
		// A declared row outranks the floor: claude-sonnet-4-6 is 128000
		// upstream (MODEL_CAPABILITIES' exact row).
		{name: "a declared row wins", provider: "antigravity", model: "claude-sonnet-4-6", wantOut: 128000},
		// The floor is the Antigravity lane's, not every claude id's.
		{name: "other lanes keep the gated claude row", provider: "opencode-zen", model: "claude-opus-4-6", wantOut: 8192},
		// Gemini rows are untouched.
		{name: "gemini rows unchanged", provider: "antigravity", model: "gemini-3.8-flash-high", wantOut: 65536},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			detail := GetCapabilitiesDetailForModel(tt.provider, tt.model)
			if detail.MaxOutput != tt.wantOut {
				t.Errorf("maxOutput = %d, want %d", detail.MaxOutput, tt.wantOut)
			}
		})
	}
}

// TestAntigravityClaudeMaxOutputFloorRespectsCustomRows keeps the operator's
// own declaration above the floor: a custom row that names a maxOutput is a
// decision, not the substring table's guess. The id is deliberately one the
// Antigravity catalog does not own — SetCustomModelCaps refuses those — but
// upstream's `*claude-3*` pattern still gives it the lane's default 64000, so
// the floor and the custom row meet on the same model.
func TestAntigravityClaudeMaxOutputFloorRespectsCustomRows(t *testing.T) {
	const model = "claude-3-5-sonnet-floor-probe"
	InvalidateCapabilitiesCache()
	t.Cleanup(func() {
		SetCustomModelCaps("antigravity", model, Capabilities{})
		InvalidateCapabilitiesCache()
	})

	if got := GetCapabilitiesDetailForModel("antigravity", model).MaxOutput; got != antigravityClaudeMaxOutputFloor {
		t.Fatalf("maxOutput without a custom row = %d, want the %d floor", got, antigravityClaudeMaxOutputFloor)
	}

	SetCustomModelCaps("antigravity", model, Capabilities{MaxOutput: 4096})
	if got := GetCapabilitiesDetailForModel("antigravity", model).MaxOutput; got != 4096 {
		t.Errorf("maxOutput = %d, want the operator's 4096", got)
	}
}
