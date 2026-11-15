package providers

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// devinCLIRegistryModel is one entry of open-sse/providers/registry/devin-cli.js.
type devinCLIRegistryModel struct {
	id         string
	name       string
	contextLen int // the registry's contextLength; 0 when the entry declares none
}

// devinCLIRegistryModels is the upstream registry's model list, transcribed
// verbatim. A lane that routes a model its catalogue does not list cannot be
// reached by name at all, and the dashboard that configures the provider reads
// its own block, so both are pinned against this list.
var devinCLIRegistryModels = []devinCLIRegistryModel{
	{id: "swe-1.6-fast", name: "SWE-1.6 Fast"},
	{id: "swe-1.6", name: "SWE-1.6"},
	{id: "swe-1.5-fast", name: "SWE-1.5 Fast"},
	{id: "swe-1.5", name: "SWE-1.5"},
	{id: "claude-opus-4.7-max", name: "Claude Opus 4.7 Max", contextLen: 200000},
	{id: "claude-opus-4.7-high", name: "Claude Opus 4.7 High", contextLen: 200000},
	{id: "claude-opus-4.7-medium", name: "Claude Opus 4.7 Medium", contextLen: 200000},
	{id: "claude-opus-4.7-low", name: "Claude Opus 4.7 Low", contextLen: 200000},
	{id: "claude-sonnet-4.6-thinking-1m", name: "Claude Sonnet 4.6 Thinking 1M", contextLen: 1000000},
	{id: "claude-sonnet-4.6-thinking", name: "Claude Sonnet 4.6 Thinking", contextLen: 200000},
	{id: "claude-sonnet-4.6", name: "Claude Sonnet 4.6", contextLen: 200000},
	{id: "claude-opus-4.6-thinking", name: "Claude Opus 4.6 Thinking", contextLen: 200000},
	{id: "claude-opus-4.6", name: "Claude Opus 4.6", contextLen: 200000},
	{id: "claude-sonnet-4.5", name: "Claude Sonnet 4.5", contextLen: 200000},
	{id: "claude-haiku-4.5", name: "Claude Haiku 4.5", contextLen: 200000},
	{id: "gpt-5.5-xhigh", name: "GPT-5.5 XHigh", contextLen: 200000},
	{id: "gpt-5.5-high", name: "GPT-5.5 High", contextLen: 200000},
	{id: "gpt-5.5-medium", name: "GPT-5.5 Medium", contextLen: 200000},
	{id: "gpt-5.5-low", name: "GPT-5.5 Low", contextLen: 200000},
	{id: "gpt-5.4-high", name: "GPT-5.4 High", contextLen: 200000},
	{id: "gpt-5.4-medium", name: "GPT-5.4 Medium", contextLen: 200000},
	{id: "gpt-5.4-low", name: "GPT-5.4 Low", contextLen: 200000},
	{id: "gpt-5.3-codex-high", name: "GPT-5.3 Codex High", contextLen: 200000},
	{id: "gpt-5.3-codex-medium", name: "GPT-5.3 Codex Medium", contextLen: 200000},
	{id: "gpt-5.3-codex-low", name: "GPT-5.3 Codex Low", contextLen: 200000},
	{id: "gpt-5.2-high", name: "GPT-5.2 High", contextLen: 200000},
	{id: "gpt-5.2-medium", name: "GPT-5.2 Medium", contextLen: 200000},
	{id: "gpt-5.2-low", name: "GPT-5.2 Low", contextLen: 200000},
	{id: "gemini-3.1-pro-high", name: "Gemini 3.1 Pro High", contextLen: 1000000},
	{id: "gemini-3.1-pro-low", name: "Gemini 3.1 Pro Low", contextLen: 1000000},
	{id: "gemini-3.0-flash-high", name: "Gemini 3.0 Flash High", contextLen: 1000000},
	{id: "gemini-2.5-pro", name: "Gemini 2.5 Pro", contextLen: 1000000},
	{id: "deepseek-v4", name: "DeepSeek V4", contextLen: 1048576},
	{id: "kimi-k2.6", name: "Kimi K2.6", contextLen: 262144},
	{id: "glm-5.1", name: "GLM-5.1", contextLen: 204800},
}

// TestDevinCLICatalogCarriesEveryRegistryModel pins the catalogue the lane
// routes: the registry entry's own model list, under both the provider id and
// the alias the registry publishes.
func TestDevinCLICatalogCarriesEveryRegistryModel(t *testing.T) {
	for _, key := range []string{"devin-cli", "dv"} {
		models := ProviderModels[key]
		if len(models) == 0 {
			t.Fatalf("ProviderModels[%q] is empty: every model of the lane would be unroutable", key)
		}
		listed := make(map[string]bool, len(models))
		for _, id := range models {
			listed[id] = true
		}
		for _, model := range devinCLIRegistryModels {
			if !listed[model.id] {
				t.Errorf("ProviderModels[%q] lacks %q, which the upstream registry serves", key, model.id)
			}
		}
	}
	if got := GetProviderModels("devin"); len(got) != len(devinCLIRegistryModels) {
		t.Errorf("GetProviderModels(devin) = %d models, want the registry's %d", len(got), len(devinCLIRegistryModels))
	}
}

// TestDevinCLICatalogNamesMatchDashboard pins the display name of every model
// where the dashboard publishes it. models.ts spells the registry's names with
// spaces instead of hyphens ("SWE 1.6 Fast" for "SWE-1.6 Fast"), which is the
// convention of every block in that file — so a renamed model must be renamed in
// both places, and a missing one leaves a model the gateway routes invisible in
// the only UI that configures it.
func TestDevinCLICatalogNamesMatchDashboard(t *testing.T) {
	raw, err := os.ReadFile("../../web/src/lib/models.ts")
	if err != nil {
		t.Fatalf("read dashboard catalog: %v", err)
	}
	block := dashboardCatalogBlock(string(raw), "devin-cli")
	if block == "" {
		t.Fatal("web/src/lib/models.ts has no devin-cli block")
	}
	entryPattern := regexp.MustCompile(`"id":\s*"([^"]+)",\s*"name":\s*"([^"]+)"`)
	names := map[string]string{}
	for _, match := range entryPattern.FindAllStringSubmatch(block, -1) {
		names[match[1]] = match[2]
	}
	if len(names) != len(devinCLIRegistryModels) {
		t.Errorf("dashboard block lists %d models, want the registry's %d", len(names), len(devinCLIRegistryModels))
	}
	for _, model := range devinCLIRegistryModels {
		want := strings.ReplaceAll(model.name, "-", " ")
		if got, ok := names[model.id]; !ok {
			t.Errorf("dashboard block lacks %q", model.id)
		} else if got != want {
			t.Errorf("dashboard name for %q = %q, want %q (registry %q)", model.id, got, want, model.name)
		}
	}
}

// TestDevinCLIContextLengthsMatchRegistry pins the context windows the registry
// declares. The lane's own figures are the CLI's, and several of them disagree
// with the name-based tables this port resolves from (the substring table reads
// every GPT-5.2/5.3-Codex row as 128k, every Gemini row as 1.05M), so without
// these rows the dashboard budgets a window the CLI does not have.
func TestDevinCLIContextLengthsMatchRegistry(t *testing.T) {
	checked := 0
	for _, model := range devinCLIRegistryModels {
		if model.contextLen == 0 {
			continue
		}
		checked++
		detail := GetCapabilitiesDetailForModel("devin-cli", model.id)
		if detail.ContextWindow != model.contextLen {
			t.Errorf("devin-cli/%s contextWindow = %d, want the registry's %d",
				model.id, detail.ContextWindow, model.contextLen)
		}
	}
	if checked == 0 {
		t.Fatal("no model declared a contextLength: the fixture stopped describing the registry")
	}
}

// TestDevinCLILaneIsNoAuthStdio pins what makes this lane reachable at all: it
// is a noAuth stdio transport, so the fallback path dispatches it with a
// synthetic key and no connection row, and nothing may claim it speaks HTTP with
// a bearer token.
func TestDevinCLILaneIsNoAuthStdio(t *testing.T) {
	cfg, ok := KnownProviders["devin-cli"]
	if !ok {
		t.Fatal("KnownProviders has no devin-cli entry")
	}
	if !cfg.NoAuth {
		t.Error("devin-cli is not noAuth: with no connection row configured, every request dies as 'no active connections'")
	}
	if cfg.BaseURL != "devin://acp/stdio" {
		t.Errorf("BaseURL = %q, want the stdio transport marker", cfg.BaseURL)
	}
	if cfg.AuthHeader != "" || cfg.AuthScheme != "" {
		t.Errorf("auth header/scheme = (%q, %q), want none: a stdio child authenticates from `devin auth login`, not from a header",
			cfg.AuthHeader, cfg.AuthScheme)
	}
	if cfg.DefaultAPIKey != "" {
		t.Errorf("DefaultAPIKey = %q, want empty so the fallback path uses its own placeholder", cfg.DefaultAPIKey)
	}
	if got := ResolveAlias("dv"); got != "devin-cli" {
		t.Errorf("ResolveAlias(dv) = %q, want devin-cli", got)
	}
	if got := ResolveAlias("devin"); got != "devin-cli" {
		t.Errorf("ResolveAlias(devin) = %q, want devin-cli", got)
	}

	meta, ok := ProviderRegistry["devin-cli"]
	if !ok {
		t.Fatal("ProviderRegistry has no devin-cli entry")
	}
	if meta.AuthType != "none" || !meta.NoAuth {
		t.Errorf("registry auth = (%q, noAuth=%v), want none/true", meta.AuthType, meta.NoAuth)
	}
	if !IsNoAuthProvider("devin-cli") {
		t.Error("IsNoAuthProvider(devin-cli) is false: the dashboard would not offer it without a key")
	}
}
