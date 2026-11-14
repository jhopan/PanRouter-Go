package providers

import "runtime"

// PanRouter-only providers and catalogue lanes that panrouter-go does not carry.
// They are applied in init so the upstream literals in each table stay
// comparable with upstream, and a future sync cannot silently drop them.

func init() {
	// MiniMax Code turns prefill for minutes on a 512k context, so its two
	// budgets are declared rather than inherited: 15m between chunks and 10m
	// before the first one (AGENTS.md §1.4).
	for _, id := range []string{"minimax-code", "minimax-code-global"} {
		cfg := KnownProviders[id]
		cfg.StallTimeoutMs = 900000
		cfg.FirstChunkTimeoutMs = 600000
		KnownProviders[id] = cfg
	}
	ProviderRegistry["agentrouter"] = ProviderMeta{
		ID:                "agentrouter",
		Name:              "AgentRouter",
		Alias:             "AR",
		Category:          CategoryAPIKey,
		AuthType:          "apikey",
		Color:             "#7C3AED",
		Icon:              "hub",
		Priority:          40,
		Usage:             true,
		AuthModes:         []string{"apikey"},
		PassthroughModels: true,
		ServiceKinds:      []string{"llm"},
		Website:           "https://agentrouter.org",
		Notice:            &Notice{APIKeyURL: "https://agentrouter.org"},
	}
	KnownProviders["agentrouter"] = ProviderConfig{
		BaseURL:    "https://agentrouter.org/v1/messages?beta=true",
		AuthHeader: "x-api-key",
		AuthScheme: "raw",
		Format:     FormatClaude,
		// transport.urlSuffix upstream; ?beta=true is part of the endpoint this
		// provider accepts, and appendBetaQuery leaves an existing one alone.
		StaticHeaders: claudeCLISpoofHeaders,
	}
	// OpenAI chat-completions aggregators. Their catalogues are snapshots in
	// registry_models.go plus a live /v1/models entry in modelsListURL; a model
	// added upstream after the snapshot is discovered through that endpoint.
	ProviderRegistry["apinex"] = ProviderMeta{
		ID:                "apinex",
		Name:              "APInex",
		Alias:             "apinex",
		Category:          CategoryAPIKey,
		AuthType:          "apikey",
		Color:             "#8B5CF6",
		Icon:              "hub",
		Priority:          64,
		AuthModes:         []string{"apikey"},
		PassthroughModels: true,
		ServiceKinds:      []string{"llm"},
		Website:           "https://apinex.bond/",
		Notice:            &Notice{Text: "APInex gateway - OpenAI chat-completions transport. Free-tier models use the free/ prefix. Key from the keys page.", APIKeyURL: "https://apinex.bond/keys"},
		ModelsFetcher:     &Fetcher{URL: "https://api.apinex.bond/v1/models", Type: "openai"},
	}
	KnownProviders["apinex"] = ProviderConfig{
		BaseURL:    "https://api.apinex.bond/v1/chat/completions",
		AuthHeader: "Authorization",
		AuthScheme: "bearer",
	}
	ProviderRegistry["apmix"] = ProviderMeta{
		ID:                "apmix",
		Name:              "APIMIX.AI",
		Alias:             "am",
		Category:          CategoryAPIKey,
		AuthType:          "apikey",
		Color:             "#06B6D4",
		Icon:              "blend",
		Priority:          63,
		AuthModes:         []string{"apikey"},
		PassthroughModels: true,
		ServiceKinds:      []string{"llm"},
		Website:           "https://apmix.ai/",
		Notice:            &Notice{Text: "APIMIX.AI - OpenAI chat-completions transport. API key from the dashboard.", APIKeyURL: "https://apmix.ai/dashboard/keys"},
		ModelsFetcher:     &Fetcher{URL: "https://api.apmix.ai/v1/models", Type: "openai"},
	}
	KnownProviders["apmix"] = ProviderConfig{
		BaseURL:    "https://api.apmix.ai/v1/chat/completions",
		AuthHeader: "Authorization",
		AuthScheme: "bearer",
	}
	ProviderRegistry["freemodel-dev"] = ProviderMeta{
		ID:                "freemodel-dev",
		Name:              "FreeModel.dev",
		Alias:             "fmd",
		Category:          CategoryAPIKey,
		AuthType:          "apikey",
		Color:             "#10B981",
		Icon:              "rocket_launch",
		Priority:          62,
		AuthModes:         []string{"apikey"},
		PassthroughModels: true,
		ServiceKinds:      []string{"llm"},
		Website:           "https://freemodel.dev/",
		Notice:            &Notice{Text: "FreeModel.dev — one-time initial free credits (GPT-5.x family, 400k context). Key from freemodel.dev. one-time-initial budget: not recurring.", APIKeyURL: "https://freemodel.dev/"},
		ModelsFetcher:     &Fetcher{URL: "https://api.freemodel.dev/v1/models", Type: "openai"},
	}
	KnownProviders["freemodel-dev"] = ProviderConfig{
		BaseURL:    "https://api.freemodel.dev/v1/chat/completions",
		AuthHeader: "Authorization",
		AuthScheme: "bearer",
	}
	// JustDoWork answers /v1/messages (Anthropic shape) and only that: the
	// OpenAI-compatible path upstream is Cloudflare-blocked from some regions,
	// which is why the registry declares format claude instead of the default.
	ProviderRegistry["justdowork"] = ProviderMeta{
		ID:           "justdowork",
		Name:         "JustDoWork",
		Alias:        "jd",
		Category:     CategoryAPIKey,
		AuthType:     "apikey",
		Color:        "#8B5CF6",
		Icon:         "task",
		Priority:     64,
		AuthModes:    []string{"apikey"},
		ServiceKinds: []string{"llm"},
		Website:      "https://api.justwoker.icu/",
		Notice:       &Notice{Text: "JustDoWork - Anthropic-compatible (verified live; the OpenAI /v1/chat/completions path is Cloudflare-blocked from some regions, /v1/messages works). API key required.", APIKeyURL: "https://api.justwoker.icu/"},
		ModelsFetcher: &Fetcher{
			URL: "https://api.justwoker.icu/v1/models", Type: "openai",
		},
	}
	KnownProviders["justdowork"] = ProviderConfig{
		BaseURL:    "https://api.justwoker.icu/v1/messages",
		AuthHeader: "x-api-key",
		AuthScheme: "raw",
		Format:     FormatClaude,
		StaticHeaders: map[string]string{
			"anthropic-version": "2023-06-01",
		},
	}
	// OrcaRouter speaks the Responses API on its only declared transport, so its
	// four models pin openai-responses in model_formats.go rather than relying
	// on the /chat/completions default.
	ProviderRegistry["orcarouter"] = ProviderMeta{
		ID:                "orcarouter",
		Name:              "OrcaRouter",
		Alias:             "orca",
		Category:          CategoryAPIKey,
		AuthType:          "apikey",
		Color:             "#0EA5E9",
		Icon:              "sailing",
		Priority:          63,
		AuthModes:         []string{"apikey"},
		PassthroughModels: true,
		ServiceKinds:      []string{"llm"},
		Website:           "https://www.orcarouter.ai/",
		Notice:            &Notice{Text: "OrcaRouter — Responses-API gateway. Free tier models (-free suffix) plus paid frontier models. Token from the console.", APIKeyURL: "https://www.orcarouter.ai/console/token"},
		ModelsFetcher:     &Fetcher{URL: "https://api.orcarouter.ai/v1/models", Type: "openai"},
	}
	KnownProviders["orcarouter"] = ProviderConfig{
		BaseURL:    "https://api.orcarouter.ai/v1/responses",
		AuthHeader: "Authorization",
		AuthScheme: "bearer",
	}
	ProviderRegistry["vyceai"] = ProviderMeta{
		ID:                "vyceai",
		Name:              "VYCEAI",
		Alias:             "vy",
		Category:          CategoryAPIKey,
		AuthType:          "apikey",
		Color:             "#10B981",
		Icon:              "bolt",
		Priority:          66,
		AuthModes:         []string{"apikey"},
		PassthroughModels: true,
		ServiceKinds:      []string{"llm"},
		Website:           "https://vyceai.com/",
		Notice:            &Notice{Text: "VYCEAI - OpenAI chat-completions transport. API key required.", APIKeyURL: "https://vyceai.com/"},
		ModelsFetcher:     &Fetcher{URL: "https://vyceai.com/v1/models", Type: "openai"},
	}
	// MMF and the self-hosted media lanes already carry registry rows further
	// down this map; the port only adds what was missing.
	KnownProviders["vyceai"] = ProviderConfig{
		BaseURL:    "https://vyceai.com/v1/chat/completions",
		AuthHeader: "Authorization",
		AuthScheme: "bearer",
	}
	// MMF is the hidden no-auth MiMo lane upstream keeps beside mimo-free,
	// whose alias is also "mmf". Both entries exist upstream; ProviderAliasMap
	// resolves the `mmf/` prefix through mimo-free's declared alias, so this id
	// is reached by a stored connection row rather than by a model prefix.
	ProviderRegistry["freeai"] = ProviderMeta{
		ID:                "freeai",
		Name:              "Free.ai",
		Alias:             "fai",
		Category:          CategoryAPIKey,
		AuthType:          "apikey",
		Color:             "#10B981",
		Icon:              "all_inclusive",
		Priority:          62,
		AuthModes:         []string{"apikey"},
		PassthroughModels: true,
		ServiceKinds:      []string{"llm"},
		Website:           "https://free.ai/",
		Notice:            &Notice{Text: "Free.ai: satu node untuk chat + code + vision (qwen-vl, qwen25-vl, moondream2). 60 RPM / 1000 req/month per key, key sekali untuk semua model. Key dari free.ai.", APIKeyURL: "https://free.ai/"},
	}
	KnownProviders["freeai"] = ProviderConfig{
		BaseURL:    "https://api.free.ai/v1/chat/",
		AuthHeader: "Authorization",
		AuthScheme: "bearer",
	}
	// Upstream kept this entry only for connections created before the two
	// families merged, so it is hidden but still routable.
	ProviderRegistry["freeai-vl"] = ProviderMeta{
		ID:                "freeai-vl",
		Name:              "Free.ai Vision",
		Alias:             "fai-vl",
		Category:          CategoryAPIKey,
		AuthType:          "apikey",
		Color:             "#10B981",
		Icon:              "visibility",
		Priority:          63,
		Hidden:            true,
		AuthModes:         []string{"apikey"},
		PassthroughModels: true,
		ServiceKinds:      []string{"llm"},
		Website:           "https://free.ai/",
		Notice:            &Notice{Text: "Free.ai Vision — DEPRECATED. Model sudah digabung ke provider freeai (fai) satu node satu key; freeai-vl dipertahankan untuk kompatibilitas koneksi lama saja.", APIKeyURL: "https://free.ai/"},
	}
	KnownProviders["freeai-vl"] = ProviderConfig{
		BaseURL:    "https://api.free.ai/v1/chat/completions",
		AuthHeader: "Authorization",
		AuthScheme: "bearer",
		Hidden:     true,
	}
	// Self-hosted, OpenAI-compatible media servers. Every other self-hostable
	// media provider here carries a fixed localhost endpoint and no credential
	// record, which is exactly why those cannot be pointed at another host:
	// providerSpecificData.baseUrl lives on the credentials row. These three
	// declare authType apikey for that reason — a local server ignores the key,
	// any non-empty value works, and the media handler prefers a connection
	// base URL over the registry default when one is set.
	ProviderRegistry["selfhosted-tts"] = ProviderMeta{
		ID:       "selfhosted-tts",
		Name:     "Self-Hosted TTS",
		Alias:    "selfhosted-tts",
		Category: CategoryAPIKey,
		// Upstream declares authType apikey rather than none: the credential
		// row is where providerSpecificData.baseUrl lives, which is the only
		// way this provider can be pointed at another host. A local server
		// ignores the key itself, so any non-empty value works.
		AuthType:     "apikey",
		Color:        "#10B981",
		Icon:         "volume_up",
		Priority:     50,
		ServiceKinds: []string{"tts"},
		Website:      "https://github.com/remsky/Kokoro-FastAPI",
	}
	KnownProviders["selfhosted-tts"] = ProviderConfig{
		BaseURL:    "http://localhost:8880",
		AuthHeader: "Authorization",
		AuthScheme: "bearer",
		TTSURL:     "http://localhost:8880/v1/audio/speech",
	}
	ProviderRegistry["selfhosted-stt"] = ProviderMeta{
		ID:           "selfhosted-stt",
		Name:         "Self-Hosted STT",
		Alias:        "selfhosted-stt",
		Category:     CategoryAPIKey,
		AuthType:     "apikey",
		Color:        "#3B82F6",
		Icon:         "mic",
		Priority:     50,
		ServiceKinds: []string{"stt"},
		Website:      "https://github.com/ggml-org/whisper.cpp",
	}
	KnownProviders["selfhosted-stt"] = ProviderConfig{
		BaseURL:    "http://localhost:8080",
		AuthHeader: "Authorization",
		AuthScheme: "bearer",
		STTURL:     "http://localhost:8080/v1/audio/transcriptions",
	}
	// The embedding lane resolves the whole URL from the base, the way the
	// jina-ai entry above does; the registry default only makes a same-host
	// deployment work out of the box.
	KnownProviders["selfhosted-embedding"] = ProviderConfig{
		BaseURL:    "http://localhost:8080/v1/embeddings",
		AuthHeader: "Authorization",
		AuthScheme: "bearer",
	}
	// Muse — Meta's Model API. Dual auth upstream (Muse Code subscription
	// device-code key and a dev.meta.ai pay-as-you-go key); both ride the same
	// bearer transport, and only an account-issued (OAuth) key needs the
	// protocol version header. Every Muse Spark model declares
	// targetFormat "openai-responses" (see museModelFormats), so /v1/responses
	// is the transport the executor dials for this provider.
	ProviderRegistry["codebuddy-intl"] = ProviderMeta{
		ID:           "codebuddy-intl",
		Name:         "CodeBuddy",
		Alias:        "cbai",
		Category:     CategoryOAuth,
		Color:        "#006EFF",
		Icon:         "smart_toy",
		AuthModes:    []string{"oauth", "apikey"},
		ServiceKinds: []string{"llm"},
		Website:      "https://www.codebuddy.ai",
		Notice:       &Notice{SignupURL: "https://www.codebuddy.ai"},
	}
	KnownProviders["codebuddy-intl"] = ProviderConfig{
		BaseURL:        "https://www.codebuddy.ai/v2/chat/completions",
		AuthHeader:     "Authorization",
		AuthScheme:     "bearer",
		ThinkingFormat: "openai",
		// The intl lane mimics CodeBuddy Code CLI 2.178.0 on every surface it
		// touches: the registry used to advertise "IDE/2.160.0" for OAuth while
		// the chat executor said "CLI/2.178.0" — one account, two identities,
		// and the gateway read the mismatch as a hand-rolled client. The
		// identity values live in codebuddy_identity.go so the chat executor,
		// the OAuth refresher and the login flow agree by construction; the
		// executor adds the rest of the CLI header set (the official CLI sends
		// ~25 headers, not these seven) on the chat path.
		StaticHeaders: map[string]string{
			"User-Agent":          CodeBuddyUserAgent,
			"X-Product":           "SaaS",
			"X-IDE-Type":          "CLI",
			"X-IDE-Name":          "CLI",
			"X-IDE-Version":       CodeBuddyCLIVersion,
			"x-requested-with":    "XMLHttpRequest",
			"x-codebuddy-request": "1",
		},
	}
	RegistryAliases["agentrouter"] = "AR"
	RegistryAliases["apinex"] = "apinex"
	RegistryAliases["apmix"] = "am"
	RegistryAliases["freemodel-dev"] = "fmd"
	RegistryAliases["justdowork"] = "jd"
	RegistryAliases["orcarouter"] = "orca"
	RegistryAliases["vyceai"] = "vy"
	RegistryAliases["freeai"] = "fai"
	RegistryAliases["freeai-vl"] = "fai-vl"
	ProviderAliasMap["AR"] = "agentrouter"
	ProviderAliasMap["ar"] = "agentrouter"
	ProviderAliasMap["apn"] = "apinex"
	ProviderAliasMap["am"] = "apmix"
	ProviderAliasMap["apmix-ai"] = "apmix"
	ProviderAliasMap["fmd"] = "freemodel-dev"
	ProviderAliasMap["freemodel"] = "freemodel-dev"
	ProviderAliasMap["jd"] = "justdowork"
	ProviderAliasMap["justwoker"] = "justdowork"
	ProviderAliasMap["orca"] = "orcarouter"
	ProviderAliasMap["vy"] = "vyceai"
	ProviderAliasMap["fai"] = "freeai"
	ProviderAliasMap["free-ai"] = "freeai"
	ProviderAliasMap["free.ai"] = "freeai"
	ProviderAliasMap["fai-vl"] = "freeai-vl"
	ProviderAliasMap["free-ai-vl"] = "freeai-vl"
	ProviderAliasMap["freeai-vision"] = "freeai-vl"
	modelsListURL["apinex"] = "https://api.apinex.bond/v1/models"
	modelsListURL["apmix"] = "https://api.apmix.ai/v1/models"
	modelsListURL["freemodel-dev"] = "https://api.freemodel.dev/v1/models"
	modelsListURL["justdowork"] = "https://api.justwoker.icu/v1/models"
	modelsListURL["orcarouter"] = "https://api.orcarouter.ai/v1/models"
	modelsListURL["vyceai"] = "https://vyceai.com/v1/models"
	ProviderModels["agentrouter"] = []string{"claude-opus-4-6", "claude-haiku-4-5-20251001", "glm-5.1", "deepseek-v3.2"}
	ProviderModels["apinex"] = []string{"free/glm-5.3-flash"}
	ProviderModels["apmix"] = []string{"stealth/space-bunny-free", "deepseek/deepseek-v4-flash-free"}
	ProviderModels["freemodel-dev"] = []string{
		"gpt-5.6-luna", "gpt-5.6-sol", "gpt-5.6-terra",
		"gpt-5.5", "gpt-5.4", "gpt-5.4-mini", "gpt-5.3-codex",
	}
	ProviderModels["justdowork"] = []string{"claude-opus-4-8"}
	ProviderModels["orcarouter"] = []string{"deepseek/deepseek-v4-flash-free", "tencent/hy3-free", "z-ai/glm-5.3-flash-free", "orca/orcaverify-text1.0-free"}
	ProviderModels["vyceai"] = []string{"qwen3.8-flash", "deepseek-v4-flash", "agnes-3.0-flash", "deepseek-v4.1", "claude-sonnet-4-6"}
	ProviderModels["freeai"] = []string{
		"qwen7b", "qwen3-8b", "deepseek-r1", "deepseek-r1-7b", "mistral",
		"qwen-coder", "qwen3-coder", "qwen-vl", "qwen25-vl", "moondream2",
	}
	ProviderModels["freeai-vl"] = []string{"qwen-vl", "qwen25-vl", "moondream2"}
	ProviderModels["selfhosted-tts"] = []string{"kokoro"}
	ProviderModels["selfhosted-stt"] = []string{"whisper-1"}
	ProviderModels["selfhosted-embedding"] = []string{"embedding"}
	ProviderModels["am"] = []string{"stealth/space-bunny-free", "deepseek/deepseek-v4-flash-free"}
	ProviderModels["fmd"] = []string{
		"gpt-5.6-luna", "gpt-5.6-sol", "gpt-5.6-terra",
		"gpt-5.5", "gpt-5.4", "gpt-5.4-mini", "gpt-5.3-codex",
	}
	ProviderModels["jd"] = []string{"claude-opus-4-8"}
	ProviderModels["orca"] = []string{"deepseek/deepseek-v4-flash-free", "tencent/hy3-free", "z-ai/glm-5.3-flash-free", "orca/orcaverify-text1.0-free"}
	ProviderModels["vy"] = []string{"qwen3.8-flash", "deepseek-v4-flash", "agnes-3.0-flash", "deepseek-v4.1", "claude-sonnet-4-6"}
	// Free.ai: one node, both families. The chat/code ids answer on /v1/chat/,
	// the three vision ids on /v1/chat/completions (ForwardFreeAI moves them).
	ProviderModels["fai"] = []string{
		"qwen7b", "qwen3-8b", "deepseek-r1", "deepseek-r1-7b", "mistral",
		"qwen-coder", "qwen3-coder", "qwen-vl", "qwen25-vl", "moondream2",
	}
	ProviderModels["fai-vl"] = []string{"qwen-vl", "qwen25-vl", "moondream2"}
	// mmf carries no row of its own: upstream's mmf and mimo-free both serve the
	// single "mimo-auto" model, and mimo-free's alias row below already holds it.
	ProviderModels["cline"] = append(ProviderModels["cline"], "cline-free/muse-spark-1.3-contributor", "cline-free/solar-pro4", "cline-free/longcat-2.0")
	ProviderModels["qoder"] = append(ProviderModels["qoder"], "dfmodel_v41")
	ProviderModels["qoder-cn"] = append(ProviderModels["qoder-cn"], "dfmodel_v41")
	ProviderModels["xiaomi-mimo"] = append([]string{"mimo-v2.6-pro", "mimo-v2.6-flash", "mimo-v2.6-pro-ultraspeed"}, ProviderModels["xiaomi-mimo"]...)
	// OrcaRouter declares one Responses transport and every model carries targetFormat
	// "openai-responses"; Chat Completions passthrough is deliberately absent.
	orcaFormats := map[string]ModelFormats{
		"deepseek/deepseek-v4-flash-free": {TargetFormat: FormatOpenAIResponses, SupportedFormats: []string{FormatOpenAIResponses}},
		"tencent/hy3-free":                {TargetFormat: FormatOpenAIResponses, SupportedFormats: []string{FormatOpenAIResponses}},
		"z-ai/glm-5.3-flash-free":         {TargetFormat: FormatOpenAIResponses, SupportedFormats: []string{FormatOpenAIResponses}},
		"orca/orcaverify-text1.0-free":    {TargetFormat: FormatOpenAIResponses, SupportedFormats: []string{FormatOpenAIResponses}},
	}
	ProviderModelFormats["orcarouter"] = orcaFormats
	ProviderModelFormats["orca"] = orcaFormats // The hidden MiMo free lane: upstream's mmf and mimo-free serve the same
	// "mimo-auto" model, so the catalogue row is shared and the entry stays
	// visible to dispatch.
	KnownProviders["mmf"] = ProviderConfig{
		BaseURL:    "https://api.xiaomimimo.com/api/free-ai/openai/chat",
		AuthHeader: "",
		AuthScheme: "",
		NoAuth:     true,
		Hidden:     true,
	}
	ProviderRegistry["mmf"] = ProviderMeta{
		ID:       "mmf",
		Name:     "MiMo Free (mmf)",
		Alias:    "mmf",
		Category: CategoryAPIKey,
		AuthType: "",
		NoAuth:   true,
		Hidden:   true,
	}
	// The embedding lane is an apikey provider like the other two self-hosted
	// ones: upstream's row marks it noAuth, which would hide the very
	// credential field its baseUrl override lives in.
	ProviderRegistry["selfhosted-embedding"] = ProviderMeta{
		ID:           "selfhosted-embedding",
		Name:         "Self-Hosted Embeddings",
		Alias:        "selfhosted-embedding",
		Category:     CategoryAPIKey,
		AuthType:     "apikey",
		Color:        "#8B5CF6",
		Icon:         "scatter_plot",
		Priority:     50,
		ServiceKinds: []string{"embedding"},
	}

}

// place that spells the CLI User-Agent.
const ClaudeCLIVersion = "2.1.280"

// claudeCLISpoofHeaders is upstream CLAUDE_CLI_SPOOF_HEADERS
// (open-sse/providers/shared.js:31-46): the full Claude Code fingerprint,
// reused by every registry entry that must look like the official CLI to get
// past a provider's client-identity gate. AgentRouter is the one such entry in
// this port, and it rejects a request carrying only anthropic-version.
//
// The Stainless OS/arch pair describes the client the fingerprint claims to be,
// so it is derived from the host the way shared.js derives it from
// process.platform/process.arch — a hardcoded pair would be one more thing to
// drift. Upstream's CLAUDE_CLI_VERSION (2.1.280) is the same value the Go
// Claude CLI fingerprint already carries.
var claudeCLISpoofHeaders = map[string]string{
	"anthropic-version": "2023-06-01",
	"Anthropic-Beta": "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14," +
		"context-management-2025-06-27,prompt-caching-scope-2026-01-05,advanced-tool-use-2025-11-20," +
		"effort-2025-11-24,structured-outputs-2025-12-15,fast-mode-2026-02-01," +
		"redact-thinking-2026-02-12,token-efficient-tools-2026-03-28",
	"Anthropic-Dangerous-Direct-Browser-Access": "true",
	"User-Agent":                  "claude-cli/" + ClaudeCLIVersion + " (external, sdk-cli)",
	"X-App":                       "cli",
	"X-Stainless-Helper-Method":   "stream",
	"X-Stainless-Retry-Count":     "0",
	"X-Stainless-Runtime-Version": "v24.14.0",
	"X-Stainless-Package-Version": "0.80.0",
	"X-Stainless-Runtime":         "node",
	"X-Stainless-Lang":            "js",
	"X-Stainless-Arch":            stainlessArch(),
	"X-Stainless-Os":              stainlessOs(),
	"X-Stainless-Timeout":         "600",
}

// stainlessArch mirrors shared.js mapStainlessArch.
func stainlessArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x64"
	case "arm64":
		return "arm64"
	case "386":
		return "x86"
	default:
		return "other::" + runtime.GOARCH
	}
}

// stainlessOs mirrors shared.js mapStainlessOs.
func stainlessOs() string {
	switch runtime.GOOS {
	case "darwin":
		return "MacOS"
	case "windows":
		return "Windows"
	case "linux":
		return "Linux"
	case "freebsd":
		return "FreeBSD"
	default:
		return "Other::" + runtime.GOOS
	}
}
