package translator_test

import (
	"slices"
	"strings"
	"testing"

	"panrouter/proxy/internal/providers"
	"panrouter/proxy/internal/translator"
)

// antigravityRegistryRow is one row of upstream's
// open-sse/providers/registry/antigravity.js `models` list, together with the
// limits upstream's capability tables resolve for it: MODEL_CAPABILITIES'
// exact entries first, then PATTERN_CAPABILITIES (specific before generic),
// both merged over DEFAULT_CAPABILITIES (200000 context / 64000 output).
//
// window/output carry what OUR catalogue must publish for the row. They are
// upstream's resolved figures except where `deviation` says otherwise, and every
// deviation is a deliberate one recorded in .changes/908.
type antigravityRegistryRow struct {
	id            string
	upstreamModel string
	kind          string // "" for chat, "image" for the image surface
	window        int
	output        int
	retired       bool // upstream still lists it; Cloud Code answers 400/404
	deviation     string
	wantWireModel string
	wireModelNote string
}

// antigravityRegistryRows is the upstream list, verbatim, in its own order.
var antigravityRegistryRows = []antigravityRegistryRow{
	{id: "gemini-4-argon-high", upstreamModel: "gemini-4-argon-high", window: 1048576, output: 65536},
	{id: "gemini-4-argon-medium", upstreamModel: "gemini-4-argon-medium", window: 1048576, output: 65536},
	{id: "gemini-4-argon-low", upstreamModel: "gemini-4-argon-low", window: 1048576, output: 65536},
	{id: "gemini-3.8-flash-high", upstreamModel: "gemini-3.8-flash-high", window: 1048576, output: 65536},
	{id: "gemini-3.8-flash-medium", upstreamModel: "gemini-3.8-flash-medium", window: 1048576, output: 65536},
	{id: "gemini-3.8-flash-low", upstreamModel: "gemini-3.8-flash-low", window: 1048576, output: 65536},
	{id: "gemini-3.8-flash", upstreamModel: "gemini-3.8-flash-medium", window: 1048576, output: 65536},
	{id: "gemini-3.7-flash-high", upstreamModel: "gemini-3.7-flash-tiered", window: 1048576, output: 65536},
	{id: "gemini-3.7-flash-medium", upstreamModel: "gemini-3.7-flash-tiered", window: 1048576, output: 65536},
	{id: "gemini-3.7-flash-low", upstreamModel: "gemini-3.7-flash-tiered", window: 1048576, output: 65536},
	{id: "gemini-3.6-flash-high", upstreamModel: "gemini-3.6-flash-tiered", window: 1048576, output: 65536},
	{id: "gemini-3.6-flash-medium", upstreamModel: "gemini-3.6-flash-tiered", window: 1048576, output: 65536},
	{id: "gemini-3.6-flash-low", upstreamModel: "gemini-3.6-flash-tiered", window: 1048576, output: 65536},
	// The 3.5 family is retired upstream-side (400/404 on Cloud Code), which is
	// why AntigravityRetiredModelIDs lists it.
	{id: "gemini-3.5-flash-high", retired: true},
	{id: "gemini-3-flash-agent", retired: true},
	{id: "gemini-3.5-flash-low", retired: true},
	{id: "gemini-3.5-flash-extra-low", retired: true},
	{id: "gemini-pro-agent", window: 1048576, output: 64000, wantWireModel: "gemini-pro-agent"},
	{
		id: "gemini-3.1-pro-low", window: 1048576, output: 65536, wantWireModel: "gemini-3.1-pro-low",
		deviation: "upstream's *gemini-3*pro* pattern declares maxOutput 65535 (one token below the 2^16 the whole Gemini 3 family uses); this port publishes 65536 for every Gemini 3/4 id",
	},
	{id: "claude-sonnet-4-6", window: 1000000, output: 128000, wantWireModel: "claude-sonnet-4-6"},
	// DEFAULT_CAPABILITIES' maxOutput (capabilities.js:63) is what upstream
	// resolves for this id: its `*claude*opus*` pattern declares flags and a
	// thinking shape only. The lane now carries that as a floor
	// (antigravityClaudeMaxOutputFloor), so the port publishes upstream's 64000
	// instead of the 8192 the shared claude substring row guessed.
	{id: "claude-opus-4-6-thinking", window: 200000, output: 64000, wantWireModel: "claude-opus-4-6-thinking"},
	{id: "gpt-oss-120b-medium", window: 128000, output: 64000, wantWireModel: "gpt-oss-120b-medium"},
	{
		id: "gemini-3-flash", window: 1048576, output: 65536, wantWireModel: "gemini-3.8-flash-high",
		wireModelNote: "the id is served as an alias of the 3.8 high tier (this port's AntigravityModelSynonyms), because upstream decolua/9router#24034f69 retired the bare gemini-3-flash id Cloud Code answers 400/404 for",
	},
	{
		id: "gemini-3.1-flash-image", kind: "image", window: 1048576, output: 65536, wantWireModel: "gemini-3.1-flash-image",
		deviation: "upstream's *gemini*image* row leaves maxOutput at DEFAULT_CAPABILITIES' 64000; this port publishes the Gemini family's 65536",
	},
}

// TestAntigravityCatalogMatchesRegistry pins the Antigravity catalogue against
// upstream's registry for the lane: every row upstream declares is either
// advertised with the same context window, or deliberately absent because Cloud
// Code no longer serves it. The window is what a client auto-compacts against,
// so a row that drifts here silently changes how much history the client sends.
func TestAntigravityCatalogMatchesRegistry(t *testing.T) {
	providers.InvalidateCapabilitiesCache()
	t.Cleanup(providers.InvalidateCapabilitiesCache)

	// Both spellings of the provider must carry the catalogue: GetProviderModels
	// resolves the alias, and the dashboard reads the id.
	for _, provider := range []string{"antigravity", "ag"} {
		catalog := providers.GetProviderModels(provider)
		if len(catalog) == 0 {
			t.Fatalf("no catalogue for %q", provider)
		}
		for _, row := range antigravityRegistryRows {
			t.Run(provider+"/"+row.id, func(t *testing.T) {
				listed := slices.Contains(catalog, row.id)
				if row.retired {
					if listed {
						t.Errorf("%s is retired upstream-side but the catalogue still advertises it", row.id)
					}
					return
				}
				if !listed {
					t.Fatalf("%s is missing from the catalogue", row.id)
				}
				if row.kind != "" && providers.ProviderModelKinds[provider][row.id] != row.kind {
					t.Errorf("kind = %q, want %q", providers.ProviderModelKinds[provider][row.id], row.kind)
				}
				detail := providers.GetCapabilitiesDetailForModel(provider, row.id)
				if detail.ContextWindow != row.window {
					t.Errorf("contextWindow = %d, want %d", detail.ContextWindow, row.window)
				}
				if detail.MaxOutput != row.output {
					t.Errorf("maxOutput = %d, want %d", detail.MaxOutput, row.output)
				}
			})
		}
	}
}

// TestAntigravityRegistryWireModel pins the id that reaches Cloud Code. The
// registry's parenthesised effort ("gemini-3.8-flash-low(low)") is panrouter's
// own thinking notation, not a Google model id: Cloud Code answers 404 for a
// suffixed id, so the wire value is the bare id the registry names.
func TestAntigravityRegistryWireModel(t *testing.T) {
	for _, row := range antigravityRegistryRows {
		if row.retired || row.wantWireModel == "" {
			continue
		}
		t.Run(row.id, func(t *testing.T) {
			got := translator.AntigravityUpstreamModel(row.id)
			if got != translator.AntigravityUpstreamModel(row.wantWireModel) {
				t.Errorf("AntigravityUpstreamModel(%q) = %q, want %q", row.id, got, row.wantWireModel)
			}
			if row.wireModelNote == "" && got != row.id && row.upstreamModel == "" {
				t.Errorf("AntigravityUpstreamModel(%q) = %q, want the id unchanged", row.id, got)
			}
			if strings.Contains(got, "(") {
				t.Errorf("wire model %q carries the thinking notation upstream strips before the wire", got)
			}
		})
	}
}
