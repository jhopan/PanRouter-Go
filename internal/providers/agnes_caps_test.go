package providers

import (
	"strings"
	"testing"
)

// Port of upstream tests/unit/agnes30-pro-model.test.js (decolua/9router#4587),
// minus the registry assertions that live in registry_models.go and the pricing
// half that lives in internal/pricing.
//
// The regression this pins: capabilities.js carried no Agnes rows at all, so
// every seeded id fell through to DEFAULT_CAPABILITIES — vision:false — and the
// translator replaced every image block with "[image omitted: model has no
// vision support]" before dispatch, with no error anywhere. The vendor's
// agnes-30-pro / agnes-30-flash doc pages state both models take "Text and
// image URL" input at a 512K window and 65,536 max output.
func TestGetCapabilitiesForModel_Agnes30(t *testing.T) {
	tests := []struct {
		model         string
		wantVision    bool
		wantReasoning bool
	}{
		{model: "agnes-3.0-pro", wantVision: true, wantReasoning: true},
		{model: "agnes-3.0-flash", wantVision: true, wantReasoning: true},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			caps := capabilitiesOf(t, "agnes", tt.model)
			if caps.Vision != tt.wantVision {
				t.Errorf("Vision = %v, want %v", caps.Vision, tt.wantVision)
			}
			if caps.Reasoning != tt.wantReasoning {
				t.Errorf("Reasoning = %v, want %v", caps.Reasoning, tt.wantReasoning)
			}
			if !caps.Tools {
				t.Error("Tools = false, want true (the default)")
			}
			if caps.ThinkingFormat != "openai" {
				t.Errorf("ThinkingFormat = %q, want %q", caps.ThinkingFormat, "openai")
			}
			if caps.ContextWindow != 512000 || caps.MaxOutput != 65536 {
				t.Errorf("limits = %d/%d, want 512000/65536", caps.ContextWindow, caps.MaxOutput)
			}
		})
	}
}

// The capabilities table is keyed on the bare model id, so routing through the
// agnes-ai alias must resolve to the same block.
func TestGetCapabilitiesForModel_AgnesAliasResolvesSame(t *testing.T) {
	if GetCapabilitiesForModel("agnes-ai", "agnes-3.0-pro") != capabilitiesOf(t, "agnes", "agnes-3.0-pro") {
		t.Error("agnes-ai alias resolves different capabilities than agnes")
	}
}

// The 2.5 line is left on the default rather than inheriting 3.0's numbers: no
// vendor figures are published for it, and a `agnes*` glob would silently apply
// those numbers to every id the provider ever ships.
func TestGetCapabilitiesForModel_Agnes25KeepsDefaults(t *testing.T) {
	for _, model := range []string{"agnes-2.5-flash", "agnes-2.5-pro", "agnes-2.5-pro-beta"} {
		t.Run(model, func(t *testing.T) {
			caps := capabilitiesOf(t, "agnes", model)
			if caps.Vision {
				t.Error("Vision = true, want false (no documented limits for the 2.5 line)")
			}
			if caps.ContextWindow != 0 || caps.MaxOutput != 0 {
				t.Errorf("limits = %d/%d, want 0/0 so the default chain resolves them", caps.ContextWindow, caps.MaxOutput)
			}
		})
	}
}

// agnes-3.0-pro is seeded under the DOTTED id the docs specify in every code
// sample ("Use `agnes-3.0-pro` as the model name"); the page slug uses dashes,
// which is only a URL convention.
func TestAgnesCatalogSeeds(t *testing.T) {
	models := GetProviderModels("agnes")
	for _, want := range []string{
		"agnes-2.5-flash", "agnes-2.5-pro", "agnes-2.5-pro-beta",
		"agnes-3.0-flash", "agnes-3.0-pro",
	} {
		if !slicesContains(models, want) {
			t.Errorf("agnes catalog missing %q", want)
		}
	}
	if slicesContains(models, "agnes-30-pro") {
		t.Error("agnes catalog seeds the dashed page slug agnes-30-pro")
	}
}

// TestAgnesRegistryParity pins the registry fields the reference declares
// (open-sse/providers/registry/agnes.js): an apikey lane at priority 61 with the
// dynamic catalogue passed through, both spellings of the alias, and the free-tier
// notice the operator sees before adding a key.
func TestAgnesRegistryParity(t *testing.T) {
	meta, ok := ProviderRegistry["agnes"]
	if !ok {
		t.Fatal("agnes is missing from the registry")
	}
	if meta.Category != CategoryAPIKey {
		t.Errorf("category = %v, want apikey", meta.Category)
	}
	if meta.Priority != 61 {
		t.Errorf("priority = %d, want 61", meta.Priority)
	}
	if !meta.PassthroughModels {
		t.Error("the catalogue is dynamic upstream; passthroughModels must be on")
	}
	if meta.Color != "#F97316" {
		t.Errorf("color = %q, want #F97316", meta.Color)
	}
	if meta.Notice == nil || meta.Notice.APIKeyURL != "https://platform.agnes-ai.com/" {
		t.Errorf("notice = %+v, want the platform key link", meta.Notice)
	}
	if meta.Notice.Text == "" || !strings.Contains(meta.Notice.Text, "no credit card") {
		t.Errorf("notice text = %q, want the free-tier wording", meta.Notice.Text)
	}

	for _, alias := range []string{"agnes-ai", "agnesai"} {
		if got := ResolveAlias(alias); got != "agnes" {
			t.Errorf("ResolveAlias(%q) = %q, want agnes", alias, got)
		}
	}
	if cfg, ok := KnownProviders["agnes"]; !ok || cfg.BaseURL != "https://apihub.agnes-ai.com/v1/chat/completions" {
		t.Errorf("base URL = %q, want the Agnes hub", cfg.BaseURL)
	}
}
