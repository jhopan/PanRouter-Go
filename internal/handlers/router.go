package handlers

import (
	json "encoding/json/v2"
	"github.com/go-chi/chi/v5"
	"net/http"
	"net/http/pprof"
	"os"
	"panrouter/proxy/internal/clientconfig"
	"panrouter/proxy/internal/constants"
	"panrouter/proxy/internal/db"
	"panrouter/proxy/internal/guardrails"
	"panrouter/proxy/internal/handlers/batches"
	"panrouter/proxy/internal/handlers/chat"
	"panrouter/proxy/internal/handlers/dashboard"
	"panrouter/proxy/internal/handlers/media"
	"panrouter/proxy/internal/handlers/oauth"
	"panrouter/proxy/internal/handlers/shared"
	"panrouter/proxy/internal/handlers/sso"
	"panrouter/proxy/internal/handlerutil"
	"panrouter/proxy/internal/middleware"
	"panrouter/proxy/internal/observ"
	"panrouter/proxy/web"
	"strings"
	"time"
)

// Re-export TokenSaverConfig for root compatibility
type TokenSaverConfig = shared.TokenSaverConfig

// NewTokenSaverConfig re-exports shared.NewTokenSaverConfig.
func NewTokenSaverConfig(rtk, caveman, ponytail bool) *TokenSaverConfig {
	return shared.NewTokenSaverConfig(rtk, caveman, ponytail)
}

// routeRegistrar is the slice of chi's routing surface these mounts use. It is
// named so the alias mounter below can wrap a router without a second copy of
// the interface in every signature.
type routeRegistrar interface {
	Get(pattern string, handlerFn http.HandlerFunc)
	Post(pattern string, handlerFn http.HandlerFunc)
	Put(pattern string, handlerFn http.HandlerFunc)
	Patch(pattern string, handlerFn http.HandlerFunc)
	Delete(pattern string, handlerFn http.HandlerFunc)
	HandleFunc(pattern string, handlerFn http.HandlerFunc)
}

// apiV1Prefix is the directory upstream's engine routes live in
// (src/app/api/v1/**). next.config.mjs rewrites `/v1/:path*` onto it, so a
// client written against the Node tree calls either spelling for the same
// endpoint, and this port has to answer both.
const apiV1Prefix = "/api/v1"

// apiV1Alias spells one engine pattern under the dashboard-visible prefix.
//
// A pattern that already names its own version segment ("/v1beta/models", the
// Gemini SDK's spelling) is absolute: its alias is "/api" + the pattern, not
// "/api/v1" + the pattern, because "/api/v1/v1beta/models" is a path neither
// upstream nor any client asks for. Everything else lives under /api/v1.
func apiV1Alias(pattern string) string {
	if strings.HasPrefix(pattern, "/v1") {
		return "/api" + pattern
	}
	return apiV1Prefix + pattern
}

// apiV1Mounter registers every route it is handed twice: at the pattern as
// written — which middleware.RequestLogger serves as /v1/<pattern>, because it
// strips a leading /v1/ before chi matches — and under /api/v1, which that
// strip leaves alone. Both registrations receive the same http.HandlerFunc
// value, so the two prefixes cannot drift onto different handlers, and a route
// mounted through the mounter cannot be missing one of the spellings.
type apiV1Mounter struct{ r routeRegistrar }

func (m apiV1Mounter) Get(pattern string, h http.HandlerFunc) {
	m.r.Get(pattern, h)
	m.r.Get(apiV1Alias(pattern), h)
}

func (m apiV1Mounter) Post(pattern string, h http.HandlerFunc) {
	m.r.Post(pattern, h)
	m.r.Post(apiV1Alias(pattern), h)
}

func (m apiV1Mounter) Put(pattern string, h http.HandlerFunc) {
	m.r.Put(pattern, h)
	m.r.Put(apiV1Alias(pattern), h)
}

func (m apiV1Mounter) Patch(pattern string, h http.HandlerFunc) {
	m.r.Patch(pattern, h)
	m.r.Patch(apiV1Alias(pattern), h)
}

func (m apiV1Mounter) Delete(pattern string, h http.HandlerFunc) {
	m.r.Delete(pattern, h)
	m.r.Delete(apiV1Alias(pattern), h)
}

func (m apiV1Mounter) HandleFunc(pattern string, h http.HandlerFunc) {
	m.r.HandleFunc(pattern, h)
	m.r.HandleFunc(apiV1Alias(pattern), h)
}

// SetupRoutes mounts all domain handlers on the provided router. It returns the
// engine's chat handler, which owns the semantic cache the engine actually
// reads and writes. The caller must pass that same handler to
// SetupDashboardRoutes: a second NewChatHandler builds a second
// PersistentStore over the same SQLite file with its own in-memory LRU, so the
// dashboard would report zero entries while the engine served hits, and
// clearing entries from the dashboard would leave the engine still serving them.
func SetupRoutes(r routeRegistrar, repo *db.Repo, ts *TokenSaverConfig) *chat.ChatHandler {
	chatH := chat.NewChatHandler(repo, ts)
	mediaH := media.NewMediaHandler(repo, ts, chatH)
	oauthH := oauth.NewOAuthHandler(repo)

	dashH := dashboard.NewDashboardHandler(repo, ts)
	if chatH != nil {
		dashH.SemanticCache = chatH.SemanticCache
		// The resilience layer is owned by the data plane, because that is the
		// side that consults it on every request. The dashboard holds the same
		// instance so its panel reports the layer a request was judged
		// against, not a second one that can disagree with it.
		dashH.Resilience = chatH.Resilience
	}
	// Engine data plane. One registration per route, mounted through
	// apiV1Mounter: the bare pattern (the spelling RequestLogger serves as
	// /v1/<pattern>) and /api/v1/<pattern>, upstream's own prefix.
	mountEngineRoutes(apiV1Mounter{r: r}, chatH, mediaH)

	// Dashboard-side mirrors of engine routes, and the media voice catalogue.
	// Upstream publishes these under /api/** (dashboardGuard
	// PROTECTED_API_PATHS), not /api/v1/**, so they stay single-prefix.
	r.Get("/api/changelog", chatH.HandleChangelog)
	r.Get("/api/models", chatH.HandleModels)
	r.Get("/api/models/*", chatH.HandleModelLookup)
	r.Get("/api/models/catalog-sync", chatH.HandleCatalogSyncStatus)
	r.Post("/api/models/catalog-sync", chatH.HandleCatalogSyncTrigger)
	r.Get("/api/media-providers/tts/voices", mediaH.HandleAudioVoices)
	r.Get("/api/media-providers/tts/inworld/voices", mediaH.HandleAudioVoices)
	r.Get("/api/media-providers/tts/minimax/voices", mediaH.HandleAudioVoices)
	r.Get("/api/media-providers/tts/deepgram/voices", mediaH.HandleAudioVoices)
	r.Get("/api/media-providers/tts/elevenlabs/voices", mediaH.HandleAudioVoices)

	// The /v1 spellings this port published before apiV1Mounter existed.
	// RequestLogger already strips a leading /v1/ before chi matches, so the
	// bare twins above serve these requests; the registrations are kept so a
	// router without the strip still answers them.
	r.Get("/v1/models", chatH.HandleModels)
	r.Get("/v1/models/*", chatH.HandleModelLookup)
	r.Post("/v1/audio/music", mediaH.HandleAudioMusic)
	r.Post("/v1/audio/sound-generation", mediaH.HandleAudioSoundGeneration)

	// The relay-deploy endpoints live in the dashboard group below, mounted
	// under /api/proxy-pools/*-deploy behind RequireDashboardAuth (upstream
	// parity). They were registered here under RequireApiKey, so the dashboard
	// SPA — which authenticates with the session cookie and sends no engine
	// key — got 401 "Authentication required" from every Deploy button.

	// CLI Tools Status Domain (dashboard batch status for installed CLI tools)
	// The /api/cli-tools/all-statuses alias is registered in SetupServerRouter
	// under RequireDashboardAuth instead — it is a dashboard read. Registering it
	// here too would win the match under RequireApiKey and keep it unreachable
	// from the session cookie the SPA actually sends.
	r.Get("/cli-tools/all-statuses", media.NewCLIToolsHandler().HandleAllStatuses)

	// Headroom Management Domain (token-compression proxy lifecycle + dashboard proxy)
	headroomH := media.NewHeadroomHandler(repo)
	r.Post("/headroom/start", headroomH.HandleHeadroomStart)
	r.Post("/headroom/stop", headroomH.HandleHeadroomStop)
	r.Post("/headroom/restart", headroomH.HandleHeadroomRestart)
	r.Get("/headroom/status", headroomH.HandleHeadroomStatus)
	r.Get("/headroom/extras", headroomH.HandleHeadroomExtras)
	r.Post("/headroom/extras", headroomH.HandleHeadroomExtras)
	r.Delete("/headroom/extras", headroomH.HandleHeadroomExtras)
	r.HandleFunc("/headroom/proxy", headroomH.HandleHeadroomProxy)
	r.HandleFunc("/headroom/proxy/*", headroomH.HandleHeadroomProxy)

	r.Post("/api/headroom/start", headroomH.HandleHeadroomStart)
	r.Post("/api/headroom/stop", headroomH.HandleHeadroomStop)
	r.Post("/api/headroom/restart", headroomH.HandleHeadroomRestart)
	r.Get("/api/headroom/status", headroomH.HandleHeadroomStatus)
	r.Get("/api/headroom/extras", headroomH.HandleHeadroomExtras)
	r.Post("/api/headroom/extras", headroomH.HandleHeadroomExtras)
	r.Delete("/api/headroom/extras", headroomH.HandleHeadroomExtras)
	r.HandleFunc("/api/headroom/proxy", headroomH.HandleHeadroomProxy)
	r.HandleFunc("/api/headroom/proxy/*", headroomH.HandleHeadroomProxy)
	// OAuth & Import Tokens Domain
	mountOAuthRoutes(r, oauthH)

	// Usage Real-time SSE Stream & Stats Domain (dashboard topology animation + recent requests)
	r.Get("/usage/stream", HandleUsageStream(repo))
	r.Get("/api/usage/stream", HandleUsageStream(repo))
	r.Get("/usage/stats", HandleUsageStats(repo))
	r.Get("/api/usage/stats", HandleUsageStats(repo))
	r.Get("/usage/chart", HandleUsageChart(repo))
	r.Get("/api/usage/chart", HandleUsageChart(repo))
	r.Get("/api/usage/dashboard", HandleUsageDashboard(repo))
	r.Get("/api/usage/request-details", HandleRequestDetails(repo))
	r.Get("/api/usage/export", HandleUsageExport(repo))
	r.Get("/api/usage/request-details/{id}", HandleRequestDetail(repo))
	// The older aggregate/log aliases upstream still serves
	// (src/app/api/usage/{history,logs,request-logs}): history is the stats
	// aggregate with its default window, the log pair is one feed.
	r.Get("/api/usage/history", HandleUsageHistory(repo))
	r.Get("/api/usage/logs", HandleUsageLogs(repo))
	r.Get("/api/usage/request-logs", HandleUsageLogs(repo))
	r.Get("/api/usage/providers", dashH.HandleGetUsageProviders)
	r.Get("/api/usage/{connectionId}", dashH.HandleGetConnectionUsage)
	r.Get("/api/usage/{connectionId}/codex-reset-credits", dashH.HandleListCodexResetCredits)
	// "Check & release": read the provider's live quota and lift the block
	// when the provider says the account is usable again.
	r.Post("/api/usage/{connectionId}/check-release", dashH.HandleCheckRelease)
	r.Post("/api/usage/{connectionId}/codex-reset-credits", dashH.HandleConsumeCodexResetCredit)
	// Claude reset credits (Anthropic program cedar_ember): the list the
	// dashboard offers, then the spend. Upstream's resource name is
	// /claude-reset (src/app/api/usage/[connectionId]/claude-reset/route.js);
	// the list rides the same path because the chooser reads it before spending.
	r.Get("/api/usage/{connectionId}/claude-reset", dashH.HandleListClaudeResetCredits)
	r.Post("/api/usage/{connectionId}/claude-reset", dashH.HandleConsumeClaudeResetCredit)
	// GLM Reset Cards (Z.ai international)
	r.Get("/api/usage/{connectionId}/glm-reset-cards", dashH.HandleGetGLMResetCards)
	r.Post("/api/usage/{connectionId}/glm-reset-cards", dashH.HandleRedeemGLMResetCard)

	// Debug Tracing Domain (p50/p95 latency per provider+model)
	r.Get("/debug/traces", HandleDebugTraces)
	return chatH
}

// mountEngineRoutes registers the model-facing data plane — the routes
// upstream serves from src/app/api/v1/**. SetupRoutes hands it an apiV1Mounter,
// so each pattern here answers at both spellings: the bare one (which
// middleware.RequestLogger serves as /v1/<pattern>) and /api/v1/<pattern>.
//
// Dashboard-facing mirrors (/api/models, /api/usage, /api/media-providers/*,
// /headroom/*) stay out on purpose: upstream publishes those under /api/**
// behind dashboardGuard, not under /api/v1/**.
func mountEngineRoutes(m routeRegistrar, chatH *chat.ChatHandler, mediaH *media.MediaHandler) {
	// Chat & Models Domain (no version here: the four version GETs are
	// public in SetupServerRouter, upstream PUBLIC_API_PATHS parity).
	m.Get("/changelog", chatH.HandleChangelog)
	m.Get("/models", chatH.HandleModels)
	m.Get("/models/info", chatH.HandleModelsInfo)
	m.Get("/models/{kind}", chatH.HandleModelsByKind)
	m.Get("/models/*", chatH.HandleModelLookup)
	m.Post("/chat/completions", chatH.HandleChatCompletions)
	m.Post("/messages", chatH.HandleMessages)
	m.Post("/messages/count_tokens", chatH.HandleCountTokens)
	// Ollama's chat lane. Upstream keeps it at src/app/api/v1/api/chat, so the
	// alias is /api/v1/api/chat and the bare spelling /api/chat.
	m.Post("/api/chat", chatH.HandleOllamaChat)

	// Gemini's own version segment: the SDK is pointed at the gateway's base URL
	// and appends /v1beta/models itself, so this surface cannot live under the
	// /v1 mount and both spellings are registered explicitly (upstream:
	// src/app/api/v1beta/models plus the /v1/:path* rewrite).
	m.Get("/v1beta/models", chatH.HandleGeminiListModels)
	m.Post("/v1beta/models/*", chatH.HandleGeminiGenerateContent)
	m.Get("/api/v1beta/models", chatH.HandleGeminiListModels)
	m.Post("/api/v1beta/models/*", chatH.HandleGeminiGenerateContent)

	// Media, Audio, Video & Web Tools Domain
	m.Post("/embeddings", mediaH.HandleEmbeddings)
	m.Post("/rerank", mediaH.HandleRerank)
	// /v1/responses needs model resolution, combos, account fallback and usage
	// logging, none of which the media passthrough has, so it lives with the
	// chat handlers — the same place /v1/messages does.
	m.Post("/responses", chatH.HandleResponses)
	m.Post("/responses/compact", chatH.HandleResponsesCompact)
	m.Post("/images/generations", mediaH.HandleImages)
	m.Post("/images/edits", mediaH.HandleImageEdits)
	m.Post("/audio/speech", mediaH.HandleAudioSpeech)
	m.Post("/audio/music", mediaH.HandleAudioMusic)
	m.Get("/audio/voices", mediaH.HandleAudioVoices)
	m.Post("/audio/transcriptions", mediaH.HandleAudioTranscriptions)
	m.Post("/audio/sound-generation", mediaH.HandleAudioSoundGeneration)
	m.Post("/videos/generations", mediaH.HandleVideoGenerations)
	m.Post("/videos/edits", mediaH.HandleVideoEdits)
	m.Post("/videos/extensions", mediaH.HandleVideoExtensions)
	m.Get("/videos/{id}", mediaH.HandleVideoGet)
	m.Post("/search", mediaH.HandleSearch)
	m.Post("/scrape", mediaH.HandleScrape)
	m.Post("/web/fetch", mediaH.HandleWebFetch)
	m.Post("/systemone", mediaH.HandleSystemone)
}

// SetupDashboardRoutes mounts the dashboard REST API. It is wrapped in
// RequireDashboardAuth by the server router, which lets a cookie-authenticated
// browser session, a valid API key or the local CLI token through when login is
// enabled (upstream dashboardGuard).
func SetupDashboardRoutes(r chi.Router, repo *db.Repo, chatH *chat.ChatHandler) {
	var ts *TokenSaverConfig
	if chatH != nil {
		ts = chatH.TokenSaver
	}
	dashH := dashboard.NewDashboardHandler(repo, ts)
	if chatH != nil {
		dashH.SemanticCache = chatH.SemanticCache
		// The engine's handler holds the limiter the data plane meters provider
		// windows with, so the dashboard reports the same numbers the gate
		// enforces instead of its own private view of them.
		dashH.Limiter = chatH.RateLimiter
		// Same instance as the resilience panel reads: the data plane owns the
		// layer it consults on every request.
		dashH.Resilience = chatH.Resilience
	}
	ssoH := sso.NewHandler(repo)

	// Password recovery (upstream src/app/api/auth/reset-password/route.js).
	// Upstream answers it behind dashboardGuard's LOCAL_ONLY_PATHS rule — the
	// handler repeats the loopback/CSRF gate — and it is not in PUBLIC_API_PATHS,
	// so it mounts in the dashboard group like its neighbours.
	r.Post("/api/auth/reset-password", dashH.HandleResetPassword)

	// Pricing table the pricing card reads (upstream src/app/api/pricing GET).
	// Same dashboard auth group as its neighbours: upstream lists /api/pricing
	// in PROTECTED_API_PATHS. PATCH and DELETE are not registered — nothing in
	// this gateway stores a user rate override, so there is no table for them to
	// write and answering them would mean accepting a write and discarding it.
	r.Get("/api/pricing", dashH.HandleGetPricing)

	// MCP stdio bridge (upstream src/app/api/mcp/[plugin]/{sse,message}). The
	// handlers repeat upstream's local-only rule: the dashboard gate admits a
	// LAN caller when login is off, but these routes spawn a child process on
	// the operator's machine, so a peer that is not on this host is refused.
	r.Get("/api/mcp/{plugin}/sse", dashH.HandleMCPSSE)
	r.Post("/api/mcp/{plugin}/message", dashH.HandleMCPMessage)

	// Cowork's two network-backed CLI-tools routes. Upstream lists /api/cli-tools
	// in PROTECTED_API_PATHS, so they sit here in the dashboard auth group beside
	// cowork-settings rather than in the API-key table.
	r.Get("/api/cli-tools/cowork-mcp-registry", dashH.HandleCoworkMCPRegistry)
	r.Post("/api/cli-tools/cowork-mcp-tools", dashH.HandleCoworkMCPTools)

	r.Get("/api/connections", dashH.HandleGetConnections)
	r.Get("/api/providers", dashH.HandleGetProvidersClient)
	r.Get("/api/providers/client", dashH.HandleGetProvidersClient)
	r.Post("/api/connections", dashH.HandleCreateConnection)
	r.Post("/api/providers/validate", dashH.HandleValidateProvider)
	// Batch connection test: the dashboard's "test all" buttons select a slice
	// (provider / auth group / compatible) of the connection table and expect
	// one per-connection verdict each. Registered beside the single-row test so
	// the two cannot diverge.
	r.Post("/api/providers/test-batch", dashH.HandleTestProvidersBatch)
	// Kilo's free-model list: the registry lane's modelsFetcher feed
	// (upstream src/app/api/providers/kilo/free-models/route.js), cached for an
	// hour like upstream.
	r.Get("/api/providers/kilo/free-models", dashH.HandleGetKiloFreeModels)
	// The provider registry the dashboard renders its catalog from. Registered here as
	// well as in dashboard.RegisterRoutes, because that table is mounted by tests
	// only: a route added there alone is reachable in a unit test and 404s on the
	// real server.
	r.Get("/api/providers/catalog", dashH.HandleGetProviderCatalog)
	r.Post("/api/providers/gemini-web/browser-login", dashH.HandleGeminiWebBrowserLogin)
	r.Post("/api/providers/gemini-web/import-chrome", dashH.HandleGeminiWebImportChrome)
	r.Post("/api/providers/deepseek-web/browser-login", dashH.HandleDeepSeekWebBrowserLogin)
	r.Post("/api/providers/deepseek-web/import-chrome", dashH.HandleDeepSeekWebImportChrome)
	r.Post("/api/connections/{id}/renew-deepseek", dashH.HandleDeepSeekWebRenew)
	r.Put("/api/connections/{id}", dashH.HandleUpdateConnection)
	r.Put("/api/providers/{id}", dashH.HandleUpdateConnection)
	r.Delete("/api/connections/{id}", dashH.HandleDeleteConnection)
	r.Delete("/api/providers/{id}", dashH.HandleDeleteConnection)
	r.Post("/api/connections/{id}/test", dashH.HandleTestConnection)
	r.Post("/api/connections/{id}/reorder", dashH.HandleReorderConnection)
	r.Post("/api/providers/{id}/reorder", dashH.HandleReorderConnection)
	r.Post("/api/providers/{id}/test", dashH.HandleTestConnection)
	r.Get("/api/providers/suggested-models", HandleSuggestedModels)
	// Usage & Quota Endpoints (dashboard quota tracker, usage stats, and topology stream)
	r.Get("/api/usage/stream", HandleUsageStream(repo))
	r.Get("/usage/stream", HandleUsageStream(repo))
	r.Get("/api/usage/stats", HandleUsageStats(repo))
	r.Get("/usage/stats", HandleUsageStats(repo))
	r.Get("/api/usage/chart", HandleUsageChart(repo))
	r.Get("/api/usage/dashboard", HandleUsageDashboard(repo))
	r.Get("/api/usage/request-details", HandleRequestDetails(repo))
	r.Get("/api/usage/export", HandleUsageExport(repo))
	r.Get("/api/usage/request-details/{id}", HandleRequestDetail(repo))
	// The older aggregate/log aliases upstream still serves
	// (src/app/api/usage/{history,logs,request-logs}): history is the stats
	// aggregate with its default window, the log pair is one feed.
	r.Get("/api/usage/history", HandleUsageHistory(repo))
	r.Get("/api/usage/logs", HandleUsageLogs(repo))
	r.Get("/api/usage/request-logs", HandleUsageLogs(repo))
	r.Get("/api/usage/providers", dashH.HandleGetUsageProviders)
	r.Get("/api/usage/{connectionId}", dashH.HandleGetConnectionUsage)

	r.Get("/api/usage/{connectionId}/codex-reset-credits", dashH.HandleListCodexResetCredits)
	// "Check & release": read the provider's live quota and lift the block
	// when the provider says the account is usable again.
	r.Post("/api/usage/{connectionId}/check-release", dashH.HandleCheckRelease)
	r.Post("/api/usage/{connectionId}/codex-reset-credits", dashH.HandleConsumeCodexResetCredit)
	// Claude reset credits (Anthropic program cedar_ember): the list the
	// dashboard offers, then the spend. Upstream's resource name is
	// /claude-reset (src/app/api/usage/[connectionId]/claude-reset/route.js);
	// the list rides the same path because the chooser reads it before spending.
	r.Get("/api/usage/{connectionId}/claude-reset", dashH.HandleListClaudeResetCredits)
	r.Post("/api/usage/{connectionId}/claude-reset", dashH.HandleConsumeClaudeResetCredit)
	// GLM Reset Cards (Z.ai international)
	r.Get("/api/usage/{connectionId}/glm-reset-cards", dashH.HandleGetGLMResetCards)
	r.Post("/api/usage/{connectionId}/glm-reset-cards", dashH.HandleRedeemGLMResetCard)

	// Cache Analytics & Management
	r.Get("/api/cache", dashH.HandleGetCache)
	r.Delete("/api/cache", dashH.HandleDeleteCache)
	r.Get("/api/cache/entries", dashH.HandleGetCacheEntries)
	r.Delete("/api/cache/entries", dashH.HandleDeleteCacheEntry)

	// Compression Analytics
	r.Get("/api/analytics/compression", dashH.HandleGetCompressionAnalytics)

	// Jailbreak: agent prompt presets + per-model binding. Registered here and in
	// dashboard.RegisterRoutes (a tests-only table); a route added there alone
	// 404s on the real server — the same trap as the per-node limits pair.
	r.Get("/api/jailbreak", dashH.HandleGetJailbreak)
	r.Put("/api/jailbreak", dashH.HandlePutJailbreak)
	r.Get("/api/jailbreak/presets", dashH.HandleListJailbreakPresets)
	r.Put("/api/jailbreak/presets", dashH.HandlePutJailbreakPreset)
	r.Delete("/api/jailbreak/presets/{id}", dashH.HandleDeleteJailbreakPreset)
	r.Post("/api/jailbreak/presets/{id}/duplicate", dashH.HandleDuplicateJailbreakPreset)
	r.Get("/api/jailbreak/models", dashH.HandleListJailbreakModels)
	r.Get("/api/jailbreak/models/catalog", dashH.HandleJailbreakModelCatalog)
	r.Put("/api/jailbreak/models", dashH.HandleSetJailbreakModel)
	r.Delete("/api/jailbreak/models", dashH.HandleDeleteJailbreakModel)
	r.Get("/api/jailbreak/excludes", dashH.HandleGetJailbreakExcludes)
	r.Put("/api/jailbreak/excludes", dashH.HandlePutJailbreakExcludes)
	r.Post("/api/jailbreak/import", dashH.HandleImportJailbreak)
	r.Post("/api/jailbreak/resolve", dashH.HandleExplainJailbreak)
	r.Get("/api/jailbreak/usage", dashH.HandleJailbreakUsage)
	r.Post("/api/jailbreak/preview", dashH.HandlePreviewJailbreak)
	r.Put("/api/jailbreak/presets/order", dashH.HandleReorderJailbreakPresets)
	r.Post("/api/jailbreak/presets/upload", dashH.HandleUploadJailbreakPreset)

	r.Get("/api/provider-nodes", dashH.HandleGetProviderNodes)
	r.Post("/api/provider-nodes", dashH.HandleCreateProviderNode)
	r.Put("/api/provider-nodes/{id}", dashH.HandleUpdateProviderNode)
	r.Delete("/api/provider-nodes/{id}", dashH.HandleDeleteProviderNode)
	r.Post("/api/provider-nodes/validate", dashH.HandleValidateProviderNode)
	// Per-node capacity limits (docs/provider-limits-plan.md). These live here as
	// well as in dashboard.RegisterRoutes because that table is mounted by tests
	// only: a route added there alone is reachable in a unit test and 404s on the
	// real server, which is how this pair was nearly shipped.
	r.Put("/api/provider-nodes/{id}/limits", dashH.HandleUpdateProviderNodeLimits)
	r.Post("/api/provider-nodes/{id}/quota/reset", dashH.HandleResetProviderNodeQuota)
	r.Get("/api/providers/{id}/models", dashH.HandleGetConnectionModels)
	// Per-model probe: the connection id resolves the provider and its catalogue,
	// and each model is pinged through the endpoint its kind serves.
	r.Post("/api/providers/{id}/test-models", dashH.HandleTestProviderModels)
	r.Get("/api/providers/{id}/export-keys", dashH.HandleExportProviderKeys)
	// Bulk removal of a node's imported models; the single-row form stays at
	// DELETE /api/models/custom/{key}.
	r.Delete("/api/providers/{id}/models", dashH.HandleDeleteProviderModels)
	r.Get("/api/providers/{id}/overrides", dashH.HandleGetProviderOverrides)
	r.Put("/api/providers/{id}/overrides", dashH.HandleSaveProviderOverrides)

	r.Get("/api/combos", dashH.HandleGetCombos)
	r.Post("/api/combos", dashH.HandleCreateCombo)
	r.Post("/api/combos/auto-free", dashH.HandleAutoFreeCombo)
	r.Post("/api/combos/auto-family", dashH.HandleAutoFamilyCombos)
	r.Get("/api/combos/presets", dashH.HandleGetComboPresets)
	r.Post("/api/combos/presets", dashH.HandleCreateComboPresets)
	r.Put("/api/combos/{id}", dashH.HandleUpdateCombo)
	// Smart select combo: dry-run a rule list, and apply every combo's rules on
	// demand. These live here as well as in dashboard.RegisterRoutes for the
	// same reason the node-limit pair does — that table is mounted by tests
	// only, so a route registered there alone answers 200 in a unit test and
	// "404 page not found" on the real server, which is exactly how the
	// smart-rules editor shipped.
	r.Post("/api/combos/reconcile", dashH.HandleReconcileSmartCombos)
	r.Post("/api/combos/{id}/smart/preview", dashH.HandleComboSmartPreview)
	r.Post("/api/combos/{id}/smart/load", dashH.HandleComboSmartLoad)
	r.Get("/api/models/meta", dashH.HandleGetModelMeta)
	// Which models are parked right now (live modelLock_* rows plus accounts
	// whose own test said "unavailable"), and the operator gesture that lifts
	// one model's cooldown.
	r.Get("/api/models/availability", dashH.HandleGetModelAvailability)
	r.Post("/api/models/availability", dashH.HandleClearModelCooldown)
	r.Delete("/api/combos/{id}", dashH.HandleDeleteCombo)

	r.Get("/api/proxy-pools", dashH.HandleGetProxyPools)
	r.Post("/api/proxy-pools", dashH.HandleCreateProxyPool)
	r.Put("/api/proxy-pools/{id}", dashH.HandleUpdateProxyPool)
	r.Delete("/api/proxy-pools/{id}", dashH.HandleDeleteProxyPool)
	r.Post("/api/proxy-pools/{id}/test", dashH.HandleTestProxyPool)

	// Relay deploy endpoints. Dashboard reads/writes, not engine traffic: the
	// SPA reaches them with its session cookie, exactly like the pool CRUD
	// above. chi prefers the static segment over {id}, so these four cannot
	// be shadowed by the parameterised routes registered first.
	relayH := media.NewMediaHandler(repo, nil, nil)
	r.Post("/api/proxy-pools/vercel-deploy", relayH.HandleVercelDeploy)
	r.Post("/api/proxy-pools/deno-deploy", relayH.HandleDenoDeploy)
	r.Post("/api/proxy-pools/cloudflare-deploy", relayH.HandleCloudflareDeploy)
	r.Post("/api/proxy-pools/netlify-deploy", relayH.HandleNetlifyDeploy)
	// Pre-flight for the Netlify CLI: the dashboard offers an install instead of
	// letting a CLI-driven deploy fail a minute later.
	r.Get("/api/proxy-pools/netlify-cli-status", dashH.HandleGetNetlifyCLIStatus)

	r.Get("/api/keys", dashH.HandleGetApiKeys)
	r.Post("/api/keys", dashH.HandleCreateApiKey)
	r.Delete("/api/keys/{id}", dashH.HandleDeleteApiKey)
	r.Put("/api/keys/{id}/toggle", dashH.HandleToggleApiKey)
	r.Post("/api/keys/{id}/rotate", dashH.HandleRotateApiKey)
	r.Post("/api/keys/{id}/reset-quota", dashH.HandleResetApiKeyQuota)
	r.Get("/api/keys/{id}/models", dashH.HandleGetApiKeyModels)
	r.Put("/api/keys/{id}/models", dashH.HandleSetApiKeyModels)
	r.Put("/api/keys/{id}", dashH.HandleUpdateApiKey)
	r.Get("/api/plans", dashH.HandleListPlans)
	r.Post("/api/plans", dashH.HandleCreatePlan)
	r.Get("/api/plans/{id}", dashH.HandleGetPlan)
	r.Put("/api/plans/{id}", dashH.HandleUpdatePlan)
	r.Delete("/api/plans/{id}", dashH.HandleDeletePlan)
	r.Get("/api/plans/{id}/keys", dashH.HandlePlanKeys)
	r.Get("/api/keys/{id}/budget", dashH.HandleGetApiKeyBudget)
	r.Get("/api/metrics", observ.Handler().ServeHTTP)
	r.Get("/api/vault/status", dashH.HandleGetVaultStatus)
	r.Get("/api/guardrails/policies", dashH.HandleGetGuardrailPolicies)
	r.Post("/api/guardrails/policies", dashH.HandleCreateGuardrailPolicy)
	r.Put("/api/guardrails/policies/{id}", dashH.HandleUpdateGuardrailPolicy)
	r.Delete("/api/guardrails/policies/{id}", dashH.HandleDeleteGuardrailPolicy)
	r.Get("/api/guardrails/logs", dashH.HandleListGuardrailLogs)
	r.Post("/api/vault/rotate", dashH.HandleRotateVault)
	r.Get("/api/models/custom", dashH.HandleGetCustomModels)
	r.Get("/api/models/caps", dashH.HandleGetModelCaps)
	r.Post("/api/models/custom", dashH.HandleSaveCustomModel)
	r.Delete("/api/models/custom/{key}", dashH.HandleDeleteCustomModel)
	r.Get("/api/models/disabled", dashH.HandleGetDisabledModels)
	// mediaH needs the chat handler every chat-lane probe is forwarded through,
	// so it is built with chatH — not the nil-chat relay handler below, whose
	// probes would panic instead of reaching the provider.
	mediaH := media.NewMediaHandler(repo, nil, chatH)
	r.Post("/api/models/test", mediaH.HandleTestModel)
	// POST /api/providers/{id}/test-models reports per-model verdicts from that
	// same probe, so the two test buttons cannot disagree about a model.
	dashH.SetModelProbe(mediaH.ProbeModel)
	// Ollama client model discovery (upstream src/app/api/tags). Registered here
	// rather than in dashboard.RegisterRoutes because the handler belongs to the
	// media package, and because this is the table the binary actually mounts.
	// Upstream routes /api/tags through PROTECTED_API_PATHS, so it sits in this
	// dashboard auth group. The preflight is answered explicitly: upstream's
	// route exports OPTIONS with the same CORS headers for browser clients.
	r.Get("/api/tags", mediaH.HandleOllamaTags)
	r.Options("/api/tags", func(w http.ResponseWriter, r *http.Request) {
		handlerutil.WriteCORSHeaders(w)
		// Upstream answers the preflight with an empty 200 (`new Response(null,
		// { headers: CORS_HEADERS })`), not the 204 helper's default.
		w.WriteHeader(http.StatusOK)
	})
	// Capability detection (vision/reasoning) for a model id: heuristic first,
	// then live probes through that same chat lane.
	r.Post("/api/models/detect-caps", mediaH.HandleDetectCaps)
	r.Post("/api/combos/{id}/test-models", mediaH.HandleComboTestModels)
	r.Get("/api/media-providers/tts/voices", mediaH.HandleAudioVoices)
	r.Get("/api/media-providers/tts/inworld/voices", mediaH.HandleAudioVoices)
	r.Get("/api/media-providers/tts/minimax/voices", mediaH.HandleAudioVoices)
	r.Get("/api/media-providers/tts/deepgram/voices", mediaH.HandleAudioVoices)
	r.Get("/api/media-providers/tts/elevenlabs/voices", mediaH.HandleAudioVoices)
	r.Put("/api/models/disabled/{provider}", dashH.HandleSaveDisabledModels)
	r.Post("/api/models/auto-disable", dashH.HandleAutoDisableModels)
	r.Get("/api/models/health", dashH.HandleGetModelHealth)
	r.Put("/api/models/health", dashH.HandleSaveModelHealth)
	r.Get("/api/models/alias", dashH.HandleGetModelAliases)
	r.Get("/api/models/deprecations", dashH.HandleGetModelDeprecations)
	r.Post("/api/models/sync", dashH.HandleSyncProviderModels)
	r.Put("/api/models/alias", dashH.HandleSetModelAlias)
	r.Delete("/api/models/alias", dashH.HandleDeleteModelAlias)

	r.Get("/api/settings", dashH.HandleGetSettings)
	r.Put("/api/settings", dashH.HandleUpdateSettings)
	r.Patch("/api/settings", dashH.HandleUpdateSettings)
	r.Put("/settings", dashH.HandleUpdateSettings)
	r.Patch("/settings", dashH.HandleUpdateSettings)

	// Settings backup/restore + outbound proxy diagnostics (profile page)
	// Automatic database backup: GitHub snapshot config, one-shot run, the
	// mirror download token and its CRUD. The download authenticates with its
	// own credential (x-backup-token), which is why
	// middleware.RequireDashboardAuth lets that one path through.
	r.Get("/api/backup/config", dashH.HandleGetBackupConfig)
	r.Post("/api/backup/config", dashH.HandlePostBackupConfig)
	r.Post("/api/backup/run", dashH.HandleRunBackup)
	r.Get("/api/backup/download", dashH.HandleDownloadBackup)
	r.Get("/api/backup/tokens", dashH.HandleListBackupTokens)
	r.Post("/api/backup/tokens", dashH.HandleCreateBackupToken)
	r.Patch("/api/backup/tokens", dashH.HandleUpdateBackupToken)
	r.Delete("/api/backup/tokens", dashH.HandleDeleteBackupToken)
	r.Get("/api/settings/database", dashH.HandleExportDatabase)
	r.Post("/api/settings/database", dashH.HandleImportDatabase)
	r.Get("/api/settings/database/stats", dashH.HandleGetDatabaseStats)
	r.Post("/api/settings/database/prune", dashH.HandlePruneDatabase)
	r.Post("/api/settings/database/prune-usage", dashH.HandlePruneUsageHistory)
	r.Post("/api/settings/database/vacuum", dashH.HandleVacuumDatabase)
	r.Post("/api/settings/proxy-test", dashH.HandleProxyTest)
	// White-label logo upload: the file lands under DATA_DIR/uploads and
	// appLogoUrl is rewritten to the path this gateway serves it from.
	r.Post("/api/settings/logo", dashH.HandleUploadLogo)

	// Currency rates (profile page): reads live USD rates for the catalog codes
	// and stores the subset the display uses.
	r.Post("/api/settings/currency/refresh", dashH.HandleRefreshCurrencyRates)

	// Token saver testing benches & filter catalog
	r.Get("/api/tokensaver/rtk/filters", dashH.HandleGetRTKFilters)
	r.Post("/api/tokensaver/rtk/test", dashH.HandleTestRTK)
	r.Post("/api/tokensaver/caveman/test", dashH.HandleTestCaveman)

	// Translator playground/editor (upstream src/app/api/translator/{load,save,
	// send,translate}/route.js). It edits the transformer dumps and replays one
	// request through the wire pipeline, so it belongs to the dashboard group:
	// the SPA reaches it with the session cookie, never an engine API key.
	translatorH := NewTranslatorHandler("", chatH)
	r.Get("/api/translator/load", translatorH.HandleLoad)
	r.Post("/api/translator/save", translatorH.HandleSave)
	r.Post("/api/translator/send", translatorH.HandleSend)
	r.Post("/api/translator/translate", translatorH.HandleTranslate)

	// Headroom token-compression proxy management (dashboard parity)
	headroomH := media.NewHeadroomHandler(repo)
	r.Get("/api/headroom/status", headroomH.HandleHeadroomStatus)
	r.Post("/api/headroom/start", headroomH.HandleHeadroomStart)
	r.Post("/api/headroom/stop", headroomH.HandleHeadroomStop)
	r.Post("/api/headroom/restart", headroomH.HandleHeadroomRestart)
	r.Get("/api/headroom/extras", headroomH.HandleHeadroomExtras)
	r.Post("/api/headroom/extras", headroomH.HandleHeadroomExtras)
	r.Delete("/api/headroom/extras", headroomH.HandleHeadroomExtras)
	r.HandleFunc("/api/headroom/proxy", headroomH.HandleHeadroomProxy)
	r.HandleFunc("/api/headroom/proxy/*", headroomH.HandleHeadroomProxy)

	// Tunnel & Tailscale
	r.Get("/api/tunnel/status", dashH.HandleTunnelStatus)
	r.Post("/api/tunnel/enable", dashH.HandleTunnelEnable)
	r.Post("/api/tunnel/disable", dashH.HandleTunnelDisable)
	r.Get("/api/tunnel/tailscale-check", dashH.HandleTailscaleCheck)
	r.Post("/api/tunnel/tailscale-enable", dashH.HandleTailscaleEnable)
	r.Post("/api/tunnel/tailscale-disable", dashH.HandleTailscaleDisable)
	// SSE install stream: progress lines, then the login URL the operator has to
	// visit. Kept beside the enable/disable pair it exists to unblock.
	r.Post("/api/tunnel/tailscale-install", dashH.HandleTailscaleInstall)

	// Single Sign-On settings checks + SP metadata (profile page)
	r.Post("/api/auth/oidc/test", ssoH.HandleOidcTest)
	r.Post("/api/auth/saml/test", ssoH.HandleSamlTest)
	r.Get("/api/auth/saml/metadata", ssoH.HandleSamlMetadata)

	// Mount OAuth routes under dashboard auth so web dashboard can initiate and exchange tokens
	oauthH := oauth.NewOAuthHandler(repo)
	mountOAuthRoutes(r, oauthH)
}

// SetupConsoleLogRoutes mounts operational log APIs behind a full dashboard
// session or local CLI token. Client API keys are intentionally rejected.
func SetupConsoleLogRoutes(r chi.Router, repo *db.Repo) {
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireConsoleLogAuth(repo))
		r.Get("/api/translator/console-logs", HandleConsoleLogsGet)
		r.Delete("/api/translator/console-logs", HandleConsoleLogsDelete)
		r.Get("/api/translator/console-logs/stream", HandleConsoleLogsStream)
		r.Get("/api/translator/console-logs/level", HandleConsoleLogsLevelGet)
		r.Put("/api/translator/console-logs/level", HandleConsoleLogsLevelPut)
	})
}

func mountOAuthRoutes(r routeRegistrar, oauthH *oauth.OAuthHandler) {
	r.Post("/api/oauth/{provider}/import", oauthH.HandleOAuthImport)
	r.Get("/api/oauth/kiro/social-authorize", oauthH.HandleOAuthKiroSocialAuthorize)
	r.Post("/api/oauth/kiro/social-exchange", oauthH.HandleOAuthKiroSocialExchange)
	r.Post("/api/oauth/kiro/import", oauthH.HandleKiroImport)
	r.Post("/api/oauth/kiro/import-cli-proxy", oauthH.HandleKiroImportCliProxy)
	r.Get("/api/oauth/kiro/auto-import", oauthH.HandleKiroAutoImport)
	r.Post("/api/oauth/kiro/api-key", oauthH.HandleKiroAPIKey)
	r.Post("/api/oauth/codex/bulk-import", oauthH.HandleOAuthCodexBulkImport)
	// A ChatGPT access token pasted from chatgpt.com settings: stored as an
	// access_token connection, no refresh flow (upstream import-token route).
	r.Post("/api/oauth/codex/import-token", oauthH.HandleCodexImportToken)
	r.Post("/api/oauth/grok-cli/bulk-import", oauthH.HandleOAuthGrokCliBulkImport)
	r.Post("/api/oauth/freebuff/initiate", oauthH.HandleFreebuffInitiate)
	r.Post("/api/oauth/freebuff/poll", oauthH.HandleFreebuffPoll)
	r.Get("/api/oauth/freebuff/session", oauthH.HandleFreebuffSessionStatus)
	r.Post("/api/oauth/freebuff/session/switch", oauthH.HandleFreebuffSessionSwitch)
	r.Get("/api/oauth/antigravity/authorize", oauthH.HandleAntigravityAuthorize)
	r.Post("/api/oauth/antigravity/exchange", oauthH.HandleAntigravityExchange)
	r.Get("/api/oauth/cline/authorize", oauthH.HandleClineAuthorize)
	r.Post("/api/oauth/cline/exchange", oauthH.HandleClineExchange)
	r.Get("/api/oauth/pkce/authorize", oauthH.HandlePKCEAuthorize)
	r.Post("/api/oauth/pkce/exchange", oauthH.HandlePKCEExchange)
	// Codex's OAuth client only accepts its registered loopback redirect URI,
	// so the callback is served by a dedicated fixed-port listener rather than
	// the dashboard's /callback page. See internal/handlers/oauth/codex_proxy.go.
	r.Get("/api/oauth/codex/start-proxy", oauthH.HandleCodexStartProxy)
	r.Get("/api/oauth/codex/poll-status", oauthH.HandleCodexPollStatus)
	r.Get("/api/oauth/codex/stop-proxy", oauthH.HandleCodexStopProxy)
	r.Get("/api/oauth/authcode/authorize", oauthH.HandleAuthCodeAuthorize)
	r.Post("/api/oauth/authcode/exchange", oauthH.HandleAuthCodeExchange)
	r.Get("/api/oauth/trae/authorize", oauthH.HandleTraeAuthorize)
	r.Post("/api/oauth/trae/exchange", oauthH.HandleTraeExchange)
	r.Get("/api/oauth/windsurf/authorize", oauthH.HandleWindsurfAuthorize)
	r.Post("/api/oauth/windsurf/exchange", oauthH.HandleWindsurfExchange)
	r.Get("/api/oauth/zed/authorize", oauthH.HandleZedAuthorize)
	r.Post("/api/oauth/zed/exchange", oauthH.HandleZedExchange)
	// Upstream serves auto-import as GET; the dashboard also posts to it.
	r.Get("/api/oauth/zed/auto-import", oauthH.HandleZedAutoImport)
	r.Post("/api/oauth/zed/auto-import", oauthH.HandleZedAutoImport)
	r.Post("/api/oauth/zed/import", oauthH.HandleZedImport)
	r.Post("/api/oauth/device/start", oauthH.HandleDeviceStart)
	r.Post("/api/oauth/device/poll", oauthH.HandleDevicePoll)
	r.Post("/api/oauth/cursor/import", oauthH.HandleCursorImport)
	r.Get("/api/oauth/cursor/auto-import", oauthH.HandleCursorAutoImport)
	r.Get("/api/oauth/kimchi/authorize", oauthH.HandleKimchiAuthorize)
	r.Post("/api/oauth/kimchi/exchange", oauthH.HandleKimchiExchange)
	r.Post("/api/oauth/gitlab/pat", oauthH.HandleGitlabPAT)
	r.Post("/api/oauth/iflow/cookie", oauthH.HandleIflowCookie)
	r.Get("/api/oauth/xiaomi-mimo/authorize", oauthH.HandleMimoAuthorize)
	r.Post("/api/oauth/xiaomi-mimo/exchange", oauthH.HandleMimoExchange)
	// Manual import, Desktop discovery, and the account-session login walk.
	// Upstream serves auto-import as GET only; the dashboard also posts to it,
	// the same pair the zed lane exposes.
	r.Post("/api/oauth/xiaomi-mimo/api-key", oauthH.HandleMimoAPIKey)
	r.Get("/api/oauth/xiaomi-mimo/auto-import", oauthH.HandleMimoAutoImport)
	r.Post("/api/oauth/xiaomi-mimo/auto-import", oauthH.HandleMimoAutoImport)
	r.Post("/api/oauth/xiaomi-mimo/login/start", oauthH.HandleMimoLoginStart)
	r.Get("/api/oauth/xiaomi-mimo/login/status", oauthH.HandleMimoLoginStatus)
}

// SetupServerRouter mounts public endpoints (/health, /api/hello) and
// API-key protected routes (all engine + admin routes) on the chi router.
//
// It returns the engine's chat handler: the one SetupRoutes built and the
// dashboard group is mounted on. The auto-ping scheduler sends its pings
// through that same instance, so a ping resolves credentials, refreshes OAuth
// and picks an executor the way real traffic does. Returning it is what keeps
// the gateway from building a second handler for the scheduler — see the
// warning on SetupRoutes about the two PersistentStores.
func SetupServerRouter(r chi.Router, repo *db.Repo, ts *TokenSaverConfig) *chat.ChatHandler {
	// Public (unauthenticated) endpoints. Upstream serves the liveness probe at
	// /api/health (src/app/api/health/route.js) and this gateway answered only
	// /health, so a client probing the upstream path fell through to the SPA
	// and got a 404 page. One handler serves both spellings, so the two cannot
	// drift; the body stays this gateway's {"status":"ok"} rather than
	// upstream's {"ok":true} because the Go dashboard probes it directly.
	healthHandler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
		w.Write([]byte(`{"status":"ok"}`))
	}
	r.Get("/health", healthHandler)
	r.Get("/api/health", healthHandler)
	// Version info is public (upstream PUBLIC_API_PATHS): the Sidebar polls
	// /api/version on every dashboard page including /login, before any
	// session or API key exists.
	versionH := chat.NewChatHandler(repo, ts)
	r.Get("/version", versionH.HandleVersion)
	r.Get("/api/version", versionH.HandleVersion)
	r.Get("/api/version/status", versionH.HandleVersionStatus)
	r.Get("/api/version/check", versionH.HandleCheckUpdate)
	// Embedded Native Dashboard SPA
	webH := web.Handler()
	oauthH := oauth.NewOAuthHandler(repo)
	// Public OAuth landing page: providers redirect browsers here after login.
	// Browsers carry neither the dashboard session nor the engine API key.
	// MiniMax Code lease auth: probe the desktop broker, then create a lease.
	r.Get("/api/oauth/{provider}/lease-status", oauthH.HandleLeaseStatus)
	r.Post("/api/oauth/{provider}/lease-connect", oauthH.HandleLeaseConnect)
	r.Get("/callback", oauthH.HandleCallbackPage)
	r.Get("/", webH.ServeHTTP)
	r.Get("/login", webH.ServeHTTP)
	// Dashboard pages require the login session when requireLogin is on; the
	// guard redirects browsers to /login (upstream dashboardGuard).
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireDashboardPage(repo))
		r.Get("/dashboard", webH.ServeHTTP)
		r.Get("/dashboard/*", webH.ServeHTTP)
	})
	r.Get("/media", webH.ServeHTTP)
	r.Get("/media-providers/*", webH.ServeHTTP)
	r.Get("/media-providers/web", webH.ServeHTTP)
	r.Get("/connections", webH.ServeHTTP)
	// Provider share link: a fragment-only URL the SPA resolves. Registered
	// outside the dashboard group because the receiver has no session yet, and
	// RequestLogger strips a leading /v1/ before the router sees the path, so
	// "/add" is the pattern that matches both spellings.
	r.Get("/add", webH.ServeHTTP)
	r.Get("/v1/add", webH.ServeHTTP)
	r.Get("/combos", webH.ServeHTTP)
	r.Get("/analytics", webH.ServeHTTP)
	r.Get("/terminal", webH.ServeHTTP)
	r.Get("/keys", webH.ServeHTTP)
	r.Get("/settings", webH.ServeHTTP)
	r.Get("/providers", webH.ServeHTTP)
	r.Get("/usage", webH.ServeHTTP)
	r.Get("/quota", webH.ServeHTTP)
	r.HandleFunc("/assets/*", webH.ServeHTTP)
	r.HandleFunc("/providers/*", webH.ServeHTTP)
	r.HandleFunc("/icons/*", webH.ServeHTTP)
	r.HandleFunc("/favicon.ico", webH.ServeHTTP)
	r.HandleFunc("/favicon.svg", webH.ServeHTTP)
	r.HandleFunc("/icons.svg", webH.ServeHTTP)
	r.HandleFunc("/sw.js", webH.ServeHTTP)
	r.HandleFunc("/manifest.webmanifest", webH.ServeHTTP)
	r.HandleFunc("/manifest.json", webH.ServeHTTP)
	r.HandleFunc("/api/hello", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			w.Write([]byte(`{"status":"ok","message":"hello"}`))
		}
	})
	// Dashboard login session. status/login/logout/require-login are public so
	// the login page can load before a cookie exists (upstream PUBLIC_API_PATHS).
	dashH := dashboard.NewDashboardHandler(repo, ts)
	if versionH != nil {
		dashH.SemanticCache = versionH.SemanticCache
		dashH.Resilience = versionH.Resilience
	}
	// Operator-supplied dashboard assets (the white-label logo). Behind the
	// same page gate as /dashboard/*, so an install that requires a login does
	// not serve its branding to an anonymous caller.
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireDashboardPage(repo))
		r.Get("/uploads/*", dashH.HandleServeUploads)
	})
	r.Get("/api/auth/status", dashH.HandleAuthStatus)
	r.Post("/api/auth/login", dashH.HandleAuthLogin)
	r.Post("/api/auth/logout", dashH.HandleAuthLogout)
	// SSO browser flows. Upstream lists /api/auth/oidc and /api/auth/saml in
	// dashboardGuard PUBLIC_API_PATHS: the login page links straight here and
	// the caller has no session yet, so these mount outside the dashboard gate.
	// Each handler refuses on its own terms (unconfigured, untrusted origin,
	// bad state) with the redirect upstream sends.
	ssoFlowH := sso.NewHandler(repo)
	r.Get("/api/auth/oidc/start", ssoFlowH.HandleOIDCStart)
	r.Get("/api/auth/oidc/callback", ssoFlowH.HandleOIDCCallback)
	r.Get("/api/auth/saml/start", ssoFlowH.HandleSAMLStart)
	r.Post("/api/auth/saml/acs", ssoFlowH.HandleSAMLACS)
	r.Get("/api/settings/require-login", dashH.HandleRequireLogin)
	r.Get("/api/tunnel/status", dashH.HandleTunnelStatus)
	// Dashboard boot surface, public upstream (dashboardGuard PUBLIC_API_PATHS:
	// "/api/init", "/api/locale"). /api/init is a constant ping; /api/locale
	// writes the locale cookie and, like upstream, has no GET — its translations
	// are static files under /i18n/literals, not an API response.
	r.Get("/api/init", dashH.HandleGetInit)
	r.Head("/api/init", dashH.HandleGetInit)
	r.Get("/api/locale", dashH.HandleGetLocale)
	r.Post("/api/locale", dashH.HandleSetLocale)

	// Admin-only operations (health reset, shutdown, update) - strictly requires session cookie or CLI token.
	// Standard client API keys are rejected, matching upstream ALWAYS_PROTECTED.
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireAdminAuth())

		// Health reset endpoint — dashboard calls this via headroom proxy
		r.Post("/admin/health/reset", func(w http.ResponseWriter, r *http.Request) {
			provider := r.URL.Query().Get("provider")
			model := r.URL.Query().Get("model")
			if err := repo.ResetProviderHealth(provider, model); err != nil {
				handlerutil.WriteJSONError(w, http.StatusInternalServerError, err.Error())
				return
			}
			w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
			json.MarshalWrite(w, map[string]string{"status": "ok"})
		})

		chatH := chat.NewChatHandler(repo, ts)
		r.Post("/api/version/update", chatH.HandleTriggerUpdate)
		r.Post("/api/version/auto-update", chatH.HandleToggleAutoUpdate)
		r.Post("/api/version/shutdown", HandleShutdown)
		// Upstream's own spelling (src/app/api/shutdown/route.js). The middleware
		// already listed /api/shutdown as always-protected, but no route answered
		// it, so a client using the upstream path got a 404. Same handler, so the
		// two replies cannot diverge.
		r.Post("/api/shutdown", HandleShutdown)

		// Profiling endpoints (pprof) — disabled by default in production for security;
		// enable explicitly via PPROF_ENABLED=true. Gated behind RequireAdminAuth
		// (issue #126) so debug surface is never exposed publicly.
		if strings.EqualFold(strings.TrimSpace(os.Getenv("PPROF_ENABLED")), "true") {
			r.HandleFunc("/debug/pprof/", pprof.Index)
			r.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
			r.HandleFunc("/debug/pprof/profile", pprof.Profile)
			r.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
			r.HandleFunc("/debug/pprof/trace", pprof.Trace)
			r.HandleFunc("/debug/pprof/*", pprof.Index)
		}
	})

	// API-key protected domain routes. The rate limiter sits after
	// RequireApiKey so it reads the per-key RPM/TPM/concurrency limits off the
	// authenticated key. Guardrails sit after both: a policy is scoped by API
	// key, and a caller the limiter already rejected must not be able to make
	// the gateway scan content either.
	// The limiter's window is a startup read: every in-memory sliding window is
	// sized against it, so changing it has to restart the gateway.
	window := time.Duration(repo.GetRateLimitDefaults().WindowSeconds) * time.Second
	rateLimiter := middleware.NewRateLimiter(window)
	// The plan tier (F-3) is resolved from the repository, so a plan change
	// applies to the next request without a restart.
	rateLimiter.SetPlanSource(repo)
	// The per-period budget (F-2) measures its spend from the same ledger the
	// Usage dashboard reads, so the ceiling and the dashboard cannot disagree
	// about what a key has spent.
	rateLimiter.SetBudgetReader(repo)
	// The fallback itself is read per request, so an operator can raise or drop
	// a global default without a restart. Only the window, which the live
	// windows already depend on, is pinned at boot.
	defaults := func() middleware.Limits {
		d := repo.GetRateLimitDefaults()
		if !d.Enabled {
			return middleware.Limits{}
		}
		return middleware.Limits{RPM: d.RPM, TPM: d.TPM, Concurrency: d.Concurrency}
	}
	guardrailStore := guardrails.NewStore(repo.RawDB())
	// One switch for both taps. It reads the settings row per request, which is
	// a single indexed read on an already-loaded connection, and buys an
	// operator an off switch that does not require deleting their policies.
	guardrailSwitch := guardrails.Switch(func() bool { return repo.GetGuardrailsEnabled() })
	var engineChatH *chat.ChatHandler
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireApiKey(repo))
		r.Use(middleware.RequireRateLimit(rateLimiter, defaults, repo))
		r.Use(guardrails.Inbound(guardrailStore, guardrailAudit(repo), guardrailSwitch))
		engineChatH = SetupRoutes(r, repo, ts)

		fileH := batches.NewFileHandler(repo)
		batchH := batches.NewBatchHandler(repo)

		// File endpoints (OpenAI Batch API compatible)
		r.Post("/v1/files", fileH.HandleCreateFile)
		r.Post("/files", fileH.HandleCreateFile)
		r.Get("/v1/files", fileH.HandleListFiles)
		r.Get("/files", fileH.HandleListFiles)
		r.Get("/v1/files/{file_id}", fileH.HandleGetFile)
		r.Get("/files/{file_id}", fileH.HandleGetFile)
		r.Get("/v1/files/{file_id}/content", fileH.HandleGetFileContent)
		r.Get("/files/{file_id}/content", fileH.HandleGetFileContent)
		r.Delete("/v1/files/{file_id}", fileH.HandleDeleteFile)
		r.Delete("/files/{file_id}", fileH.HandleDeleteFile)

		// Batch endpoints (OpenAI Batch API compatible)
		r.Post("/v1/batches", batchH.HandleCreateBatch)
		r.Post("/batches", batchH.HandleCreateBatch)
		r.Get("/v1/batches", batchH.HandleListBatches)
		r.Get("/batches", batchH.HandleListBatches)
		r.Get("/v1/batches/{batch_id}", batchH.HandleGetBatch)
		r.Get("/batches/{batch_id}", batchH.HandleGetBatch)
		r.Post("/v1/batches/{batch_id}/cancel", batchH.HandleCancelBatch)
		r.Post("/batches/{batch_id}/cancel", batchH.HandleCancelBatch)
	})

	// The provider gate (docs/provider-limits-plan.md) reads the same limiter
	// for its per-node buckets, which the per-key middleware never touches.
	// Without this call the gate is inert on a real boot: its unit tests set
	// the field by hand, so only a booted router can tell the difference.
	engineChatH.SetRateLimiter(rateLimiter)

	// Dashboard management API: login-gated when requireLogin is on, but still
	// reachable with a valid API key or the local CLI token (upstream parity).
	// It reads the cache through the engine's chat handler: versionH was built
	// before any traffic, so the cache on that instance never fills.
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireDashboardAuth(repo))
		SetupDashboardRoutes(r, repo, engineChatH)
	})

	// CLI Tools status is a dashboard read: the SPA calls it with the session
	// cookie, never an LLM API key. It was registered inside SetupRoutes, which
	// is mounted under RequireApiKey, so every dashboard call 401'd and the view
	// bounced to the endpoint tab. Reachable with a session, an API key, or the
	// local CLI token — same policy as the rest of the dashboard group.
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireDashboardAuth(repo))
		r.Get("/api/cli-tools/all-statuses", media.NewCLIToolsHandler().HandleAllStatuses)
		// Hermes configuration is per-profile and writes into ~/.hermes on the
		// operator's disk, so it belongs to the dashboard group: the SPA reaches
		// it with the session cookie, never an engine API key.
		hermesH := media.NewHermesHandler()
		r.Get("/api/cli-tools/hermes-profiles", hermesH.HandleProfiles)
		r.Get("/api/cli-tools/hermes-settings", hermesH.HandleGet)
		r.Post("/api/cli-tools/hermes-settings", hermesH.HandlePost)
		r.Delete("/api/cli-tools/hermes-settings", hermesH.HandleDelete)
		// pi writes the same kind of file Hermes does — a CLI agent's own
		// config on the operator's disk — so it rides the same dashboard group.
		piH := media.NewPiHandler(repo)
		r.Get("/api/cli-tools/pi-settings", piH.HandleGet)
		r.Post("/api/cli-tools/pi-settings", piH.HandlePost)
		r.Delete("/api/cli-tools/pi-settings", piH.HandleDelete)
		// Codex multi-model profiles: one file per profile under CODEX_HOME
		// (~/.codex by default), selected with `codex -p <name>`. Same group as
		// the settings routes above, because it writes a CLI's own files on the
		// operator's disk rather than proxying an LLM request.
		codexProfilesH := media.NewCodexProfilesHandler()
		r.Get("/api/cli-tools/codex-profiles", codexProfilesH.HandleGet)
		r.Post("/api/cli-tools/codex-profiles", codexProfilesH.HandlePost)
		r.Delete("/api/cli-tools/codex-profiles", codexProfilesH.HandleDelete)

		// Every clientconfig adapter (the nine `connect` clients plus the ten
		// dashboard-only ones — codewhale, copilot, cowork, deepseek-tui,
		// devin, forge, grok-build, jcode, openclaw, smelt) answers the same
		// three verbs. Upstream gives every client its own Next route; here one
		// handler resolves the id out of the path, so the adapter table stays
		// the only place a client is described and a new adapter needs no
		// registration code.
		cliH := media.NewCLIToolHandler(engineChatH)
		for _, id := range clientconfig.IDs() {
			// pi has its own handler above (it merges over a previously saved
			// block); skip it so the two cannot disagree about the same file.
			if id == "pi" {
				continue
			}
			route := "/api/cli-tools/" + id + "-settings"
			r.Get(route, cliH.HandleCLIToolGet)
			r.Post(route, cliH.HandleCLIToolPost)
			r.Delete(route, cliH.HandleCLIToolDelete)
		}
	})

	SetupConsoleLogRoutes(r, repo)
	return engineChatH
}
