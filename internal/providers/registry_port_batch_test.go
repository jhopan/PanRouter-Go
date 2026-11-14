package providers

import (
	"strings"
	"testing"
)

// TestRegistryOnlyProvidersParity pins the registry entries ported from
// upstream open-sse/providers/registry (2026-11 batch): agentrouter, apinex,
// apmix, freemodel-dev, justdowork, orcarouter, vyceai and the hidden mmf lane.
//
// These arrived the way the v0.5.91 aggregators did — as registry-only
// providers with no bespoke executor — so the same rule applies: an entry is
// only real once every table carries it. A provider that reaches KnownProviders
// but not ProviderAliasMap is invisible from a short prefix, one missing from
// ProviderRegistry never renders in the dashboard, and one whose models are
// absent from ProviderModels answers /v1/models with nothing.
func TestRegistryOnlyProvidersParity(t *testing.T) {
	tests := []struct {
		id          string
		baseURL     string
		format      string
		authHeader  string
		authScheme  string
		noAuth      bool
		hidden      bool
		category    Category
		alias       string   // published prefix (RegistryAliases)
		resolve     []string // every spelling upstream resolves to this id
		resolveTo   string   // expected ResolveAlias(id) when another provider's alias shadows it
		catalogKeys []string // ProviderModels keys that must carry the models
		models      []string
		modelsList  string
	}{
		{
			// Anthropic Messages behind a client-identity gate: the spoof
			// headers and the ?beta=true suffix are what make it answer.
			id: "agentrouter", baseURL: "https://agentrouter.org/v1/messages?beta=true",
			format: FormatClaude, authHeader: "x-api-key", authScheme: "raw",
			category: CategoryAPIKey, alias: "AR",
			resolve:     []string{"AR", "ar", "agentrouter"},
			catalogKeys: []string{"agentrouter", "AR"},
			models:      []string{"claude-opus-4-6", "claude-haiku-4-5-20251001", "glm-5.1", "deepseek-v3.2"},
		},
		{
			id: "apinex", baseURL: "https://api.apinex.bond/v1/chat/completions",
			category: CategoryAPIKey, alias: "apinex",
			resolve:     []string{"apinex", "apn"},
			catalogKeys: []string{"apinex"},
			models:      []string{"free/glm-5.3-flash"},
			modelsList:  "https://api.apinex.bond/v1/models",
		},
		{
			id: "apmix", baseURL: "https://api.apmix.ai/v1/chat/completions",
			category: CategoryAPIKey, alias: "am",
			resolve:     []string{"am", "apmix", "apmix-ai"},
			catalogKeys: []string{"apmix", "am"},
			models:      []string{"stealth/space-bunny-free", "deepseek/deepseek-v4-flash-free"},
			modelsList:  "https://api.apmix.ai/v1/models",
		},
		{
			id: "freemodel-dev", baseURL: "https://api.freemodel.dev/v1/chat/completions",
			category: CategoryAPIKey, alias: "fmd",
			resolve:     []string{"fmd", "freemodel", "freemodel-dev"},
			catalogKeys: []string{"freemodel-dev", "fmd"},
			models: []string{
				"gpt-5.6-luna", "gpt-5.6-sol", "gpt-5.6-terra",
				"gpt-5.5", "gpt-5.4", "gpt-5.4-mini", "gpt-5.3-codex",
			},
			modelsList: "https://api.freemodel.dev/v1/models",
		},
		{
			// Anthropic-shaped endpoint: the OpenAI-compatible path upstream is
			// Cloudflare-blocked from some regions, so format claude is declared.
			id: "justdowork", baseURL: "https://api.justwoker.icu/v1/messages",
			format: FormatClaude, authHeader: "x-api-key", authScheme: "raw",
			category: CategoryAPIKey, alias: "jd",
			resolve:     []string{"jd", "justdowork", "justwoker"},
			catalogKeys: []string{"justdowork", "jd"},
			models:      []string{"claude-opus-4-8"},
			modelsList:  "https://api.justwoker.icu/v1/models",
		},
		{
			id: "orcarouter", baseURL: "https://api.orcarouter.ai/v1/responses",
			category: CategoryAPIKey, alias: "orca",
			resolve:     []string{"orca", "orcarouter"},
			catalogKeys: []string{"orcarouter", "orca"},
			models: []string{
				"deepseek/deepseek-v4-flash-free", "tencent/hy3-free",
				"z-ai/glm-5.3-flash-free", "orca/orcaverify-text1.0-free",
			},
			modelsList: "https://api.orcarouter.ai/v1/models",
		},
		{
			id: "vyceai", baseURL: "https://vyceai.com/v1/chat/completions",
			category: CategoryAPIKey, alias: "vy",
			resolve:     []string{"vy", "vyceai"},
			catalogKeys: []string{"vyceai", "vy"},
			models:      []string{"qwen3.8-flash", "deepseek-v4-flash", "agnes-3.0-flash", "deepseek-v4.1", "claude-sonnet-4-6"},
			modelsList:  "https://vyceai.com/v1/models",
		},
		{
			// Upstream's hidden no-auth MiMo lane. It shares mimo-free's model,
			// so it carries a transport and a registry row but no catalog row
			// of its own — mimo-free's alias row already holds "mimo-auto".
			//
			// `mmf` also resolves through mimo-free's declared alias, which is
			// upstream's own ambiguity: ALIAS_TO_PROVIDER_ID is set by both the
			// mmf entry (id) and mimo-free's alias, so the last writer wins.
			// This port keeps mimo-free, and models_hidden_test.go pins it.
			id: "mmf", baseURL: "https://api.xiaomimimo.com/api/free-ai/openai/chat",
			noAuth: true, hidden: true, category: CategoryAPIKey, alias: "mmf",
			resolve:     []string{},
			resolveTo:   "mimo-free",
			catalogKeys: []string{"mmf"},
			models:      []string{"mimo-auto"},
		},
		{
			// One node, two endpoint families behind one key; ForwardFreeAI
			// moves the vision models to the OpenAI-compatible base.
			id: "freeai", baseURL: "https://api.free.ai/v1/chat/",
			category: CategoryAPIKey, alias: "fai",
			resolve:     []string{"fai", "free-ai", "free.ai", "freeai"},
			catalogKeys: []string{"freeai", "fai"},
			models: []string{
				"qwen7b", "qwen3-8b", "deepseek-r1", "deepseek-r1-7b", "mistral",
				"qwen-coder", "qwen3-coder", "qwen-vl", "qwen25-vl", "moondream2",
			},
		},
		{
			// Kept for connections created before the two families merged.
			id: "freeai-vl", baseURL: "https://api.free.ai/v1/chat/completions",
			hidden: true, category: CategoryAPIKey, alias: "fai-vl",
			resolve:     []string{"fai-vl", "free-ai-vl", "freeai-vision"},
			catalogKeys: []string{"freeai-vl", "fai-vl"},
			models:      []string{"qwen-vl", "qwen25-vl", "moondream2"},
		},
		{
			// Media lanes: the transport is what the page needs to dial a
			// self-hosted server, and the kind table is what keeps their single
			// model out of the LLM list.
			id: "selfhosted-tts", baseURL: "http://localhost:8880",
			category: CategoryAPIKey, alias: "selfhosted-tts",
			resolve:     []string{"selfhosted-tts"},
			catalogKeys: []string{"selfhosted-tts"},
			models:      []string{"kokoro"},
		},
		{
			id: "selfhosted-stt", baseURL: "http://localhost:8080",
			category: CategoryAPIKey, alias: "selfhosted-stt",
			resolve:     []string{"selfhosted-stt"},
			catalogKeys: []string{"selfhosted-stt"},
			models:      []string{"whisper-1"},
		},
		{
			id: "selfhosted-embedding", baseURL: "http://localhost:8080/v1/embeddings",
			category: CategoryAPIKey, alias: "selfhosted-embedding",
			resolve:     []string{"selfhosted-embedding"},
			catalogKeys: []string{"selfhosted-embedding"},
			models:      []string{"embedding"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			cfg, ok := KnownProviders[tt.id]
			if !ok {
				t.Fatalf("KnownProviders has no entry for %q", tt.id)
			}
			if cfg.BaseURL != tt.baseURL {
				t.Errorf("BaseURL = %q, want %q", cfg.BaseURL, tt.baseURL)
			}
			if cfg.Format != tt.format {
				t.Errorf("Format = %q, want %q", cfg.Format, tt.format)
			}
			if cfg.NoAuth != tt.noAuth {
				t.Errorf("NoAuth = %v, want %v", cfg.NoAuth, tt.noAuth)
			}
			if cfg.Hidden != tt.hidden {
				t.Errorf("Hidden = %v, want %v", cfg.Hidden, tt.hidden)
			}
			wantHeader, wantScheme := tt.authHeader, tt.authScheme
			if wantHeader == "" && !tt.noAuth {
				wantHeader, wantScheme = "Authorization", "bearer"
			}
			if cfg.AuthHeader != wantHeader || cfg.AuthScheme != wantScheme {
				t.Errorf("auth = %q/%q, want %q/%q", cfg.AuthHeader, cfg.AuthScheme, wantHeader, wantScheme)
			}

			// Prefix resolution: every spelling upstream's ALIAS_TO_PROVIDER_ID
			// carries must land on this provider and no other.
			for _, spelling := range tt.resolve {
				if got := ResolveAlias(spelling); got != tt.id {
					t.Errorf("ResolveAlias(%q) = %q, want %q", spelling, got, tt.id)
				}
			}
			// Published prefix: what /v1/models and the dashboard show.
			if got := GetProviderAlias(tt.id); got != tt.alias {
				t.Errorf("GetProviderAlias(%q) = %q, want %q", tt.id, got, tt.alias)
			}
			if tt.resolveTo != "" {
				if got := ResolveAlias(tt.id); got != tt.resolveTo {
					t.Errorf("ResolveAlias(%q) = %q, want %q", tt.id, got, tt.resolveTo)
				}
			}

			// Offline catalogue: every key the provider is reachable under.
			for _, key := range tt.catalogKeys {
				got := GetProviderModels(key)
				if len(got) != len(tt.models) {
					t.Errorf("catalog[%q] has %d models, want %d", key, len(got), len(tt.models))
				}
				for _, m := range tt.models {
					if !slicesContains(got, m) {
						t.Errorf("catalog[%q] missing %q", key, m)
					}
				}
			}

			if got := ModelsListURL(tt.id); got != tt.modelsList {
				t.Errorf("ModelsListURL(%q) = %q, want %q", tt.id, got, tt.modelsList)
			}

			// Dashboard metadata: a provider with a transport but no row here
			// routes and stays invisible.
			meta, ok := ProviderRegistry[tt.id]
			if !ok {
				t.Fatalf("ProviderRegistry has no entry for %q", tt.id)
			}
			if meta.Category != tt.category {
				t.Errorf("Category = %q, want %q", meta.Category, tt.category)
			}
			if meta.Alias != tt.alias {
				t.Errorf("meta Alias = %q, want %q", meta.Alias, tt.alias)
			}
			if meta.NoAuth != tt.noAuth || meta.Hidden != tt.hidden {
				t.Errorf("meta noAuth/hidden = %v/%v, want %v/%v", meta.NoAuth, meta.Hidden, tt.noAuth, tt.hidden)
			}
			if tt.modelsList != "" && meta.ModelsFetcher == nil {
				t.Error("provider has a live catalogue endpoint but no modelsFetcher for the dashboard")
			}
		})
	}
}

// TestAgentRouterCarriesTheClaudeCLIFingerprint pins the two things the WAF
// gate actually reads. Upstream attaches CLAUDE_CLI_SPOOF_HEADERS to this entry
// and nothing else; a bare anthropic-version is rejected, and a fingerprint
// missing the Stainless block is still not the official client.
func TestAgentRouterCarriesTheClaudeCLIFingerprint(t *testing.T) {
	cfg, ok := KnownProviders["agentrouter"]
	if !ok {
		t.Fatal("agentrouter missing from KnownProviders")
	}
	for header, want := range map[string]string{
		"anthropic-version":           "2023-06-01",
		"User-Agent":                  "claude-cli/" + ClaudeCLIVersion + " (external, sdk-cli)",
		"X-App":                       "cli",
		"X-Stainless-Lang":            "js",
		"X-Stainless-Runtime":         "node",
		"X-Stainless-Helper-Method":   "stream",
		"X-Stainless-Package-Version": "0.80.0",
		"X-Stainless-Runtime-Version": "v24.14.0",
		"X-Stainless-Timeout":         "600",
	} {
		if got := cfg.StaticHeaders[header]; got != want {
			t.Errorf("StaticHeaders[%q] = %q, want %q", header, got, want)
		}
	}
	if got := cfg.StaticHeaders["Anthropic-Beta"]; !strings.Contains(got, "claude-code-20250219") ||
		!strings.Contains(got, "redact-thinking-2026-02-12") {
		t.Errorf("Anthropic-Beta = %q, want the full Claude CLI beta list", got)
	}
	// The Stainless pair describes the host the fingerprint claims to be, so it
	// must be one of the mapped values rather than an empty string.
	for _, header := range []string{"X-Stainless-Os", "X-Stainless-Arch"} {
		if cfg.StaticHeaders[header] == "" {
			t.Errorf("StaticHeaders[%q] is empty; the CLI fingerprint always sends it", header)
		}
	}
}

// TestOrcaRouterPinsTheResponsesLane pins the transport contract: every model
// this provider lists is declared openai-responses, and no Chat Completions
// passthrough is declared, because the provider publishes only /v1/responses.
func TestOrcaRouterPinsTheResponsesLane(t *testing.T) {
	for _, key := range []string{"orcarouter", "orca"} {
		for _, model := range GetProviderModels(key) {
			formats, declared := GetModelFormats(key, model)
			if !declared {
				t.Errorf("GetModelFormats(%q, %q) is undeclared; the Responses lane must be pinned", key, model)
				continue
			}
			if formats.TargetFormat != FormatOpenAIResponses {
				t.Errorf("GetModelFormats(%q, %q).TargetFormat = %q, want %q",
					key, model, formats.TargetFormat, FormatOpenAIResponses)
			}
			if !slicesContains(formats.SupportedFormats, FormatOpenAIResponses) {
				t.Errorf("GetModelFormats(%q, %q) does not support the Responses format it dials", key, model)
			}
			if slicesContains(formats.SupportedFormats, FormatOpenAI) {
				t.Errorf("GetModelFormats(%q, %q) claims Chat Completions support the provider does not publish", key, model)
			}
		}
	}
}
