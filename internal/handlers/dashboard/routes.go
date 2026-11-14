package dashboard

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"panrouter/proxy/internal/db"
	"panrouter/proxy/internal/dbbackup"
	"panrouter/proxy/internal/handlers/shared"
	"panrouter/proxy/internal/handlerutil"
	"panrouter/proxy/internal/mcpbridge"
	"panrouter/proxy/internal/middleware"
	"panrouter/proxy/internal/modelhealth"
	"panrouter/proxy/internal/models"
	"panrouter/proxy/internal/observ"
	"panrouter/proxy/internal/resilience"
	"panrouter/proxy/internal/semanticcache"
)

// Default probe URLs for proxy pool health checks.
const (
	DefaultPrimaryProbeURL   = "https://www.google.com/generate_204"
	DefaultSecondaryProbeURL = "https://cloudflare.com/cdn-cgi/trace"
)

// DashboardHandler handles dashboard REST API endpoints.
type DashboardHandler struct {
	Repo              *db.Repo
	PrimaryProbeURL   string
	SecondaryProbeURL string
	TokenSaver        *shared.TokenSaverConfig
	SemanticCache     *semanticcache.Cache
	// Resilience is the provider-level admission control the data plane
	// consults. Held here so the dashboard reports the same layer a request was
	// judged against rather than a second estimate that can drift from it. Nil
	// when the handler was built outside a boot (tests, tooling).
	Resilience *resilience.Controller
	// Limiter is the process-wide rate limiter, shared with the data plane so the
	// dashboard reports the same provider windows the dispatch gate enforces. It
	// is optional: a handler built outside a boot (tests, tooling) has none, and
	// the live usage fields then read zero rather than being invented.
	Limiter *middleware.RateLimiter
	// jailbreakImport fetches prompt collections from GitHub for the preset
	// library. Nil means the production importer; a test points it at an
	// httptest server.
	jailbreakImport *jailbreakImporter
	// ModelHealth is the automatic model-health loop. It is optional for the
	// same reason Limiter is: a handler built outside a boot has none, and the
	// operator-action recording the disabled-models write performs is skipped
	// rather than invented. Wiring it matters — without it a model the operator
	// re-enables stays marked scheduler-owned, so the loop either refuses to
	// disable it again or silently re-enables it on the next good probe.
	//
	// It is read per request rather than snapshotted: the router is built
	// before the app's OnStart hook starts the scheduler, so a handler that
	// captured the loop at construction would hold nil in production and
	// record nothing.
	ModelHealth *modelhealth.Loop
	// Backup is the automatic database-backup runner (daily snapshots pushed to
	// a private GitHub repo). Every side effect it performs — the GitHub API
	// read, the snapshot, the git push — is a field on it, so a test replaces
	// those instead of reaching the network.
	Backup *dbbackup.Service
	// DatabasePath is the SQLite file /api/backup/download serves. Empty means
	// the configured path, resolved per request so a test can point the route
	// at a fixture file.
	DatabasePath string
	// batchTest runs one connection probe for POST /api/providers/test-batch.
	// Nil means the production probe (the same testSingleConnection +
	// persistProbeResult pair the single-row Test button runs); a test injects
	// a fake sender here so "one attempt per connection" is asserted without a
	// network and without sleeping.
	batchTest func(ctx context.Context, conn *models.ProviderConnection) batchTestOutcome
	// modelProbe runs one per-model probe for POST
	// /api/providers/{id}/test-models. Nil means the handler was built outside a
	// boot (tests, tooling) and no probe was wired; the route then refuses rather
	// than reporting verdicts nothing measured. Production wires the media
	// handler's probe, so a model test here is the same call the dashboard's Test
	// button makes.
	modelProbe func(ctx context.Context, model, kind string) handlerutil.ModelProbeOutcome
	// MCPBridge is the stdio MCP bridge the /api/mcp routes drive. Nil means the
	// process-wide bridge (mcpbridge.Default), which is what production uses:
	// one child per plugin is shared by every request, so a second bridge would
	// spawn a second npx process under the same plugin name. A test supplies its
	// own so the routes drive a stub process instead of npx.
	MCPBridge *mcpbridge.Bridge
	// coworkRegistry is the one cached Anthropic MCP catalogue entry, with the
	// upstream TTL and a size cap. The zero value is ready to use.
	coworkRegistry coworkRegistryCache
	// coworkMCPNow is the TTL clock for the registry cache. Nil means time.Now;
	// a test freezes it instead of sleeping.
	coworkMCPNow func() time.Time
}

// NewDashboardHandler initializes a DashboardHandler with the provided Repo.
func NewDashboardHandler(repo *db.Repo, ts ...*shared.TokenSaverConfig) *DashboardHandler {
	h := &DashboardHandler{
		Repo:              repo,
		PrimaryProbeURL:   DefaultPrimaryProbeURL,
		SecondaryProbeURL: DefaultSecondaryProbeURL,
		Backup:            dbbackup.New(repo),
	}
	if len(ts) > 0 && ts[0] != nil {
		h.TokenSaver = ts[0]
	}
	// An install that already had backups switched on keeps its schedule across
	// a restart: the handler is built once per boot, which is when upstream's
	// initializeApp starts the loop. A config write re-runs this to match.
	h.Backup.Configure()
	return h
}

// importer returns the configured GitHub importer, or the production one.
func (h *DashboardHandler) importer() *jailbreakImporter {
	if h.jailbreakImport != nil {
		return h.jailbreakImport
	}
	return defaultJailbreakImporter
}

// getURLParam retrieves a route parameter from Chi URLParam or standard PathValue.
func getURLParam(r *http.Request, key string) string {
	if v := chi.URLParam(r, key); v != "" {
		return v
	}
	if key == "key" {
		if v := chi.URLParam(r, "*"); v != "" {
			return v
		}
	}
	return r.PathValue(key)
}

// RegisterRoutes registers all dashboard REST endpoints under /api on the router.
func RegisterRoutes(r chi.Router, h *DashboardHandler) {
	r.Route("/api", func(r chi.Router) {
		// Boot surface: the dashboard pings /api/init on load and the language
		// switcher writes the locale cookie. Both are public upstream
		// (dashboardGuard PUBLIC_API_PATHS), so their real mounts are in
		// SetupServerRouter; the mirrors here keep this table complete for the
		// dashboard package's own tests.
		r.Get("/init", h.HandleGetInit)
		r.Get("/locale", h.HandleGetLocale)
		r.Post("/locale", h.HandleSetLocale)

		// Pricing table the pricing card reads, and the MCP stdio bridge the
		// cowork plugin view drives. Both sit in the dashboard auth group
		// upstream (PROTECTED_API_PATHS).
		r.Get("/pricing", h.HandleGetPricing)
		r.Get("/mcp/{plugin}/sse", h.HandleMCPSSE)
		r.Post("/mcp/{plugin}/message", h.HandleMCPMessage)

		// Cowork's network-backed CLI-tools routes: Anthropic's MCP catalogue
		// (cached for an hour) and an SSRF-gated MCP tools probe. They share the
		// dashboard auth group with /api/cli-tools/cowork-settings upstream.
		r.Get("/cli-tools/cowork-mcp-registry", h.HandleCoworkMCPRegistry)
		r.Post("/cli-tools/cowork-mcp-tools", h.HandleCoworkMCPTools)

		// Connections
		r.Get("/connections", h.HandleGetConnections)
		r.Post("/connections", h.HandleCreateConnection)
		r.Put("/connections/{id}", h.HandleUpdateConnection)
		r.Put("/providers/{id}", h.HandleUpdateConnection)
		r.Delete("/connections/{id}", h.HandleDeleteConnection)
		r.Delete("/providers/{id}", h.HandleDeleteConnection)
		r.Post("/connections/{id}/test", h.HandleTestConnection)
		r.Post("/providers/{id}/test", h.HandleTestConnection)
		r.Get("/providers/{id}/models", h.HandleGetConnectionModels)
		// Per-model probe: the connection id resolves the provider and its
		// catalogue, and each model is pinged through the endpoint its kind serves.
		r.Post("/providers/{id}/test-models", h.HandleTestProviderModels)
		r.Get("/providers/{id}/export-keys", h.HandleExportProviderKeys)
		// Bulk removal for the rows the import path writes; the single-row form
		// stays at DELETE /models/custom/{key}.
		r.Delete("/providers/{id}/models", h.HandleDeleteProviderModels)
		r.Get("/providers/{id}/overrides", h.HandleGetProviderOverrides)
		r.Put("/providers/{id}/overrides", h.HandleSaveProviderOverrides)
		r.Post("/providers/validate", h.HandleValidateProvider)
		// Batch connection test and Kilo's free-model feed: the dashboard's
		// "test all" buttons and the Kilo card, both beside the provider routes
		// they belong to.
		r.Post("/providers/test-batch", h.HandleTestProvidersBatch)
		r.Get("/providers/kilo/free-models", h.HandleGetKiloFreeModels)
		// The provider registry is the single source of truth for the dashboard's
		// catalog (category, icon, colour, auth type, service kinds), so the SPA
		// fetches it instead of shipping a second copy that can drift.
		r.Get("/providers/catalog", h.HandleGetProviderCatalog)

		// Provider Nodes (Custom Endpoints)
		r.Get("/provider-nodes", h.HandleGetProviderNodes)
		r.Post("/provider-nodes", h.HandleCreateProviderNode)
		r.Put("/provider-nodes/{id}", h.HandleUpdateProviderNode)
		r.Post("/provider-nodes/validate", h.HandleValidateProviderNode)
		r.Delete("/provider-nodes/{id}", h.HandleDeleteProviderNode)
		// Per-node capacity limits (docs/provider-limits-plan.md). The GET list
		// above carries them too; these two write the policy and lift a spent
		// token quota without touching the history the analytics reads.
		r.Put("/provider-nodes/{id}/limits", h.HandleUpdateProviderNodeLimits)
		r.Post("/provider-nodes/{id}/quota/reset", h.HandleResetProviderNodeQuota)

		// Combos
		r.Get("/combos", h.HandleGetCombos)
		r.Post("/combos", h.HandleCreateCombo)
		r.Post("/combos/auto-free", h.HandleAutoFreeCombo)
		r.Post("/combos/auto-family", h.HandleAutoFamilyCombos)
		r.Get("/combos/presets", h.HandleGetComboPresets)
		r.Post("/combos/presets", h.HandleCreateComboPresets)
		r.Put("/combos/{id}", h.HandleUpdateCombo)
		// Smart select combo: dry-run a rule list, and apply every combo's rules
		// on demand. The automatic half runs on each model write.
		r.Post("/combos/reconcile", h.HandleReconcileSmartCombos)
		r.Post("/combos/{id}/smart/preview", h.HandleComboSmartPreview)
		r.Post("/combos/{id}/smart/load", h.HandleComboSmartLoad)
		r.Get("/models/meta", h.HandleGetModelMeta)
		r.Delete("/combos/{id}", h.HandleDeleteCombo)

		// Proxy Pools
		r.Get("/proxy-pools", h.HandleGetProxyPools)
		r.Post("/proxy-pools", h.HandleCreateProxyPool)
		r.Put("/proxy-pools/{id}", h.HandleUpdateProxyPool)
		r.Delete("/proxy-pools/{id}", h.HandleDeleteProxyPool)
		r.Post("/proxy-pools/{id}/test", h.HandleTestProxyPool)
		// Pre-flight for the Netlify CLI, the same read the deploy button makes
		// before it offers an install instead of a doomed deploy.
		r.Get("/proxy-pools/netlify-cli-status", h.HandleGetNetlifyCLIStatus)

		// API Keys
		r.Get("/keys", h.HandleGetApiKeys)
		r.Post("/keys", h.HandleCreateApiKey)
		r.Delete("/keys/{id}", h.HandleDeleteApiKey)
		r.Put("/keys/{id}/toggle", h.HandleToggleApiKey)
		r.Post("/keys/{id}/rotate", h.HandleRotateApiKey)
		r.Get("/keys/{id}/models", h.HandleGetApiKeyModels)
		r.Put("/keys/{id}/models", h.HandleSetApiKeyModels)
		r.Get("/keys/{id}/budget", h.HandleGetApiKeyBudget)
		r.Put("/keys/{id}", h.HandleUpdateApiKey)

		// F-3 Plans: reusable policy templates the keys inherit from.
		r.Get("/plans", h.HandleListPlans)
		r.Post("/plans", h.HandleCreatePlan)
		r.Get("/plans/{id}", h.HandleGetPlan)
		r.Put("/plans/{id}", h.HandleUpdatePlan)
		r.Delete("/plans/{id}", h.HandleDeletePlan)
		r.Get("/plans/{id}/keys", h.HandlePlanKeys)

		// F-5 Credential vault: status and master-key rotation.
		r.Get("/vault/status", h.HandleGetVaultStatus)
		r.Post("/vault/rotate", h.HandleRotateVault)
		r.Get("/metrics", observ.Handler().ServeHTTP)

		// Guardrails: policy CRUD and the audit log.
		r.Get("/guardrails/policies", h.HandleGetGuardrailPolicies)
		r.Post("/guardrails/policies", h.HandleCreateGuardrailPolicy)
		r.Put("/guardrails/policies/{id}", h.HandleUpdateGuardrailPolicy)
		r.Delete("/guardrails/policies/{id}", h.HandleDeleteGuardrailPolicy)
		r.Get("/guardrails/logs", h.HandleListGuardrailLogs)

		// Jailbreak: agent prompt presets library; per-model preset binding.
		r.Get("/jailbreak", h.HandleGetJailbreak)
		r.Put("/jailbreak", h.HandlePutJailbreak)
		r.Get("/jailbreak/presets", h.HandleListJailbreakPresets)
		r.Put("/jailbreak/presets", h.HandlePutJailbreakPreset)
		r.Delete("/jailbreak/presets/{id}", h.HandleDeleteJailbreakPreset)
		r.Post("/jailbreak/presets/{id}/duplicate", h.HandleDuplicateJailbreakPreset)
		r.Get("/jailbreak/models", h.HandleListJailbreakModels)
		r.Get("/jailbreak/models/catalog", h.HandleJailbreakModelCatalog)
		r.Put("/jailbreak/models", h.HandleSetJailbreakModel)
		r.Delete("/jailbreak/models", h.HandleDeleteJailbreakModel)
		r.Get("/jailbreak/excludes", h.HandleGetJailbreakExcludes)
		r.Put("/jailbreak/excludes", h.HandlePutJailbreakExcludes)
		r.Post("/jailbreak/import", h.HandleImportJailbreak)
		r.Post("/jailbreak/resolve", h.HandleExplainJailbreak)
		r.Get("/jailbreak/usage", h.HandleJailbreakUsage)
		r.Post("/jailbreak/preview", h.HandlePreviewJailbreak)
		r.Put("/jailbreak/presets/order", h.HandleReorderJailbreakPresets)
		r.Post("/jailbreak/presets/upload", h.HandleUploadJailbreakPreset)

		// Models
		r.Get("/models/custom", h.HandleGetCustomModels)
		r.Get("/models/caps", h.HandleGetModelCaps)
		// Live cooldown board: which models are parked, and clearing one.
		r.Get("/models/availability", h.HandleGetModelAvailability)
		r.Post("/models/availability", h.HandleClearModelCooldown)
		r.Post("/models/custom", h.HandleSaveCustomModel)
		r.Delete("/models/custom/{key}", h.HandleDeleteCustomModel)
		r.Delete("/models/custom/*", h.HandleDeleteCustomModel)
		r.Get("/models/disabled", h.HandleGetDisabledModels)
		r.Put("/models/disabled/{provider}", h.HandleSaveDisabledModels)
		r.Post("/models/auto-disable", h.HandleAutoDisableModels)
		r.Get("/models/health", h.HandleGetModelHealth)
		r.Put("/models/health", h.HandleSaveModelHealth)

		// Automatic database backup: GitHub snapshot config, one-shot run, the
		// mirror download token and its CRUD. The download is the only route
		// here that authenticates with its own credential (x-backup-token)
		// instead of the dashboard session, so the server drops it out of the
		// dashboard auth group — see middleware.RequireDashboardAuth.
		r.Get("/backup/config", h.HandleGetBackupConfig)
		r.Post("/backup/config", h.HandlePostBackupConfig)
		r.Post("/backup/run", h.HandleRunBackup)
		r.Get("/backup/download", h.HandleDownloadBackup)
		r.Get("/backup/tokens", h.HandleListBackupTokens)
		r.Post("/backup/tokens", h.HandleCreateBackupToken)
		r.Patch("/backup/tokens", h.HandleUpdateBackupToken)
		r.Delete("/backup/tokens", h.HandleDeleteBackupToken)

		// Settings
		r.Get("/settings", h.HandleGetSettings)
		r.Put("/settings", h.HandleUpdateSettings)
		r.Get("/settings/database", h.HandleExportDatabase)
		r.Post("/settings/database", h.HandleImportDatabase)
		r.Get("/settings/database/stats", h.HandleGetDatabaseStats)
		r.Post("/settings/database/prune", h.HandlePruneDatabase)
		r.Post("/settings/database/prune-usage", h.HandlePruneUsageHistory)
		r.Post("/settings/database/vacuum", h.HandleVacuumDatabase)
		r.Post("/settings/proxy-test", h.HandleProxyTest)
		// White-label logo upload: the file lands under DATA_DIR/uploads and
		// appLogoUrl is rewritten to the path this gateway serves it from.
		r.Post("/settings/logo", h.HandleUploadLogo)
		r.Post("/settings/currency/refresh", h.HandleRefreshCurrencyRates)

		// Token Saver Testing Benches
		r.Post("/tokensaver/rtk/test", h.HandleTestRTK)
		r.Post("/tokensaver/caveman/test", h.HandleTestCaveman)

		// Tunnel & Tailscale
		r.Get("/tunnel/status", h.HandleTunnelStatus)
		r.Post("/tunnel/enable", h.HandleTunnelEnable)
		r.Post("/tunnel/disable", h.HandleTunnelDisable)
		// Rewrites the tunnel's ingress rule to point at this gateway's own
		// origin. Separate from enable: an operator who moved the gateway to a
		// new port needs the rule fixed without tearing the tunnel down.
		r.Post("/tunnel/repair-origin", h.HandleTunnelRepairOrigin)
		r.Get("/tunnel/tailscale-check", h.HandleTailscaleCheck)
		r.Post("/tunnel/tailscale-enable", h.HandleTailscaleEnable)
		// SSE install stream: progress lines, then the login URL.
		r.Post("/tunnel/tailscale-install", h.HandleTailscaleInstall)
		r.Post("/tunnel/tailscale-disable", h.HandleTailscaleDisable)
		// Usage & Quotas
		r.Get("/usage/providers", h.HandleGetUsageProviders)
		// Usage per client API key. Registered before the {connectionId}
		// route: chi matches in order, so a route added after it would be
		// read as a connection id.
		r.Get("/usage/api-keys", h.HandleGetUsageByAPIKey)
		r.Get("/usage/{connectionId}", h.HandleGetConnectionUsage)
		// Codex reset credits, upstream names
		// (src/app/api/usage/[connectionId]/codex-reset-credits/route.js):
		// GET lists, POST spends one.
		r.Get("/usage/{connectionId}/codex-reset-credits", h.HandleListCodexResetCredits)
		r.Post("/usage/{connectionId}/codex-reset-credits", h.HandleConsumeCodexResetCredit)
		// "Check & release": read the provider's live quota and lift the block
		// when the provider says the account is usable again.
		r.Post("/usage/{connectionId}/check-release", h.HandleCheckRelease)

		// GLM Reset Cards (Z.ai international). Only for provider == "glm".
		r.Get("/usage/{connectionId}/glm-reset-cards", h.HandleGetGLMResetCards)
		r.Post("/usage/{connectionId}/glm-reset-cards", h.HandleRedeemGLMResetCard)

		// Claude reset credits (cedar_ember grants), upstream's /claude-reset
		// resource: GET is the list the chooser reads, POST spends one grant.
		r.Get("/usage/{connectionId}/claude-reset", h.HandleListClaudeResetCredits)
		r.Post("/usage/{connectionId}/claude-reset", h.HandleConsumeClaudeResetCredit)

		// Cache Analytics & Management
		r.Get("/resilience", h.HandleGetResilience)
		r.Post("/resilience/reset", h.HandleResetResilience)
		r.Get("/cache", h.HandleGetCache)
		r.Delete("/cache", h.HandleDeleteCache)
		r.Get("/cache/entries", h.HandleGetCacheEntries)
		r.Delete("/cache/entries", h.HandleDeleteCacheEntry)

		// Compression Analytics
		r.Get("/analytics/compression", h.HandleGetCompressionAnalytics)
	})
	// Operator-supplied dashboard assets (the white-label logo). Registered
	// here as well as in SetupServerRouter because that table is mounted by
	// tests only: a route added there alone is reachable in a unit test and
	// 404s on the real server, which is how the upload half was nearly
	// shipped. The page gate that guards it in production is applied by
	// SetupServerRouter, not here — the test router has no session to present.
	r.Get("/uploads/*", h.HandleServeUploads)
}
