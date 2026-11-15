package chat

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"panrouter/proxy/internal/db"
	"panrouter/proxy/internal/handlerutil"
	"panrouter/proxy/internal/headroom"
	"panrouter/proxy/internal/log"
	"panrouter/proxy/internal/middleware"
	"panrouter/proxy/internal/observ"
	"panrouter/proxy/internal/providers"
	"panrouter/proxy/internal/proxy/executor"
	"panrouter/proxy/internal/thinking"
	"panrouter/proxy/internal/tokensaver"
	"panrouter/proxy/internal/tracing"
	"panrouter/proxy/internal/translator"
	"panrouter/proxy/internal/usagetracker"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	internalproxy "panrouter/proxy/internal/proxy"
)

// StatusClientClosedRequest is the canonical HTTP status for client connection aborts (nginx 499).
const StatusClientClosedRequest = 499

// clientClosedBody is the JSON body that travels with StatusClientClosedRequest.
// Single-sourced so the combo and account-fallback paths cannot drift apart.
const clientClosedBody = `{"error":{"message":"client closed request","type":"client_closed_request","code":499}}`

// borrowZenKeyForGo returns the identity and key of an active opencode-zen
// connection for the paid go lane, which shares the same backend account.
// Only used when no go connection exists at all; resolution goes through the
// standard connection path so health, stickiness and token refresh apply.
func (h *ChatHandler) borrowZenKeyForGo(ctx context.Context, model string) (connID, connName, connEmail, apiKey string, err error) {
	zenConns, err := h.Repo.GetProviderConnections("opencode-zen", true)
	if err != nil {
		return "", "", "", "", err
	}
	for _, c := range zenConns {
		connObj, connData, err := h.getBestConnectionWithContext(ctx, "opencode-zen", c.ID, nil, model)
		if err != nil || connObj == nil || connData == nil {
			continue
		}
		if apiKey := extractAPIKey(connData); apiKey != "" {
			return connObj.ID, connObjName(connObj), connObjEmail(connObj), apiKey, nil
		}
	}
	return "", "", "", "", fmt.Errorf("no active opencode-zen connection with an API key")
}

// handleAccountFallback attempts to forward a request with automatic account fallback.
func (h *ChatHandler) handleAccountFallback(
	ctx context.Context,
	w http.ResponseWriter,
	provider string,
	model string,
	pinnedConnectionID string,
	body []byte,
	isStream bool,
	translateResponse bool,
	endpoint string,
) error {
	if FallbackAttemptRecorderFrom(ctx) == nil {
		ctx = WithFallbackAttemptRecorder(ctx, NewFallbackAttemptRecorder())
	}
	body = repairToolCallIDsInJSON(body)
	if pinnedConnectionID != "" {
		connObj, connData, err := h.getBestConnectionWithContext(ctx, provider, pinnedConnectionID, nil, model)
		if err != nil {
			return fmt.Errorf("pinned connection %s: %w", pinnedConnectionID, err)
		}
		log.Debug("fallback", "pinned", "pinnedConn", pinnedConnectionID, "connObj", connObj.ID)
		return h.tryForwardWithConnection(forwardRequestParams{
			Ctx: ctx, W: w, Provider: provider, Model: model,
			ConnectionID: connObj.ID, ConnName: connObjName(connObj), ConnEmail: connObjEmail(connObj),
			ConnData: connData, Body: body, Attempts: 1,
			IsStream: isStream, TranslateResponse: translateResponse, Endpoint: endpoint,
		})
	}

	if !handlerutil.IsProbeContext(ctx) && !h.Repo.IsProviderAvailable(provider, model) {
		log.Warn("fallback", "skip unhealthy", "provider", provider, "model", model)
		return fmt.Errorf("provider %s/%s is unhealthy", provider, model)
	}

	observ.IncFallback(provider, model, observ.FallbackReasonUnhealthy)

	allConns, err := h.Repo.GetProviderConnections(provider, true)
	if err != nil || len(allConns) == 0 {
		// opencode-go shares its backend account with opencode-zen: with no
		// go connection configured, borrow the key from an active zen
		// connection (key only — provider, BaseURL and lanes stay go).
		// Operator-requested; the borrow is logged so it stays visible.
		if provider == "opencode-go" {
			if connID, connName, connEmail, apiKey, bErr := h.borrowZenKeyForGo(ctx, model); bErr == nil {
				log.Warn("fallback", "opencode-go has no connection, borrowing opencode-zen key", "conn", connID)
				return h.tryForwardWithConnection(forwardRequestParams{
					Ctx: ctx, W: w, Provider: provider, Model: model,
					ConnectionID: connID, ConnName: connName, ConnEmail: connEmail,
					ConnData: &ConnectionData{APIKey: apiKey, ProxyPoolID: h.ResolveProviderProxyPoolID(provider)}, Body: body, Attempts: 1,
					IsStream: isStream, TranslateResponse: translateResponse, Endpoint: endpoint,
				})
			} else {
				log.Debug("fallback", "no zen key to borrow for opencode-go", "error", bErr)
			}
		}
		if cfg, ok := providers.KnownProviders[provider]; ok && (cfg.NoAuth || cfg.DefaultAPIKey != "") {
			apiKey := cfg.DefaultAPIKey
			if apiKey == "" {
				apiKey = "public"
			}
			proxyPoolID := h.ResolveProviderProxyPoolID(provider)
			if err := h.tryForwardWithConnection(forwardRequestParams{
				Ctx: ctx, W: w, Provider: provider, Model: model,
				ConnectionID: "default", ConnData: &ConnectionData{APIKey: apiKey, ProxyPoolID: proxyPoolID}, Body: body, Attempts: 1,
				IsStream: isStream, TranslateResponse: translateResponse, Endpoint: endpoint,
			}); err != nil {
				// Same contract as the multi-connection loop's return: a
				// free-tier refusal leaves this path as the classified
				// answer, not the upstream body verbatim.
				return opencodeClosureError(provider, model, err)
			}
			return nil
		}
		observ.IncFallback(provider, model, observ.FallbackReasonNoConnection)
		return fmt.Errorf("no active connections for provider: %s", provider)
	}

	var strat db.ProviderStrategy
	affinityEligible := false
	if h.Repo != nil {
		if settings, sErr := h.Repo.GetSettings(); sErr == nil && settings != nil {
			strat = connRotationStrategy(settings, provider)
			affinityEligible = isAffinityEligibleStrategy(strat.RotateStrategy)
		}
	}

	// Apply provider connection routing strategy (round-robin, sticky, random) or session affinity
	if len(allConns) > 1 && strat.RotateStrategy != "" && strat.RotateStrategy != "none" {
		sessionID := handlerutil.GetSessionID(ctx)
		hasAffinity := false
		if sessionID != "" && affinityEligible {
			if boundID, ok := h.getSessionAffinity().Get(provider, model, sessionID); ok && boundID != "" {
				for idx, c := range allConns {
					if c != nil && c.ID == boundID {
						if idx > 0 {
							bound := allConns[idx]
							copy(allConns[1:idx+1], allConns[0:idx])
							allConns[0] = bound
						}
						hasAffinity = true
						break
					}
				}
				if !hasAffinity {
					h.getSessionAffinity().Evict(provider, model, sessionID)
				}
			}
		}
		if !hasAffinity {
			effectiveStrat := strat
			if sessionID != "" && affinityEligible {
				effectiveStrat.StickyLimit = 1
			}
			allConns = h.applyConnectionStrategy(allConns, effectiveStrat)
		}
	}

	var excludeIDs []string
	// Pools already refused by an OpenCode free-tier gate in this turn. The
	// resolver filters them out, so the retry walks to the next egress instead
	// of burning a healthy account.
	excludedPools := map[string]bool{}
	var lastErr error
	for _, c := range allConns {
		if slices.Contains(excludeIDs, c.ID) {
			continue
		}
		connObj, connData, err := h.getBestConnectionWithContext(ctx, provider, c.ID, nil, model, excludedPools)
		if err != nil || connObj == nil {
			if lastErr == nil && err != nil {
				lastErr = err
			}
			continue
		}
		apiKey := extractAPIKey(connData)
		if apiKey == "" {
			providerCfg, pErr := h.getProviderConfig(provider, connData)
			switch {
			case pErr == nil && providerCfg.DefaultAPIKey != "":
				apiKey = providerCfg.DefaultAPIKey
			case customJSONKeyOptional(connData):
				// A custom JSON endpoint may be keyless by design: the node
				// says so, and the connection carries no credential. Requiring
				// one would make the node unusable through the fallback path.
			default:
				if lastErr == nil {
					lastErr = fmt.Errorf("connection %s has no API key", connObj.ID)
				}
				continue
			}
		}
		// connObj, not the loop candidate: a candidate in cooldown can resolve
		// to a different account (force fallback), and the usage row, the logs
		// and the exclusion below must all name the account that was dialled.
		log.Debug("fallback", "connection", "conn", connObj.ID, "candidate", c.ID)
		attemptStart := time.Now()
		if err := h.tryForwardWithConnection(forwardRequestParams{
			Ctx: ctx, W: w, Provider: provider, Model: model,
			ConnectionID: connObj.ID, ConnName: connObjName(connObj), ConnEmail: connObjEmail(connObj),
			ConnData: connData, Body: body, Attempts: attemptCount(ctx),
			IsStream: isStream, TranslateResponse: translateResponse, Endpoint: endpoint,
		}); err == nil {
			RecordAttemptFromContext(ctx, connObj.ID, connObjName(connObj), provider, model, 200, nil, time.Since(attemptStart).Milliseconds())
			if sessionID := handlerutil.GetSessionID(ctx); sessionID != "" && affinityEligible {
				h.getSessionAffinity().Bind(provider, model, sessionID, connObj.ID)
			}
			return nil
		} else {
			lastErr = err
			observ.IncFallback(provider, model, observ.FallbackReasonUpstreamError)
			RecordAttemptFromContext(ctx, connObj.ID, connObjName(connObj), provider, model, 0, err, time.Since(attemptStart).Milliseconds())
			var ue *upstreamError
			if errors.As(lastErr, &ue) {
				if ue.StatusCode == http.StatusTooManyRequests || ue.StatusCode == http.StatusUnauthorized || ue.StatusCode == http.StatusForbidden {
					if sessionID := handlerutil.GetSessionID(ctx); sessionID != "" {
						h.getSessionAffinity().Evict(provider, model, sessionID)
					}
				}
			}
		}
		// A custom JSON business call is never replayed on the next account: the
		// endpoint may already have performed its operation, and the doomed
		// second call is the automation signature a business API bans on.
		if isCustomJSONConn(connData) {
			log.Info("fallback", "custom JSON failure is terminal, not rotating",
				"provider", provider, "conn", connObj.ID, "error", lastErr)
			return lastErr
		}

		// A node-state refusal (contract ended, lifetime quota spent) answers 403, which
		// is a retryable status, so without this it would fall into the generic branch
		// below and park the connection for two minutes on the model *and* on the
		// account — for a fact about the node, not about the credential. The combo loops
		// already intercept it before their writers; the single-provider path arrives
		// here, through a pinned connection that skipped the selection-time check.
		if _, refused := providerLimitRefusal(lastErr); refused {
			log.Info("fallback", "provider node state, not locking the account",
				"provider", provider, "model", model, "conn", connObj.ID)
			excludeIDs = append(excludeIDs, connObj.ID)
			continue
		}

		var ue *upstreamError
		if errors.As(lastErr, &ue) && providers.IsModelDeprecation(ue.StatusCode, ue.Body) {
			// A retired model fails on every account of this provider, so the
			// remaining connections are skipped and the badge the operator
			// needs is recorded before the client sees the 410 (#179). The
			// lastErr still returns when every account is out, so a direct
			// request keeps reporting the upstream's own body.
			h.recordModelDeprecation(ctx, provider, model, connObj.ID, ue)
			excludeIDs = append(excludeIDs, connObj.ID)
			lastErr = ue
			continue
		}
		if errors.As(lastErr, &ue) && providers.RetryableStatusCodes[ue.StatusCode] {
			// A gateway fault (500–504) is about the upstream, not the
			// credential: it earns the model a short cooldown badge and a lock
			// on this account, so the next request does not re-dial an upstream
			// that just failed in the same second.
			if ue.StatusCode >= http.StatusInternalServerError {
				h.recordModelCooldown(ctx, provider, model, connObj.ID, ue)
			}
			if handlerutil.IsProbeContext(ctx) {
				excludeIDs = append(excludeIDs, connObj.ID)
				continue
			}
			// A free-tier refusal says nothing about the account or the model:
			// the gate is not serving this EGRESS at the moment. Exclude the
			// pool that served the attempt and retry the same account through
			// the next one; with every pool excluded the resolver stops
			// binding a pool (errNoUsablePool) and the retry goes direct.
			// Cooling the account down would mark it unhealthy over an egress
			// verdict, so the cooldown stays untouched.
			if isOpencodeFreeTierRefusal(provider, ue) {
				if poolID := connData.ProxyPoolID; poolID != "" {
					log.Warn("fallback", "opencode free-tier refusal, rotating proxy pool",
						"conn", connObj.ID, "provider", provider, "model", model, "pool", poolID)
					excludedPools[poolID] = true
					continue
				}
				log.Warn("fallback", "opencode free-tier refusal, not locking the account",
					"conn", connObj.ID, "provider", provider, "model", model)
				excludeIDs = append(excludeIDs, connObj.ID)
				continue
			}
			// Extract error text from upstream body for classification
			errorText := extractErrorText(ue.Body)
			// Get current backoff level from this connection
			currentBackoffLevel := h.Repo.GetConnectionBackoffLevel(connObj.ID)
			// Classify error to get dynamic cooldown; provider-scoped rules
			// key off the canonical provider the request routed to.
			classification := providers.ClassifyErrorForProvider(ue.StatusCode, errorText, currentBackoffLevel, provider)
			cooldownSec := retryableCooldownSec(ue.StatusCode, time.Duration(classification.CooldownMs)*time.Millisecond, ue, provider)
			errMsg := errorText
			if errMsg == "" {
				errMsg = fmt.Sprintf("%d upstream error", ue.StatusCode)
			}
			lockKey := canonicalLockModel(provider, model)
			h.Repo.LockConnectionModel(connObj.ID, lockKey, cooldownSec, classification.NewBackoffLevel)
			if lockKey != model {
				_ = h.Repo.LockConnectionModel(connObj.ID, model, cooldownSec, classification.NewBackoffLevel)
			}
			// Account-scoped cooldown alongside the per-model locks, so the
			// selector can skip this account before spending a request
			// (upstream applyErrorState). Only lock if error is account-scoped,
			// not model-scoped (e.g. 401 auth issues), so unrelated models stay available.
			if isModelScopedError(ue.StatusCode, errorText, model, ue.Body) {
				if recErr := h.Repo.RecordConnectionScopedError(connObj.ID, model, "chat", ue.StatusCode, errorText, classification.NewBackoffLevel); recErr != nil {
					log.Warn("fallback", "record connection error failed", "conn", connObj.ID, "error", recErr)
				}
			} else {
				until := time.Now().UTC().Add(time.Duration(cooldownSec) * time.Second)
				// A spent budget is parked as a quota: upstream records the window
				// the provider named in quotaExhaustedUntil, which is what the
				// dashboard's cooldown readout and the router's quota state read.
				// Every other account-scoped refusal keeps the generic cooldown.
				// Both writes take the same deadline, so the park and the lock can
				// never name different dates.
				if !h.parkQuotaExhausted(connObj.ID, ue, provider, errorText, classification, until) {
					if lockErr := h.Repo.LockConnectionRateLimit(connObj.ID, until, classification.NewBackoffLevel, ue.StatusCode, errorText); lockErr != nil {
						log.Warn("fallback", "rate limit lock failed", "conn", connObj.ID, "error", lockErr)
					}
				}
				h.getSessionAffinity().EvictConnection(connObj.ID)
			}
			log.Warn("fallback", "connection locked", append([]any{
				"conn", connObj.ID, "provider", provider, "model", model,
				"lockKey", lockKey, "status", ue.StatusCode, "cooldown_s", cooldownSec,
			}, connIdentityKV(connObj)...)...)
			if stopsRotationOnRateLimit(provider, ue) {
				log.Warn("fallback", "rate limit stops the rotation",
					"provider", provider, "conn", connObj.ID, "model", model, "status", ue.StatusCode)
				return lastErr
			}
			excludeIDs = append(excludeIDs, connObj.ID)
			continue
		}
		// A gateway fault (5xx) is not retried across accounts, but it is still
		// a fact about the model's health right now: record the cooldown badge
		// and lock the model so the next request does not re-dial an upstream
		// that just failed.
		if errors.As(lastErr, &ue) && ue.StatusCode >= http.StatusInternalServerError {
			h.recordModelCooldown(ctx, provider, model, connObj.ID, ue)
		}
		return lastErr
	}
	if lastErr != nil {
		return opencodeClosureError(provider, model, lastErr)
	}
	return fmt.Errorf("no available connections for provider: %s", provider)
}

// opencodeClosureError turns a free-tier refusal that survived the executor's
// retries into a response the caller can act on, instead of forwarding the
// upstream body verbatim.
//
// The gate refuses in two shapes (see isOpencodeFreeTierRefusal): a 403
// FreeTierError when it is not serving free traffic from this egress at that
// moment, and a 429 FreeUsageLimitError once the free budget is spent. That is
// a temporary unavailability of a shared free tier, not a verdict on the
// caller's credentials — but a 403 is read as "you are not allowed" by every
// client, including this gateway's own callers, so an agent loop stops
// mid-tool-call and the user is left with a wall of text about downloading
// OpenCode.
//
// The classified answer keeps the caller's back-off honest: 429/503 plus a
// Retry-After, so a client waits and retries rather than giving up or assuming
// its key is wrong. The upstream's own wording is preserved in the message so
// the cause is still diagnosable, and the type names the condition so a client
// can branch on it without string matching.
type opencodeRefusalDetail struct {
	StatusCode int
	ErrorType  string
	Message    string
	RetryAfter int
}

func classifyOpencodeFreeTierRefusal(provider string, body []byte) opencodeRefusalDetail {
	msg := extractErrorText(body)
	lower := strings.ToLower(msg)
	// The type token is the stable half of the payload. extractErrorText keeps
	// only the message, and the usage-limit message ("Free usage limit reached.
	// Subscribe to OpenCode Go to keep going") names neither a rate limit nor a
	// quota — so the token has to be read for the shape to be classified.
	token := strings.ToLower(string(body))

	switch {
	case strings.Contains(lower, "not available in your country") ||
		strings.Contains(lower, "country_blocked") ||
		strings.Contains(lower, "country not allowed") ||
		strings.Contains(lower, "region"):
		return opencodeRefusalDetail{
			StatusCode: http.StatusForbidden,
			ErrorType:  "free_tier_region_blocked",
			Message:    fmt.Sprintf("OpenCode's free tier is not available in your region (%s). Route through a proxy pool in a supported country.", provider),
		}

	case isOpencodeQuotaRefusal(lower, token):
		return opencodeRefusalDetail{
			StatusCode: http.StatusTooManyRequests,
			ErrorType:  "free_tier_quota_exhausted",
			Message:    fmt.Sprintf("OpenCode free tier rate limit or quota exceeded for this egress. Retry in about 60s, rotate proxy pool, or use a paid %s key.", provider),
			RetryAfter: 60,
		}

	case strings.Contains(lower, "upgrade required") ||
		strings.Contains(lower, "upgrade_required") ||
		strings.Contains(lower, "client version") ||
		strings.Contains(lower, "version deprecated"):
		return opencodeRefusalDetail{
			StatusCode: http.StatusUpgradeRequired,
			ErrorType:  "free_tier_upgrade_required",
			Message:    "OpenCode client version is deprecated or requires upgrade.",
		}

	case strings.Contains(lower, "detect abuse traffic") ||
		strings.Contains(lower, "abuse"):
		return opencodeRefusalDetail{
			StatusCode: http.StatusServiceUnavailable,
			ErrorType:  "free_tier_abuse_detected",
			Message:    "OpenCode free tier gateway flagged abuse traffic on this egress. Route through a clean residential/ISP proxy pool, or retry in about 30s.",
			RetryAfter: 30,
		}

	default:
		const retryAfterSeconds = 30
		return opencodeRefusalDetail{
			StatusCode: http.StatusServiceUnavailable,
			ErrorType:  "free_tier_unavailable",
			Message: fmt.Sprintf(
				"OpenCode's free tier is not serving requests right now. This is an upstream condition rather than a problem with your request or credentials. Retry in about %ds, or use a paid %s key or another provider.",
				retryAfterSeconds, provider,
			),
			RetryAfter: retryAfterSeconds,
		}
	}
}

// isOpencodeQuotaRefusal reports whether the gate's answer is its usage-limit
// shape: the FreeUsageLimitError type token, or prose naming a rate limit or a
// quota (the 403 lane reports the same condition in words).
func isOpencodeQuotaRefusal(msg, token string) bool {
	return strings.Contains(token, "freeusagelimiterror") ||
		strings.Contains(msg, "rate limit") ||
		strings.Contains(msg, "too many requests") ||
		strings.Contains(msg, "quota") ||
		strings.Contains(msg, "usage limit")
}

func opencodeClosureError(provider, model string, err error) error {
	var ue *upstreamError
	if !errors.As(err, &ue) || !isOpencodeFreeTierRefusal(provider, ue) {
		return err
	}
	detail := classifyOpencodeFreeTierRefusal(provider, ue.Body)
	// The carrier is the authority when it names a wait: the classifier's 60s is
	// only an estimate. A carrier longer than the safety ceiling is clamped, so a
	// hostile or nonsensical value cannot park the egress forever — the same
	// maxResetCooldown every other quota path respects.
	if carrier, ok := opencodeCarrierRetryAfter(ue.Header, time.Now()); ok {
		detail.RetryAfter = int(carrier / time.Second)
	}
	if limit := int(maxResetCooldown / time.Second); detail.RetryAfter > limit {
		detail.RetryAfter = limit
	}
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": detail.Message,
			"type":    detail.ErrorType,
			"code":    detail.StatusCode,
		},
	})
	log.Warn("fallback", "opencode free tier refused after retries; reporting classified failure",
		"provider", provider, "model", model, "type", detail.ErrorType, "status", detail.StatusCode)
	header := http.Header{
		"Content-Type": []string{"application/json"},
	}
	if detail.RetryAfter > 0 {
		header.Set("Retry-After", fmt.Sprint(detail.RetryAfter))
	}
	return &upstreamError{
		StatusCode: detail.StatusCode,
		Body:       body,
		Header:     header,
	}
}

// tryForwardWithConnection attempts a single upstream request using the given connection data.
// isAnthropicUpstream reports whether the request is headed to Anthropic's
// native Messages API (as opposed to an anthropic-compatible custom node).
func isAnthropicUpstream(provider string, cfg *providers.ProviderConfig) bool {
	if cfg == nil {
		return false
	}
	if cfg.UpstreamIsAnthropic {
		return true
	}
	if provider != "claude" && provider != "anthropic" {
		return false
	}
	if cfg == nil {
		return false
	}
	targetURL := cfg.BaseURL
	// Backward compatibility: if an unmigrated test or caller still supplies
	// the old header set, resolve the destination from it.
	if cfg.StaticHeaders != nil {
		if relayTarget, ok := cfg.StaticHeaders["x-relay-target"]; ok && relayTarget != "" {
			targetURL = relayTarget + cfg.StaticHeaders["x-relay-path"]
		}
	}
	return targetURL == "https://api.anthropic.com/v1/messages" ||
		strings.HasPrefix(targetURL, "https://api.anthropic.com/v1/messages?")
}

// servesClaudeMessages reports whether the provider's own endpoint speaks
// Anthropic Messages, so the body reaches it in Claude format and the reply
// comes back Claude-shaped.
//
// That is a different question from isAnthropicUpstream: Anthropic's own API
// additionally needs the beta query, the OAuth cloaking and the beta-flag
// merge below, none of which a third-party Messages endpoint wants. A provider
// declares the wire format on its registry entry (Format: "claude"), which is
// the same field the /v1/models metadata publishes.
func servesClaudeMessages(provider string, cfg *providers.ProviderConfig) bool {
	return cfg != nil && cfg.Format == providers.FormatClaude
}

func appendBetaQuery(u string) string {
	if strings.Contains(u, "beta=true") {
		return u
	}
	if strings.Contains(u, "?") {
		return u + "&beta=true"
	}
	return u + "?beta=true"
}

// forwardRequestParams bundles tryForwardWithConnection inputs. Ten positional
// params (including two adjacent bools) made call sites unreadable and
// extension error-prone; named fields fix the call sites while the function
// body intentionally keeps short local aliases.
type forwardRequestParams struct {
	Ctx               context.Context
	W                 http.ResponseWriter
	Provider          string
	Model             string
	RequestedModel    string
	ComboName         string
	Attempts          int
	Protocol          string
	StartedAt         time.Time
	ConnectionID      string
	ConnName          string // human-readable account name, for logs
	ConnEmail         string
	ConnData          *ConnectionData
	Body              []byte
	IsStream          bool
	TranslateResponse bool
	Endpoint          string
}

func (h *ChatHandler) tryForwardWithConnection(f forwardRequestParams) error {
	ctx, w := f.Ctx, f.W
	provider, model := f.Provider, f.Model
	connectionID, connData := f.ConnectionID, f.ConnData
	// Per-connection opt-in for a private/reserved upstream: the SSRF guard is
	// what stops a credential-bearing connection from reaching an internal
	// host, and this flag is the operator's explicit exception for a local
	// gateway or a LAN peer. It rides on the request context, so the dialer
	// sees it on both the direct and the proxied legs.
	if connData != nil && connData.AllowPrivateNetworking {
		ctx = internalproxy.WithAllowPrivateNetworking(ctx)
	}
	body, isStream := f.Body, f.IsStream
	translateResponse, endpoint := f.TranslateResponse, f.Endpoint
	// Unified thinking normalisation, inbound half: capture the client's
	// reasoning intent before any converter rewrites the body. model stays
	// the requested id for logs/usage; wireModel is suffix-free.
	body, wireModel, thinkingIntent := h.prepareInboundThinking(ctx, provider, model, body, f.Endpoint == "/v1/v1/messages" || f.Endpoint == "/v1/messages")
	ctx = translator.WithUsageCapture(ctx)

	providerCfg, err := h.getProviderConfig(provider, connData)
	if err != nil {
		return fmt.Errorf("get config for %s/%s: %w", provider, model, err)
	}
	// anthropic-compatible nodes fronting a Claude model need Anthropic-Beta flags (parity #3797)
	if (provider == "claude" || strings.HasPrefix(provider, "anthropic-compatible-") || strings.HasPrefix(provider, "anthropic")) && strings.HasPrefix(model, "claude-") {
		if providerCfg.StaticHeaders == nil {
			providerCfg.StaticHeaders = make(map[string]string)
		}
		if _, ok := providerCfg.StaticHeaders["Anthropic-Beta"]; !ok {
			providerCfg.StaticHeaders["Anthropic-Beta"] = "prompt-caching-scope-2026-01-05, context-management-2025-06-27"
		}
	}

	// OAuth connections (e.g. Claude subscription logins) must authenticate
	// with "Authorization: Bearer" against the beta endpoint, matching the
	// Next.js dashboard (open-sse/executors/default.js + registry/claude.js).
	// Sending the OAuth access token via x-api-key makes api.anthropic.com
	// return 401 "API key is invalid" even after a successful token refresh.
	// A connection is OAuth when it only carries an accessToken (no apiKey).
	isAnthropic := isAnthropicUpstream(provider, providerCfg)
	// OAuth = connection carries only an accessToken, OR the token itself is
	// an Anthropic OAuth token (some connections store sk-ant-oat in APIKey).
	isOAuth := isAnthropic && connData != nil &&
		((connData.APIKey == "" && connData.AccessToken != "") ||
			strings.Contains(connData.APIKey, "sk-ant-oat"))
	if isOAuth {
		providerCfg.AuthHeader = "Authorization"
		providerCfg.AuthScheme = "bearer"
		if providerCfg.StaticHeaders != nil && providerCfg.StaticHeaders["x-relay-path"] != "" {
			providerCfg.StaticHeaders["x-relay-path"] = appendBetaQuery(providerCfg.StaticHeaders["x-relay-path"])
		} else {
			providerCfg.BaseURL = appendBetaQuery(providerCfg.BaseURL)
		}
	}

	apiKey := resolveProviderAuthToken(provider, connData, extractAPIKey(connData))
	if apiKey == "" {
		switch {
		case providerCfg.DefaultAPIKey != "":
			apiKey = providerCfg.DefaultAPIKey
		case customJSONKeyOptional(connData):
			// A keyless custom JSON node is usable without a credential: the
			// endpoint authenticates its own callers or needs none.
		default:
			return &upstreamError{StatusCode: http.StatusUnauthorized, Body: []byte(`{"error":{"message":"no API key found","type":"auth_error","code":401}}`)}
		}
	}

	if connectionID != "" {
		rekey, _, err := h.refreshOAuthTokenIfExpired(connectionID, apiKey)
		if err == nil {
			apiKey = rekey
		} else {
			log.Warn("fallback", "OAuth token refresh error", "conn", connectionID, "error", err)
		}
	}
	apiKey = NormalizeProviderToken(provider, apiKey)

	// Token savers + provider-format normalization:
	// - /v1/messages (claudeNative): body stays Claude format; savers inject
	//   into top-level "system".
	// - /v1/chat/completions to an Anthropic upstream: the body is OpenAI
	//   format and would otherwise be forwarded raw; convert it to a
	//   spec-compliant Claude Messages payload (top-level system, tools,
	//   merged roles) — same conversion the dashboard applies server-side.
	// claudeNative indicates the CLIENT body is in Claude Messages format.
	// Requires an Anthropic upstream too: chat.go converts /v1/messages
	// requests for non-Anthropic providers (DeepSeek, OpenAI-compatible) to
	// OpenAI format before the fallback, and passing endpoint "/v1/v1/messages"
	// alone would wrongly inject a top-level "system" the upstream ignores.
	//
	// A /v1/messages client is only converted away from Claude format when the
	// upstream cannot answer in it. That is true of two kinds of provider: one
	// routing some of its models to a Messages endpoint (opencode-zen's Claude
	// and Qwen lanes), and one whose own endpoint speaks Messages outright
	// (minimax-code on MiniMax's mavis gateway). Both keep the client's own
	// wire format end to end — upstream resolveTransport picks the
	// sourceFormat-matched transport and skips translation.
	messagesClient := endpoint == "/v1/v1/messages" || endpoint == "/v1/messages"
	// A Messages-speaking provider answers a Messages client in its own wire
	// format, whichever of the two ways it earns that: a model routed to a
	// Messages endpoint (opencode-zen's Claude and Qwen lanes), or an endpoint
	// that speaks Messages outright (minimax-code on the mavis gateway).
	// Anthropic's own two provider ids are the third way, and they follow the
	// same rule chat.go applies when it decides whether to convert at all: it
	// keeps the body in Claude format for provider "claude"/"anthropic"
	// whatever base URL the connection carries. Mirrored here, because this
	// flag is also what says "the body arriving is Claude-shaped" — without
	// the name check a relay or proxy override made isAnthropic false, the
	// outbound thinking pass ran with target "openai", and
	// FilterToOpenAIFormatJSON rewrote a Claude body's tools into
	// {type:"function",function:{…}} on its way to a Messages API.
	claudeNative := messagesClient && (isAnthropic ||
		provider == "claude" || provider == "anthropic" ||
		executor.ServesMessagesEndpoint(provider, model) ||
		servesClaudeMessages(provider, providerCfg))

	// upstreamClaude is what the executor needs to translate the Claude reply
	// back for a Chat Completions client. Anthropic's own API and a third-party
	// Messages endpoint both answer in Claude, but only Anthropic's wants the
	// beta query, the OAuth cloaking and the beta-flag merge below.
	upstreamClaude := isAnthropic || servesClaudeMessages(provider, providerCfg)
	compressStart := time.Now()
	saver := h.applyTokenSavers(ctx, body, claudeNative)
	pipedBody := saver.Body
	origTokens, savedTokens, savedPct := saver.OrigTokens, saver.SavedTokens, saver.SavedPct
	compressDurMs := int(time.Since(compressStart).Milliseconds())
	if compressDurMs <= 0 {
		compressDurMs = 1
	}
	var claudeToolMap map[string]string
	if upstreamClaude {
		if !claudeNative {
			// Raw OpenAI-format body would be invalid at the Messages API:
			// convert to a spec-compliant Claude payload (top-level system,
			// tools, merged roles) — what the dashboard does server-side.
			pipedBody = executor.EnsureClaudeMessagesFor(pipedBody, provider, model)
		}
		// The OpenAI→Claude conversion merges turns, so a client body whose
		// last message was a tool result can still land on an assistant turn.
		// A prefill is the client's own choice, so it is detected on the
		// pre-conversion body (upstream ensureTrailingUserTurn, claude.js:345-349).
		prefill := translator.ClaudeIntentionalPrefill(body)
		pipedBody = translator.EnsureTrailingUserTurnBody(pipedBody, prefill)

		// OAuth connections (or sk-ant-oat tokens) require Claude-Code-shaped requests:
		// billing-header system block + metadata.user_id + cloaked tools,
		// or the API 429s (anti-abuse fingerprinting).
		if isOAuth || strings.Contains(apiKey, "sk-ant-oat") {
			pipedBody = applyClaudeCloaking(pipedBody, apiKey, handlerutil.GetSessionID(ctx))
			var reqMap map[string]any
			if err := json.Unmarshal(pipedBody, &reqMap); err == nil {
				claudeToolMap = cloakClaudeTools(reqMap)
				if out, err := json.Marshal(reqMap, json.Deterministic(true)); err == nil {
					pipedBody = out
				}
			}
		}
	}

	// A body that asked for thinking summaries must not also carry the
	// redact-thinking beta: it asks Anthropic for signature-only thinking blocks
	// and would blank exactly what the client requested. The header map is
	// shared with the registry, so this edits a request-local copy.
	if isAnthropic && thinking.BodyMentionsThinking(pipedBody) && executor.WantsThinkingSummaries(pipedBody) {
		providerCfg.StaticHeaders = providers.WithoutBetaFlag(
			providerCfg.StaticHeaders, providers.AnthropicBetaRedactThinking,
		)
	}

	if isAnthropic {
		// The caller's own beta flags are merged into the upstream request
		// instead of being dropped: a client asking for a beta the gateway does
		// not list would be refused without ever being told why. The registry
		// header map is shared, so this edits a request-local copy.
		if clientBeta := handlerutil.GetClientAnthropicBeta(ctx); clientBeta != "" {
			merged := providers.MergeAnthropicBeta(providerCfg.StaticHeaders["Anthropic-Beta"], clientBeta)
			if merged != "" {
				providerCfg.StaticHeaders = providers.WithHeader(providerCfg.StaticHeaders, "Anthropic-Beta", merged)
			}
		}

		// Claude OAuth wants x-claude-code-session-id to agree with the
		// metadata.user_id the cloak generates, otherwise the API sees two
		// different sessions for one request.
		if isOAuth || strings.Contains(apiKey, "sk-ant-oat") {
			if sid := claudeSessionIDFromBody(pipedBody); sid != "" {
				if providerCfg.StaticHeaders["x-claude-code-session-id"] == "" {
					providerCfg.StaticHeaders = providers.WithHeader(
						providerCfg.StaticHeaders, "x-claude-code-session-id", sid,
					)
				}
			}
		}
	}

	// Sanitize tool schemas for all OpenAI-compatible providers (opencode, gemini-openai, etc.)
	// Fixes misplaced `required` inside `properties` and missing `items` for arrays.
	if sanitized, err := translator.SanitizeOpenAITools(pipedBody); err == nil && sanitized != nil && string(sanitized) != string(pipedBody) {
		log.Debug("fallback", "sanitized tools", "provider", provider, "model", model, "conn", connectionID[:min(8, len(connectionID))], "beforeBytes", len(pipedBody), "afterBytes", len(sanitized))
		pipedBody = sanitized
	} else if err != nil {
		log.Warn("fallback", "sanitize failed", "provider", provider, "model", model, "error", err)
	}
	// DeepSeek answers 400 "Tool names must be unique" for a request that
	// declares the same tool twice, so same-name definitions are collapsed at
	// the last point before dispatch — after every conversion (OpenAI →
	// Claude for an Anthropic upstream included), like upstream
	// dedupeTools in open-sse/handlers/chatCore.js. Scoped by model id, so no
	// other provider's tool array is touched.
	if deduped := translator.DedupeToolsDeepSeek(pipedBody, model); len(deduped) != len(pipedBody) {
		log.Debug("fallback", "deduped duplicate tool names", "provider", provider, "model", model)
		pipedBody = deduped
	}
	// DeepSeek (and Kimi) thinking mode requires every assistant message echoed
	// back with a reasoning_content field whenever the request declares tools;
	// OpenAI-shaped clients drop it when replaying history, and the API answers
	// 400 ("The reasoning_content in the thinking mode must be passed back to
	// the API") without it. A placeholder satisfies the validator; reasoning the
	// client really sent is preserved. Port of upstream
	// utils/reasoningContentInjector.js, run at the same last-point-before-
	// dispatch stage as the tool collapse above. Internal provider/model rules
	// decide applicability (any provider serving a deepseek/kimi model id), so
	// the call itself is unconditional on non-Claude lanes.
	if !upstreamClaude {
		pipedBody = executor.InjectReasoningContent(pipedBody, provider)
		if provider == "deepseek" {
			// DeepSeek V4 models think by default and reject required/named
			// tool_choice while thinking is on ("Thinking mode does not
			// support this tool_choice"); downgrade the pin to auto so the
			// turn is served instead of failing with 400.
			pipedBody = executor.NormalizeDeepSeekThinkingToolChoice(pipedBody)
		}
	}

	// Fit tool names exceeding MaxToolNameLength (64 chars) to prevent upstream HTTP 400.
	fittedBody, fittedToolMap := translator.FitToolNames(pipedBody)
	if len(fittedToolMap) > 0 {
		log.Debug("fallback", "fitted long tool names", "provider", provider, "model", model, "count", len(fittedToolMap))
		pipedBody = fittedBody
		w = executor.NewToolNameRestoringWriter(w, fittedToolMap)
		ctx = translator.WithToolNameMap(ctx, fittedToolMap)
		if claudeToolMap == nil {
			claudeToolMap = make(map[string]string, len(fittedToolMap))
		}
		for k, v := range fittedToolMap {
			claudeToolMap[k] = v
		}
	}

	// G11: a target whose wire format cannot fetch a remote image would receive
	// the URL as a dead string. Done here rather than inside a translator
	// because this is the one place that knows both the target provider and the
	// client's own body shape. Fail-open: any refusal leaves the body as-is.
	pipedBody = prefetchImagesForTarget(ctx, provider, model, providerCfg, pipedBody)

	// Codex Fast intent: the account's stored preference is the fallback, and a
	// body that carries its own intent wins. Applied after every body rewrite
	// above so a saver or a prefetch cannot reorder or drop the service tier.
	var accountFastOpt *bool
	if connData != nil && connData.ProviderSpecificData != nil {
		if v, ok := connData.ProviderSpecificData["codexFastMode"].(bool); ok {
			accountFastOpt = &v
		}
	}
	if fastBody, modified := ApplyCodexFastMode(ctx, pipedBody, provider, accountFastOpt); modified {
		pipedBody = fastBody
	}

	// Unified thinking normalisation, outbound half (port of open-sse
	// applyThinking and filterToOpenAIFormat). Runs last on the wire body so
	// no converter can resurrect a dialect the target rejects. Claude-native
	// clients author their own Claude thinking and keep their existing
	// handling.
	//
	// The target format is the wire the upstream speaks, not the shape the
	// client sent: a Responses-native upstream (opencode, muse, a custom
	// node on a /responses endpoint) reads reasoning:{effort,summary} and
	// rejects a top-level reasoning_effort, so the pass runs there too —
	// upstream applies the thinking pass for every target format
	// (open-sse/translator/index.js:137). The mandatory-reasoning clamp is
	// part of the writer, so the Responses lane gets it as well.
	if !claudeNative {
		target := "openai"
		switch {
		case executor.UpstreamSpeaksResponses(provider, wireModel, providerCfg):
			target = "openai-responses"
		case isAnthropic || upstreamClaude:
			// A provider whose own endpoint speaks the Messages format
			// (minimax-code on the mavis gateway) gets the Claude wire: its
			// body was converted above, and the OpenAI filter would strip the
			// tool_use blocks and rewrite its tools into a shape the Messages
			// API rejects.
			target = "claude"
		}
		pipedBody = thinking.ApplyJSON(pipedBody, target, provider, wireModel, thinkingIntent)
		if target == "openai" && !providerCfg.IsGeminiNative() {
			filtered, filterErr := translator.FilterToOpenAIFormatJSON(pipedBody, translator.FilterOptions{
				PreserveCacheControl: providers.PreservesCacheControl(provider),
			})
			if filterErr != nil {
				// The request lost every message. The upstream answers an
				// empty messages array with a 400, so the gateway says so
				// itself instead of spending the call.
				return &upstreamError{
					StatusCode: http.StatusBadRequest,
					Body:       []byte(`{"error":{"type":"invalid_request_error","message":"the request carries no messages"}}`),
				}
			}
			pipedBody = filtered
		}
	}

	// A custom openai-compatible node answers 400 "max_tokens and
	// max_completion_tokens cannot both be set" on a streaming request that
	// carries both (repro: bansos/gpt-6-luna on api.sakoo.biz.id — whose
	// backend is GitHub models). A client may spell either field; when both
	// are present the explicit max_completion_tokens value wins and
	// max_tokens is dropped, and when the client sent neither the upstream's
	// own default is honoured.
	//
	// The condition is the wire the body is in, not the client endpoint that
	// reached here: a /v1/responses client is converted to Chat Completions
	// before this point when the target speaks Chat (a custom
	// openai-compatible node), and that conversion keeps both fields as sent.
	// Gating on the endpoint alone left the pair on the wire for exactly the
	// lane this fix came from. A Claude body and a Responses body carry
	// neither field, so the normalizer is a no-op there.
	if !claudeNative && !upstreamClaude && !executor.UpstreamSpeaksResponses(provider, wireModel, providerCfg) {
		pipedBody = normalizeChatTokenFields(pipedBody)
	}

	start := time.Now()
	metrics := &streamMetrics{}
	var fwdErr error

	// The names behind this in-flight request. The forwarder carries both: for
	// a combo request RequestedModel is the combo the client asked for and
	// ComboName marks that a combo served it, while model/provider name the
	// member that is doing the work. The dashboard animates exactly that hop.
	requestedName := f.RequestedModel
	if requestedName == "" {
		requestedName = translator.RequestedModelFromContext(ctx)
	}
	comboName := f.ComboName

	// Correlated console lines for this attempt (port of chatCore.js:258-279 and
	// the saver line at :348). The body is final by now — every translator,
	// sanitizer and saver has rewritten it — so the counts describe what the
	// upstream is about to receive. The request line walks that body, so it is
	// built only when the level will print it; the tag and the attempt id are
	// read either way, because the DONE line of this attempt carries the same
	// pair.
	reqTag := log.TagForSession(handlerutil.GetSessionID(ctx))
	reqID := log.RequestID()
	if log.LineEnabled() {
		// A request whose model id never reached the context (a bypass path, a
		// probe) prints the resolved model, which is what the client asked for
		// in that case.
		clientModel := requestedName
		if clientModel == "" {
			clientModel = provider + "/" + model
		}
		msgs, tools := bodyShapeCounts(pipedBody)
		emitAttemptLines(reqTag, log.RequestLine{
			ClientModel: clientModel,
			Provider:    provider,
			Model:       model,
			SourceFmt:   clientWireFormat(ctx, endpoint),
			TargetFmt:   upstreamWireFormat(provider, wireModel, providerCfg),
			Stream:      isStream,
			Messages:    msgs,
			Tools:       tools,
			Think:       thinkForLine(thinkingIntent),
			AccName:     f.ConnName,
			AccID:       connectionID,
			RequestID:   reqID,
		}, saver)
	}
	usagetracker.GetTracker().TrackPendingRequested(requestedName, comboName, model, provider, connectionID, true, false)
	defer func() {
		hasErr := fwdErr != nil
		usagetracker.GetTracker().TrackPendingRequested(requestedName, comboName, model, provider, connectionID, false, hasErr)
	}()

	// A connection bound to a pool that cannot serve traffic must fail before
	// anything is sent: the first request is the one that would otherwise
	// escape from the real IP while the error only shows on the next attempt.
	httpClient, clientErr := h.getClientForConnection(connData)
	if clientErr != nil {
		// Marshalled, not concatenated: the pool name is user-supplied and a
		// quote in it produced a body that no longer parsed as JSON, so both
		// the client and the diagnostics reader saw "request failed" instead
		// of why the egress refused the request.
		errBody, mErr := json.Marshal(map[string]any{
			"error": map[string]string{"type": "proxy_error", "message": clientErr.Error()},
		})
		if mErr != nil {
			errBody = []byte(`{"error":{"type":"proxy_error","message":"proxy pool unavailable"}}`)
		}
		fwdErr = &upstreamError{StatusCode: http.StatusBadGateway, Body: errBody}
		// Failing closed on an unusable pool is a failed upstream attempt, and
		// upstream records it: the same condition throws out of proxyFetch.js
		// ("Proxy required but failed"), inside executor.execute, so chatCore.js
		// catches it and writes both the requestDetails error row and the
		// isError TrackPending. Assigning fwdErr before returning is also what
		// makes the deferred TrackPending above report isError=true, which is
		// how the topology card learns this provider failed.
		connName, connEmail := identityNames(h.connIdentityKVOr(f, connectionID))
		h.LogFailure(
			ctx,
			&UsageLogInfo{
				Provider:              provider,
				Model:                 model,
				ConnectionID:          connectionID,
				ConnName:              connName,
				ConnEmail:             connEmail,
				Endpoint:              endpoint,
				Egress:                resolveEgress(connData, providerCfg).LogValue(),
				OriginalInputTokens:   origTokens,
				SavedTokens:           savedTokens,
				SavedPercent:          savedPct,
				CompressionDurationMs: compressDurMs,
			},
			nil,
			fwdErr,
			time.Since(start).Milliseconds(),
			body,
			metrics,
		)
		log.Warn("fallback", "upstream blocked by proxy egress", "provider", provider, "model", model, "error", clientErr)
		return fwdErr
	}
	// The client carries the strict-proxy marker itself, but the executors'
	// DoRequest path sees only ctx, so the decision travels on both. A
	// connection with nothing proxied at all is left unmarked: strict there
	// means "never replay this request directly", not "a proxy must exist".
	if connData != nil && (connData.ProxyPoolID != "" || connData.ConnectionProxyEnabled || connData.StrictProxy) {
		ctx = internalproxy.WithStrictProxy(ctx, true)
	}
	if providers.IsLocalByDesign(provider) {
		ctx = internalproxy.WithAllowLoopback(ctx)
	}
	sessionID := handlerutil.GetSessionID(ctx)

	// The outbound guardrail tap goes here, not inside a response handler:
	// this is the one point every dispatch path shares, so a provider with a
	// registered executor is covered by exactly the same policy as one
	// without. Placing it per handler left every executor-backed provider —
	// which is most of the catalog — completely unfiltered, while the unit
	// tests, which exercise a provider with no executor, still passed.
	//
	// The resolved policy goes on the context so the executors pick it up
	// where they write, and a stream is tapped at the writer itself, because a
	// buffered body could still be judged whole but a stream cannot be taken
	// back once its first byte is out.
	ctx = h.withOutboundPolicy(ctx)
	gtapWriter, gtap := guardrailTap(ctx, w, endpoint, isStream)
	if gtap != nil {
		// Closed on the way out so the held window is discharged on the error
		// paths below as well as the success one.
		w = gtapWriter
		defer gtap.Close()
	}

	// Provider-scope capacity limits (docs/provider-limits-plan.md, PR 2). A custom
	// provider is one upstream endpoint shared by every connection under it, so its
	// RPM/TPM/concurrency are enforced here — once per attempt, against the body
	// about to be sent — rather than in a middleware: the provider is only known
	// after model resolution, and a refusal must fail over to the next candidate
	// instead of ending the request.
	//
	// The slot is released when this attempt ends, on every path including
	// failover. Holding it for the rest of the request would let one pass lock the
	// node out of the concurrency it was granted (D6).
	providerSlot, providerRefusal := h.acquireProviderAttempt(ctx, provider, model, connectionID, pipedBody)
	if providerRefusal != nil {
		return providerRefusal
	}
	if providerSlot != nil {
		defer providerSlot.Release()
	}

	// Hand the usage meter a way to reconcile the limiter's TPM reservations —
	// the caller's own and this provider's — against what the turn actually cost.
	//
	// Both are read back off the context rather than recomputed: the limiter
	// estimated from the body it had already read, and estimating twice from a
	// body that may have been rewritten since would settle the charge against a
	// number that was never charged.
	reserved, keyReserved := middleware.ReservedTokensFromContext(ctx)
	if (keyReserved || providerSlot != nil) && h.RateLimiter != nil {
		clientKey := requestKeyFromContext(ctx)
		ctx = middleware.WithTokenSettler(ctx, func(actual int) {
			if keyReserved && clientKey != nil {
				if limit := h.RateLimiter.EffectiveTPM(h.rateLimitDefaults(), clientKey); limit > 0 {
					h.RateLimiter.SettleTokenUsage(clientKey.ID, limit, reserved, actual)
				}
			}
			// The provider's TPM is charged prompt-side and settled with the
			// turn's prompt+completion total, which is the accounting the plan
			// chose (D7, decision 3).
			providerSlot.Settle(actual)
		})
	}

	// Inject served route response headers (x-panrouter-provider, x-panrouter-model, etc.)
	// before any executor or forwarding function writes headers to w.
	injectRouteHeaders(w, provider, model, f.ComboName, f.Attempts)

	// A custom JSON business endpoint answers in its own shape and its reply is
	// final as given, so it has its own executor: routing it through the OpenAI
	// one reports "200 without a completion" and invites a replay of a business
	// transaction.
	if isCustomJSONConn(connData) {
		execReq := &executor.Request{
			Ctx:           ctx,
			Client:        httpClient,
			Config:        providerCfg,
			Provider:      provider,
			APIKey:        apiKey,
			Body:          pipedBody,
			IsStream:      isStream,
			ClaudeClient:  endpoint == "/v1/v1/messages" || endpoint == "/v1/messages",
			TranslateResp: translateResponse,
			ConnectionID:  connectionID,
			SessionID:     sessionID,
			ConnData:      connData.ProviderSpecificData,
			ResponseBuf:   &metrics.ResponseBuf,
			StartTime:     start,
			TTFT:          &metrics.TTFT,
		}
		err := executor.ForwardCustomJSON(w, execReq)
		execReq.Release()
		// A business transaction is never replayed on another account: the
		// endpoint may have charged the operator's customer on the first call.
		if err != nil {
			return customJSONTerminalError(err)
		}
		return nil
	}

	if exec := executor.Get(provider); exec != nil {
		execReq := &executor.Request{
			Ctx:            ctx,
			Client:         httpClient,
			Config:         providerCfg,
			Provider:       provider,
			APIKey:         apiKey,
			Body:           pipedBody,
			IsStream:       isStream,
			ClaudeClient:   endpoint == "/v1/v1/messages" || endpoint == "/v1/messages",
			TranslateResp:  translateResponse,
			ConnectionID:   connectionID,
			SessionID:      sessionID,
			ToolNameMap:    claudeToolMap,
			UpstreamClaude: upstreamClaude && !claudeNative,
			ResponseBuf:    &metrics.ResponseBuf,
			StartTime:      start,
			TTFT:           &metrics.TTFT,
		}
		// client_id cloaking: hand the connection's providerSpecificData
		// (fingerprintId) to executors that mimic an official client.
		if connData != nil && len(connData.ProviderSpecificData) > 0 {
			execReq.ConnData = connData.ProviderSpecificData
		}
		// Cross-process session coordination (e.g. Freebuff leases): nil
		// when no repo (tests), so executors fall back to memory only.
		if h.Repo != nil {
			execReq.Leases = h.Repo
		}
		fwdErr = exec(w, execReq)
		execReq.Release()
	} else if providerCfg.IsGeminiNative() {
		fwdErr = h.forwardGeminiNativeRequest(ctx, w, provider, providerCfg, apiKey, connectionID, pipedBody, isStream, translateResponse, metrics, httpClient)
	} else {
		fwdErr = h.forwardRequest(ctx, w, providerCfg, apiKey, pipedBody, isStream, translateResponse, metrics, httpClient, provider, upstreamClaude, claudeNative)
	}

	// A tap that cut the stream must win over whatever the dispatch returned:
	// the dispatch succeeded as far as it knew, so a blocked stream would
	// otherwise be logged, billed, and cooled down as a served turn.
	fwdErr = guardrailStreamOutcome(gtap, fwdErr)

	var ue *upstreamError
	if errors.As(fwdErr, &ue) && ue.StatusCode == http.StatusUnauthorized && connectionID != "" {
		refreshedKey, _, rErr := h.forceRefreshOAuthToken(connectionID)
		if rErr == nil && refreshedKey != "" && refreshedKey != apiKey {
			log.Info("fallback", "reactive 401 token refresh success, retrying request", "conn", connectionID)
			apiKey = NormalizeProviderToken(provider, refreshedKey)
			if exec := executor.Get(provider); exec != nil {
				retryReq := &executor.Request{
					Ctx:            ctx,
					Client:         httpClient,
					Config:         providerCfg,
					Provider:       provider,
					APIKey:         apiKey,
					Body:           pipedBody,
					IsStream:       isStream,
					ClaudeClient:   endpoint == "/v1/v1/messages" || endpoint == "/v1/messages",
					TranslateResp:  translateResponse,
					ConnectionID:   connectionID,
					SessionID:      sessionID,
					ToolNameMap:    claudeToolMap,
					UpstreamClaude: upstreamClaude && !claudeNative,
					ResponseBuf:    &metrics.ResponseBuf,
					StartTime:      start,
					TTFT:           &metrics.TTFT,
				}
				// Same client_id cloaking as the first attempt: a refreshed
				// retry without ConnData would fall back to a random id and
				// re-brand the request mid-conversation.
				if connData != nil && len(connData.ProviderSpecificData) > 0 {
					retryReq.ConnData = connData.ProviderSpecificData
				}
				if h.Repo != nil {
					retryReq.Leases = h.Repo
				}
				fwdErr = exec(w, retryReq)
				retryReq.Release()
			} else if providerCfg.IsGeminiNative() {
				fwdErr = h.forwardGeminiNativeRequest(ctx, w, provider, providerCfg, apiKey, connectionID, pipedBody, isStream, translateResponse, metrics, httpClient)
			} else {
				fwdErr = h.forwardRequest(ctx, w, providerCfg, apiKey, pipedBody, isStream, translateResponse, metrics, httpClient, provider, upstreamClaude, claudeNative)
			}
		}
	}

	// Codex answers an exhausted 5h/7d window with a 429 carrying
	// usage_limit_reached and the exact reset. Caching it lets the next
	// picker pass skip this account instead of spending a request to
	// rediscover the same 429 (mirrors the Antigravity quota cache).
	//
	// When the body carries no reset, read live quota instead — throttled to
	// one wham call per 30s per connection. That only blocks the account if
	// the reading is actually exhausted, so a per-request or burst 429 on an
	// otherwise healthy account caches a healthy reading and blocks nothing.
	if errors.As(fwdErr, &ue) && provider == "codex" && connectionID != "" && ue.StatusCode == http.StatusTooManyRequests {
		if resetAt := NoteCodexQuotaError(connectionID, ue.StatusCode, ue.Body); resetAt != nil {
			log.Info("fallback", "codex quota exhausted, cached until reset", "conn", shortConnID(connectionID), "resetAt", resetAt.Format(time.RFC3339))
		} else if _, qErr := RefreshCodexQuota(ctx, httpClient, connectionID, apiKey); qErr != nil {
			log.Debug("fallback", "codex quota refresh failed", "conn", shortConnID(connectionID), "error", qErr)
		}
	}

	latencyMs := time.Since(start).Milliseconds()

	// Lightweight request trace for /debug/traces (provider/model latency).
	completed := fwdErr == nil
	if !completed && isClientCanceled(ctx, fwdErr) && metrics != nil && metrics.ResponseBuf.Len() > 0 {
		completed = true
	}
	if !completed && (metrics == nil || metrics.ResponseBuf.Len() == 0) {
		clearRouteHeaders(w)
	}

	status := "error"
	var zc *internalproxy.ZeroContentError
	if completed {
		status = strconv.Itoa(http.StatusOK)
	} else if isClientCanceled(ctx, fwdErr) {
		status = strconv.Itoa(StatusClientClosedRequest)
	} else if errors.As(fwdErr, &zc) {
		status = strconv.Itoa(http.StatusOK)
	} else if ue, ok := fwdErr.(*upstreamError); ok && ue.StatusCode > 0 {
		status = strconv.Itoa(ue.StatusCode)
	}
	tracing.Record(tracing.Span{
		Provider:   provider,
		Model:      model,
		Status:     status,
		DurationMs: latencyMs,
		TTFTMs:     metrics.TTFT,
	})

	usage := translator.GetAndClearUsage(ctx)
	if completed {
		// A dashboard "Test" probe (POST /api/models/test) is dispatched into
		// this very handler, and it reaches the provider for real — but it is
		// not operator traffic. Recording it wrote a usageHistory row and a
		// byProvider bucket, so the probed provider appeared on the Usage
		// Overview topology map having "served" a request nobody sent, and
		// the daily totals counted a token bill that was never metered
		// (#280). Every other probe side effect here is already gated on the
		// context; this is the one that was not.
		if handlerutil.IsProbeContext(ctx) {
			fwdErr = nil
			return nil
		}
		// Clear any existing model lock on success (matching Next.js clearAccountError).
		lockKey := canonicalLockModel(provider, model)
		if unlockErr := h.Repo.UnlockConnectionModel(connectionID, lockKey); unlockErr != nil {
			log.Warn("fallback", "unlock failed", "provider", provider, "model", lockKey, "error", unlockErr)
		}
		if lockKey != model {
			_ = h.Repo.UnlockConnectionModel(connectionID, model)
		}
		// A served request also clears the account-scoped cooldown, so an
		// account that recovered is not kept out of rotation until the
		// cooldown expires on its own.
		if clearErr := h.Repo.ClearConnectionRateLimit(connectionID); clearErr != nil {
			log.Warn("fallback", "rate limit clear failed", "conn", connectionID, "error", clearErr)
		}
		// A served request proves the account is usable again, so drop any
		// cached quota block rather than leaving it to expire on its own.
		if provider == "codex" {
			ClearCodexQuotaBlock(connectionID)
		}
		// A served request proves the model is alive, so a badge recorded by
		// an earlier 410 must not outlive it (#179).
		h.clearModelDeprecation(provider, model)
		if usage == nil {
			usage = &translator.OpenAIUsage{}
		}
		reqModel := f.RequestedModel
		if reqModel == "" {
			reqModel = translator.RequestedModelFromContext(ctx)
		}
		if reqModel == "" {
			reqModel = model
		}
		protocol := f.Protocol
		if protocol == "" {
			protocol = resolveProtocol(endpoint)
		}
		startedAt := f.StartedAt
		if startedAt.IsZero() {
			startedAt = time.Now().Add(-time.Duration(latencyMs) * time.Millisecond)
		}

		logInfo := &UsageLogInfo{
			Provider:              provider,
			Model:                 model,
			RequestedModel:        reqModel,
			ComboName:             f.ComboName,
			Protocol:              protocol,
			CacheSource:           resolveCacheSource(usage),
			StartedAt:             startedAt,
			ConnectionID:          connectionID,
			APIKey:                apiKey,
			Endpoint:              endpoint,
			Egress:                resolveEgress(connData, providerCfg).LogValue(),
			OriginalInputTokens:   origTokens,
			SavedTokens:           savedTokens,
			SavedPercent:          savedPct,
			CompressionDurationMs: compressDurMs,
		}
		logInfo.ConnName, logInfo.ConnEmail = identityNames(h.connIdentityKVOr(f, connectionID))
		h.logUsage(ctx, logInfo, usage, latencyMs, body, metrics)
		// This attempt's completion line, from the usage the path has already
		// parsed and the latency it has already measured — the same three
		// figures upstream prints from (requestDetail.js:86-101).
		log.Line(reqTag, "📊", log.FormatDoneLine(log.DoneLine{
			TotalMs:    latencyMs,
			TTFTMs:     metrics.TTFT,
			InTokens:   usage.PromptTokens,
			OutTokens:  usage.CompletionTokens,
			CacheRead:  usage.GetCachedTokens(),
			CacheWrite: usage.CacheCreationInputTokens,
		}))
		metrics.ResponseBuf.Reset()
		fwdErr = nil
		return nil
	}

	// Consume any terminal usage even when streaming aborts, then persist one
	// failure row. A later fallback attempt gets its own usage holder because
	// the outer handler installs capture once and tryForward never replaces it.
	statusCode := 0
	if errors.As(fwdErr, &ue) {
		statusCode = ue.StatusCode
	} else if errors.As(fwdErr, &zc) {
		statusCode = http.StatusOK
	}
	identity := h.connIdentityKVOr(f, connectionID)
	// Every failure line below carries the egress: a 401/429 from a provider
	// that rate-limits by IP is only diagnosable if the log says whether the
	// request actually left through the pool.
	identity = append(identity, "egress", resolveEgress(connData, providerCfg).LogValue())
	connName, connEmail := identityNames(identity)
	reqModel := f.RequestedModel
	if reqModel == "" {
		reqModel = translator.RequestedModelFromContext(ctx)
	}
	if reqModel == "" {
		reqModel = model
	}
	protocol := f.Protocol
	if protocol == "" {
		protocol = resolveProtocol(endpoint)
	}
	startedAt := f.StartedAt
	if startedAt.IsZero() {
		startedAt = time.Now().Add(-time.Duration(latencyMs) * time.Millisecond)
	}

	h.LogFailure(
		ctx,
		&UsageLogInfo{
			Provider:              provider,
			Model:                 model,
			RequestedModel:        reqModel,
			ComboName:             f.ComboName,
			Protocol:              protocol,
			StartedAt:             startedAt,
			ConnectionID:          connectionID,
			ConnName:              connName,
			ConnEmail:             connEmail,
			APIKey:                apiKey,
			Endpoint:              endpoint,
			Egress:                resolveEgress(connData, providerCfg).LogValue(),
			OriginalInputTokens:   origTokens,
			SavedTokens:           savedTokens,
			SavedPercent:          savedPct,
			CompressionDurationMs: compressDurMs,
		},
		usage,
		fwdErr,
		latencyMs,
		body,
		metrics,
	)
	metrics.ResponseBuf.Reset()
	if isClientCanceled(ctx, fwdErr) {
		log.Info("fallback", "client canceled request", append([]any{
			"provider", provider, "model", model,
		}, identity...)...)
	} else if projectProbeCached(connectionID) {
		log.Debug("fallback", "upstream skipped (cached no-project)", append([]any{
			"provider", provider, "model", model, "error", fwdErr,
		}, identity...)...)
	} else if errors.As(fwdErr, &zc) {
		log.Warn("fallback", "upstream returned zero usable content", append([]any{
			"provider", provider, "model", model,
			"status", statusCode,
		}, identity...)...)
	} else {
		log.Warn("fallback", "upstream failed", append([]any{
			"provider", provider, "model", model,
			"status", statusCode, "error", fwdErr,
		}, identity...)...)
	}

	if !errors.As(fwdErr, &zc) {
		observ.IncUpstreamError(provider, model, statusCode)
	}
	// A client that hung up is not an upstream failure. Returning the raw error
	// here made every abort surface as a bare 502 at the write sites in chat.go:
	// the caller saw "upstream error: context canceled" and the request log
	// recorded a 5xx for a disconnect the gateway did not cause. The combo path
	// already reports this as 499, so the two agree.
	if isClientCanceled(ctx, fwdErr) {
		return &upstreamError{StatusCode: StatusClientClosedRequest, Body: []byte(clientClosedBody)}
	}
	return fwdErr
}

// isClientCanceled reports whether the client that asked for this response
// went away. A deadline the gateway set for itself is not that: the dashboard
// model probe wraps its own call in a 45s timeout, and treating that expiry as
// a hangup reported "HTTP 499: client closed request" for a model that was
// merely still queued upstream. Only an explicit cancel counts, plus the 499
// this package already produced.
func isClientCanceled(ctx context.Context, err error) bool {
	if ctx != nil && errors.Is(ctx.Err(), context.Canceled) {
		return true
	}
	var ue *upstreamError
	if errors.As(err, &ue) && ue.StatusCode == StatusClientClosedRequest {
		return true
	}
	return false
}

// applyTokenSavers runs RTK compression and prompt injection on the request body.
// claudeNative indicates the body is in Claude Messages format (endpoint
// /v1/messages): system prompts must go to the top-level "system" field —
// a role:"system" message is rejected by the Anthropic API.
// false from compress/inject means nothing changed (or unparseable) — keep original, not a failure.
//
// The result carries what the pass did to the body, including the RTK byte
// accounting and the caveman level whose prompt was applied, because the
// correlated console lines print both (chatCore.js:313-322).
func (h *ChatHandler) applyTokenSavers(ctx context.Context, body []byte, claudeNative bool) tokenSaverResult {
	res := tokenSaverResult{Body: body}
	// X-PanRouter-Token-Saver: off|0|false|bypass opts the whole request out of
	// every token saver — RTK, Caveman, Ponytail, ADHD, Headroom, system prompt
	// prefix/suffix — and the body is forwarded untouched with 0 tokens saved.
	if TokenSaverBypassEnabled(ctx) {
		return res
	}
	if h.TokenSaver == nil {
		return res
	}
	// Prompt-injection guard: tag (never block) flagged user content. Early
	// detection here means operators can see abuse before it reaches upstream.
	// Toggle via settings.injectionGuardEnabled (off bypasses the scan).
	if h.TokenSaver.InjectionGuardEnabled() {
		if inj := tokensaver.DetectInjection(body); inj.Flagged {
			log.Warn("guard", "prompt injection flagged", "reasons", inj.Reasons, "messageId", inj.MessageID)
		}
	}
	out := body
	var origTokens, savedTokens, savedPct int
	if h.TokenSaver.RTKEnabled() {
		rtkCfg := h.TokenSaver.RTKConfig()
		next, did, stats := tokensaver.CompressMessagesWithConfigStats(out, rtkCfg)
		// Booked whether or not the pass changed anything: the console line
		// prints nothing without a hit, and the bytes it weighed answer "why
		// was nothing compressed" for a body full of tool output.
		res.RTK = stats
		if did {
			origTokens = len(out) / 4
			compressedTokens := len(next) / 4
			savedTokens = origTokens - compressedTokens
			savedPct = 0
			if origTokens > 0 {
				savedPct = (savedTokens * 100) / origTokens
			}
			log.Info(
				"token_saver", "RTK compressed tool output",
				"orig_est", origTokens,
				"compressed_est", compressedTokens,
				"saved_est", savedTokens,
				"saved_pct", fmt.Sprintf("%d%%", savedPct),
			)
			out = next
		}
	}
	if h.TokenSaver.CavemanInputMode() {
		if next, did := tokensaver.CompressInputMessages(out, h.TokenSaver.CavemanPreserveKeywords()); did {
			out = next
		}
	}
	inject := tokensaver.InjectSystemPrompt
	if claudeNative {
		inject = tokensaver.InjectSystemPromptClaude
	}
	// Persona prompts only ever add to the body, so the size before them is the
	// baseline each injection is measured against. The baseline moves after
	// every successful injection: a persona added on top of an earlier one adds
	// only its own prompt, and measuring it against the pre-persona length would
	// count the earlier persona's bytes a second time.
	beforeLen := len(out)
	if h.TokenSaver.CavemanEnabled() {
		bypass := false
		if h.TokenSaver.CavemanAutoClarity() && tokensaver.ShouldBypassCaveman(out) {
			bypass = true
		}
		if !bypass {
			level := h.TokenSaver.CavemanLevel()
			prompt := tokensaver.GetCavemanPromptWithLang(level, h.TokenSaver.CavemanLanguage())
			if next, did := inject(out, prompt); did {
				out = next
				res.CavemanLevel = level
				recordPersonaInjection(&origTokens, &savedTokens, beforeLen, len(next))
				beforeLen = len(out)
			}
		}
	}
	if h.TokenSaver.PonytailEnabled() {
		prompt := tokensaver.GetPonytailPrompt(h.TokenSaver.PonytailLevel())
		if next, did := inject(out, prompt); did {
			out = next
			recordPersonaInjection(&origTokens, &savedTokens, beforeLen, len(next))
			beforeLen = len(out)
		}
	}
	if h.TokenSaver.ADHDEnabled() {
		prompt := tokensaver.GetADHDPrompt(h.TokenSaver.ADHDLevel())
		if next, did := inject(out, prompt); did {
			out = next
			recordPersonaInjection(&origTokens, &savedTokens, beforeLen, len(next))
			// No baseline update: this is the last persona, so nothing reads
			// beforeLen again. A persona added after this one must move the
			// baseline here too, or it will count the prompts above it twice.
		}
	}
	// Headroom runs after the in-process savers and before the global prompt, so
	// it never sees the prompt this process is about to add.
	//
	// The client is built per call rather than cached on the handler: the engine
	// and the proxy URL are operator settings that can change while requests are
	// in flight, and a shared client would need a lock around every field read
	// to stay correct. Compress itself fails open on every refusal, so the only
	// cost of not caching is one struct allocation.
	//
	// The token counts are measured here rather than taken from the engine: the
	// same estimator RTK uses above, so a saving from either stage is comparable
	// on one scale and the usage page shows one number instead of two. Without
	// this, headroom-only compression left SavedTokens at 0, and the usage page
	// reported no compression at all on a request that was demonstrably
	// compressed — the analytics row is only written when SavedTokens > 0.
	if h.TokenSaver.HeadroomEnabled() {
		url, timeoutMs := h.TokenSaver.HeadroomEndpoint()
		engine := h.TokenSaver.HeadroomEngineName()
		// The built-in engine needs no URL, so a compression that is switched on
		// must not silently do nothing because the URL field happens to be empty.
		if url != "" || headroom.NormalizeEngine(engine) == headroom.EngineBuiltin {
			client := headroom.NewClientWithEngine(url, "", nil, engine)
			if next, did, err := client.Compress(ctx, out, "", timeoutMs); err == nil && did {
				nextOrig := len(out) / 4
				nextCompressed := len(next) / 4
				nextSaved := nextOrig - nextCompressed
				if nextSaved > 0 {
					// Headroom runs after RTK, so its saving is on top of the one
					// already measured: the totals are cumulative, matching what
					// the provider is about to be billed for.
					if nextOrig > origTokens {
						origTokens = nextOrig
					}
					savedTokens += nextSaved
					if origTokens > 0 {
						savedPct = (savedTokens * 100) / origTokens
					}
					log.Info(
						"token_saver", "headroom compressed request",
						"engine", headroom.NormalizeEngine(engine),
						"orig_est", nextOrig,
						"compressed_est", nextCompressed,
						"saved_est", nextSaved,
						"total_saved_est", savedTokens,
						"saved_pct", fmt.Sprintf("%d%%", savedPct),
					)
				}
				out = next
			}
		}
	}
	// Global system prompt, last in the chain so the prefix stays the first
	// thing the model reads and the suffix its last word — after every style
	// prompt has been appended in between.
	if h.TokenSaver.SystemPromptEnabled() {
		if prefix := h.TokenSaver.SystemPromptPrefix(); prefix != "" {
			if next, did := tokensaver.InjectPrefixSystemPrompt(out, prefix, claudeNative); did {
				out = next
			}
		}
		if suffix := h.TokenSaver.SystemPromptSuffix(); suffix != "" {
			if next, did := tokensaver.InjectSuffixSystemPrompt(out, suffix, claudeNative); did {
				out = next
			}
		}
	}
	res.Body = out
	res.OrigTokens = origTokens
	res.SavedTokens = savedTokens
	res.SavedPct = savedPct
	return res
}

func resolveProtocol(endpoint string) string {
	if endpoint == "/v1/messages" {
		return "Claude-Messages"
	}
	if strings.Contains(endpoint, "gemini") {
		return "Gemini"
	}
	return "OpenAI-Chat"
}

func resolveCacheSource(u *translator.OpenAIUsage) string {
	if u != nil && (u.CachedTokens > 0 || (u.PromptTokensDetails != nil && u.PromptTokensDetails.CachedTokens > 0)) {
		return "Upstream (Provider)"
	}
	return "None"
}

// extractErrorText attempts to extract a human-readable error message from an upstream error JSON body.
// Returns "" when the body isn't parseable or has no message field.
func extractErrorText(body []byte) string {
	var parsed struct {
		Error struct {
			Message string `json:"message"`
			Status  string `json:"status"`
			Details []struct {
				Reason string `json:"reason"`
			} `json:"details"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil {
		var parts []string
		if parsed.Error.Message != "" {
			parts = append(parts, parsed.Error.Message)
		} else if parsed.Message != "" {
			parts = append(parts, parsed.Message)
		}
		if parsed.Error.Status != "" {
			parts = append(parts, parsed.Error.Status)
		}
		for _, d := range parsed.Error.Details {
			if d.Reason != "" {
				parts = append(parts, d.Reason)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, " ")
		}
	}
	trimmed := bytes.TrimSpace(body)
	if bytes.HasPrefix(trimmed, []byte("<!DOCTYPE html")) || bytes.HasPrefix(trimmed, []byte("<html")) {
		lower := strings.ToLower(string(trimmed))
		if strings.Contains(lower, "cloudflare") || strings.Contains(lower, "attention required") {
			return "Cloudflare WAF challenge (Attention Required!): check User-Agent or network proxy"
		}
		if titleStart := strings.Index(lower, "<title>"); titleStart != -1 {
			titleEnd := strings.Index(lower[titleStart:], "</title>")
			if titleEnd != -1 {
				return "upstream returned HTML: " + strings.TrimSpace(string(trimmed[titleStart+7:titleStart+titleEnd]))
			}
		}
		return "upstream returned HTML error page"
	}
	return ""
}

var (
	resetsInRegex = regexp.MustCompile(`(?i)resets?\s+in\s+([0-9hms\.]+)`)
	// The sign is its own group: the provider prints UTC+8 and could print
	// UTC-5, and the offset must be read out of the body either way.
	codebuddyResetDateRegex = regexp.MustCompile(`(?i)(\d{4}-\d{2}-\d{2})\s+(\d{2}:\d{2}:\d{2})(?:\s*UTC([+-]?)([0-9:]+))?`)
)

const (
	minResetCooldown = 5 * time.Second
	maxResetCooldown = 2 * time.Hour

	// resetSafetyMargin is upstream RESET_SAFETY_MARGIN_MS
	// (open-sse/executors/codebuddy-intl.js:22-33): a provider that names a
	// reset time states when the quota refills on ITS clock, and the request in
	// flight, the clock skew and the moment the counter rolls over are not the
	// same instant. Asking one second early earns another refusal — and the
	// retry is exactly what a gateway reads as automation. Ten minutes is cheap
	// insurance against a whole extra refusal cycle.
	resetSafetyMargin = 10 * time.Minute

	// maxMonthlyResetCooldown is the ceiling for a provider whose quota window
	// is MONTHLY. Upstream reaches the same number through quotaWindows.js
	// (defaultMaxCooldownMs = 40 days, with `codebuddy-intl` and
	// `codebuddy-cn` registered as MONTHLY scopes): a body naming a reset days
	// away must park the account for that long, because clamping it to the
	// generic two-hour ceiling puts an account that is still out of quota back
	// into rotation.
	maxMonthlyResetCooldown = 40 * 24 * time.Hour

	// maxWeeklyResetCooldown is the ceiling for a provider whose quota window
	// is WEEKLY. Token Harbor's free tier is a rolling 7-day allowance
	// (upstream registers it as QUOTA_SCOPES.WEEKLY), so the reset it names is
	// at most one window out; a longer one is a parse or a clock bug and must
	// not park the account past the window it belongs to. This is also the
	// calendar the park falls back to when the 429 body names no reset.
	maxWeeklyResetCooldown = 7 * 24 * time.Hour
)

// codebuddyIntlProviders are the spellings a stored connection row may use for
// the CodeBuddy Intl site.
var codebuddyIntlProviders = map[string]bool{
	"codebuddy-intl": true,
	"cbai":           true,
}

// stopsRotationOnRateLimit reports whether a rate limit from this provider must
// end the pass instead of rotating to the next account.
//
// Upstream's rule (AGENTS.md §1): a 429 on CodeBuddy Intl gets ZERO retries, the
// account is locked until the provider's own reset plus the margin, and the
// rotation stops. Walking the pool is what turned one quota refusal into the
// automation signature the provider bans on — 29 requests in two seconds across
// 29 accounts — and earned 11140 "account restricted due to unusual activity"
// followed by 30-day bans. Every other provider keeps the pool walk, because
// there the next account is a real second chance rather than a second signal.
func stopsRotationOnRateLimit(provider string, ue *upstreamError) bool {
	if ue == nil || ue.StatusCode != http.StatusTooManyRequests {
		return false
	}
	return codebuddyIntlProviders[provider]
}

// extractResetDuration attempts to extract a structured reset duration from an error payload.
// It parses:
// 1. Google RPC ErrorInfo metadata: quotaResetDelay ("1h12m28.109534319s")
// 2. Text message patterns: "Resets in 1h12m28s."
// 3. Clamps duration between 5 seconds and 2 hours to prevent deadlock / indefinite lockout.
func extractResetDuration(body []byte) (time.Duration, bool) {
	if len(body) == 0 {
		return 0, false
	}

	// 1. Google RPC error details
	var rpcErr struct {
		Error struct {
			Message string `json:"message"`
			Details []struct {
				Metadata map[string]string `json:"metadata"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &rpcErr); err == nil {
		for _, d := range rpcErr.Error.Details {
			if delayStr, ok := d.Metadata["quotaResetDelay"]; ok && delayStr != "" {
				delayStr = strings.TrimRight(delayStr, ".")
				if dur, err := time.ParseDuration(delayStr); err == nil && dur > 0 {
					return clampResetDuration(dur), true
				}
			}
		}
		if rpcErr.Error.Message != "" {
			if matches := resetsInRegex.FindStringSubmatch(rpcErr.Error.Message); len(matches) > 1 {
				raw := strings.TrimRight(matches[1], ".")
				if dur, err := time.ParseDuration(raw); err == nil && dur > 0 {
					return clampResetDuration(dur), true
				}
			}
		}
	}

	// 2. Fallback regex on raw body string
	if matches := resetsInRegex.FindSubmatch(body); len(matches) > 1 {
		raw := strings.TrimRight(string(matches[1]), ".")
		if dur, err := time.ParseDuration(raw); err == nil && dur > 0 {
			return clampResetDuration(dur), true
		}
	}

	// 3. CodeBuddy date-time pattern: "<date> <clock> UTC+8" (or no offset).
	if tm, ok := codebuddyResetInstant(body); ok {
		// The instant is absolute, so the wait is the same whatever zone the
		// gateway runs in. The safety margin goes on top, and the ceiling is
		// the monthly one: a frequency limit on a MONTHLY quota can name a
		// reset days away.
		if dur := time.Until(tm) + resetSafetyMargin; dur > 0 {
			return clampCooldown(dur, maxMonthlyResetCooldown), true
		}
	}

	return 0, false
}

// codebuddyResetInstant returns the absolute instant a CodeBuddy quota body
// names: "<date> <clock> UTC±N", e.g.
// "your usage will reset at 2026-11-02 10:05:00 UTC+8".
//
// The offset is read out of the BODY, never from the host's zone.
// open-sse/executors/codebuddy-intl.js:376-391 builds the Date from the body's
// own offset for the same reason: the gateway runs WIB (UTC+7) while the
// provider prints UTC+8, so reading 10:05 as server-local puts the lock an
// hour early — the account is retried while it is still limited, and that
// retry is the automation signal that earns a ban.
//
// A body with no offset falls back to UTC+8, the provider's own clock
// (upstream's default branch). The safety margin is the caller's job: this
// answers when the quota refills, not when the next request is safe.
func codebuddyResetInstant(body []byte) (time.Time, bool) {
	matches := codebuddyResetDateRegex.FindSubmatch(body)
	if len(matches) < 3 {
		return time.Time{}, false
	}
	offset := 8 * 3600 // no offset in the body: the provider's own clock
	if len(matches) > 4 && len(matches[4]) > 0 {
		secs, ok := codebuddyOffsetSeconds(string(matches[3]), string(matches[4]))
		if !ok {
			return time.Time{}, false
		}
		offset = secs
	}
	// ParseInLocation with the body's own fixed offset yields an absolute
	// instant, so time.Local never enters the answer.
	tm, err := time.ParseInLocation("2006-01-02 15:04:05",
		string(matches[1])+" "+string(matches[2]), time.FixedZone("", offset))
	if err != nil {
		return time.Time{}, false
	}
	return tm, true
}

// codebuddyOffsetSeconds turns the offset digits of a CodeBuddy body
// ("8", "08:00", "5:30") into seconds east of UTC. A bare number is hours,
// which is how the provider writes UTC+8; the colon form is hours:minutes as
// some gateways print it. Out-of-range values are rejected rather than
// clamped, so a garbled body falls through to the generic cooldown instead of
// parking an account on a nonsense instant.
func codebuddyOffsetSeconds(sign, digits string) (int, bool) {
	hours, minutes := 0, 0
	if i := strings.IndexByte(digits, ':'); i >= 0 {
		h, hErr := strconv.Atoi(digits[:i])
		m, mErr := strconv.Atoi(digits[i+1:])
		if hErr != nil || mErr != nil {
			return 0, false
		}
		hours, minutes = h, m
	} else {
		h, hErr := strconv.Atoi(digits)
		if hErr != nil {
			return 0, false
		}
		hours = h
	}
	if hours > 18 || minutes > 59 {
		return 0, false
	}
	secs := hours*3600 + minutes*60
	if sign == "-" {
		secs = -secs
	}
	return secs, true
}

// clampCooldown keeps an announced wait inside [minResetCooldown, ceiling].
func clampCooldown(dur, ceiling time.Duration) time.Duration {
	if dur < minResetCooldown {
		return minResetCooldown
	}
	if dur > ceiling {
		return ceiling
	}
	return dur
}

func clampResetDuration(dur time.Duration) time.Duration {
	return clampCooldown(dur, maxResetCooldown)
}

// extractRetryAfter extracts a retryAfter ISO timestamp from an upstream error JSON body.
// Checks quotaResetDelay, "Resets in X", or common field names: retryAfter, retry_after, resetsAt, resets_at.
// Returns "" when not found or not parseable.
func extractRetryAfter(body []byte) string {
	if dur, ok := extractResetDuration(body); ok {
		return time.Now().UTC().Add(dur).Format(time.RFC3339)
	}
	var parsed struct {
		RetryAfter string `json:"retryAfter"`
		RetryAlt   string `json:"retry_after"`
		ResetsAt   string `json:"resetsAt"`
		ResetsAlt  string `json:"resets_at"`
		Error      struct {
			RetryAfter string `json:"retryAfter"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return ""
	}
	if parsed.RetryAfter != "" {
		return parsed.RetryAfter
	}
	if parsed.RetryAlt != "" {
		return parsed.RetryAlt
	}
	if parsed.ResetsAt != "" {
		return parsed.ResetsAt
	}
	if parsed.ResetsAlt != "" {
		return parsed.ResetsAlt
	}
	if parsed.Error.RetryAfter != "" {
		return parsed.Error.RetryAfter
	}
	return ""
}

// formatRetryAfter formats an ISO timestamp into a human-readable "reset after Xm Ys" string.
// Returns "" when the timestamp is empty, unparseable, or in the past.
func formatRetryAfter(isoTimestamp string) string {
	if isoTimestamp == "" {
		return ""
	}
	parsed, err := time.Parse(time.RFC3339, isoTimestamp)
	if err != nil {
		return ""
	}
	// time.Until is a Duration in nanoseconds. Dividing by 1000 yielded
	// milliseconds, so a 150s cooldown read as "reset after 41399h" and
	// contradicted the Retry-After header sent alongside it.
	remaining := time.Until(parsed)
	if remaining <= 0 {
		return "reset after 0s"
	}
	totalSec := int((remaining + time.Second - 1) / time.Second) // ceil
	h := totalSec / 3600
	m := (totalSec % 3600) / 60
	s := totalSec % 60
	var parts []string
	if h > 0 {
		parts = append(parts, fmt.Sprintf("%dh", h))
	}
	if m > 0 {
		parts = append(parts, fmt.Sprintf("%dm", m))
	}
	if s > 0 || len(parts) == 0 {
		parts = append(parts, fmt.Sprintf("%ds", s))
	}
	return "reset after " + strings.Join(parts, " ")
}

// claudeSessionIDFromBody reads the session id the cloak wrote into
// metadata.user_id, so it can be echoed in x-claude-code-session-id.
func claudeSessionIDFromBody(body []byte) string {
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		return ""
	}
	meta, _ := req["metadata"].(map[string]any)
	if meta == nil {
		return ""
	}
	userID, _ := meta["user_id"].(string)
	return extractClaudeSessionIdFromUserId(userID)
}

func isAccountAuthFailure(lower string) bool {
	return strings.Contains(lower, "no credentials") ||
		strings.Contains(lower, "invalid_grant") ||
		strings.Contains(lower, "invalid token") ||
		strings.Contains(lower, "token type is not supported") ||
		strings.Contains(lower, "authentication expired") ||
		strings.Contains(lower, "token expired") ||
		strings.Contains(lower, "account suspended") ||
		strings.Contains(lower, "account disabled") ||
		strings.Contains(lower, "incorrect api key") ||
		strings.Contains(lower, "invalid api key") ||
		strings.Contains(lower, "organization is not supported") ||
		strings.Contains(lower, "insufficient funds for organization")
}

// isModelQuotaText reports a refusal that names one model's quota rather than
// the account's standing. These lock the model, not the connection: a Qoder
// account whose paid models are out of credits still serves its free ones, and
// parking the whole connection answered `dfmodel` with "all in cooldown" after
// a single `dmodel` refusal.
func isModelQuotaText(errorText string) bool {
	upper := strings.ToUpper(errorText)
	return strings.Contains(upper, "QUOTA_EXHAUSTED") ||
		strings.Contains(errorText, "Individual quota reached") ||
		strings.Contains(upper, "MODEL_CAPACITY_EXHAUSTED") ||
		// Qoder's billing envelope: code 110/112 or a pricingUrl field
		// (qoderIsBillingBlock). The refusal is per model — the account's
		// free models keep working while a paid one is out of credits.
		strings.Contains(upper, "PRICINGURL") ||
		strings.Contains(upper, "PRICING_URL") ||
		// Qoder's queue window (qoderQueuedTimeout): the model is briefly out
		// of capacity, which throttles that model alone. Locking the account
		// here parked every other model on it behind one busy queue.
		strings.Contains(upper, "STILL QUEUED")
}

func mentionsGate(lower string) bool {
	return strings.Contains(lower, "is not supported") ||
		strings.Contains(lower, "not supported") ||
		strings.Contains(lower, "model access is disabled") ||
		strings.Contains(lower, "endpoint is unavailable")
}

// isModelScopedError reports whether an upstream retryable error is
// specific to the requested model (e.g. 429 model quota, 402 insufficient funds
// for paid models, or 401 "model is not supported") rather than an account-scoped
// credential failure. For model-scoped errors, only LockConnectionModel and
// RecordConnectionError are set so that unrelated healthy models on the same account
// remain available.
// isOpencodeFreeTierRefusal reports whether an upstream failure is the OpenCode
// free-tier gate refusing to serve this traffic, rather than a problem with the
// account, key or request.
//
// The gate answers in two shapes, and both mean the same thing — it is not
// serving this egress at that moment:
//
//   - 403 with a body whose type is FreeTierError, when the request does not
//     look like the official client.
//   - 429 with a body whose type is FreeUsageLimitError ("Free usage limit
//     reached. Subscribe to OpenCode Go to keep going"), when the free budget
//     accounted to this egress is spent.
//
// Neither says anything about the caller's credentials, and both are read as a
// verdict on the request by every client — so neither may be forwarded verbatim;
// see opencodeClosureError. The 429 in particular must not cool the account
// down: the account is healthy, the egress is not.
//
// The provider check keeps every other provider's 403/429 handling untouched,
// and the body check keeps a genuine OpenCode auth or billing error on the
// normal path.
func isOpencodeFreeTierRefusal(provider string, ue *upstreamError) bool {
	switch provider {
	case "opencode", "opencode-zen", "opencode-go":
	default:
		return false
	}
	if ue == nil {
		return false
	}
	// Match the error type, not the prose: the gate's message text has already
	// been reworded once.
	switch ue.StatusCode {
	case http.StatusForbidden:
		return bytes.Contains(ue.Body, []byte("FreeTierError"))
	case http.StatusTooManyRequests:
		return bytes.Contains(ue.Body, []byte("FreeUsageLimitError"))
	}
	return false
}

func isModelScopedError(statusCode int, errorText string, model string, bodies ...[]byte) bool {
	if model == "" {
		return false
	}
	lower := strings.ToLower(errorText)
	if isAccountAuthFailure(lower) {
		return false
	}
	// An OpenCode free-tier refusal is an egress verdict, not a model gate:
	// the failover loop answers it by rotating the proxy pool, so it must not
	// be recorded as a model-scoped lock. Matched on the body's error type —
	// the prose is not stable enough to key on, and every other 403 keeps the
	// existing cooldown-only handling (it never rotates a pool). The 429 shape
	// (FreeUsageLimitError) is caught by isOpencodeFreeTierRefusal before this
	// classifier runs; the 403 shape is kept here so a caller that reaches this
	// function directly still never treats the refusal as a model lock.
	if statusCode == http.StatusForbidden {
		for _, body := range bodies {
			if bytes.Contains(body, []byte("FreeTierError")) {
				return false
			}
		}
	}

	// 1. Quota exhaustion markers (Antigravity Claude, etc.) on 429/403/503
	if statusCode == http.StatusTooManyRequests || statusCode == http.StatusForbidden || statusCode == http.StatusServiceUnavailable {
		if isModelQuotaText(errorText) {
			return true
		}
	}

	// 2. Model-gate verdicts only ever arrive on 400/401/402/403.
	// A 5xx that merely says "not supported" is a node fault, not a model gate.
	switch statusCode {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden:
	default:
		return false
	}

	// 402 Insufficient account funds on providers serving tiered models (e.g. Zen paid models)
	if statusCode == http.StatusPaymentRequired {
		if (strings.Contains(lower, "insufficient account funds") || strings.Contains(lower, "insufficient funds")) &&
			!strings.Contains(lower, "credits are exhausted") && !strings.Contains(lower, "spending limit") {
			return true
		}
	}

	// 401/400/403: Body must name the requested model AND mention that the model is unsupported/disabled
	cleanModel := strings.ToLower(model)
	if idx := strings.LastIndex(cleanModel, "/"); idx != -1 {
		cleanModel = cleanModel[idx+1:]
	}
	if (strings.Contains(lower, cleanModel) || strings.Contains(lower, strings.ToLower(model))) && mentionsGate(lower) {
		return true
	}

	return false
}

// tokenSaverBypassKey carries the token-saver opt-out on the context.
// Port of upstream fallback.go: X-PanRouter-Token-Saver: off|0|false|bypass.
type tokenSaverBypassKey struct{}

// WithTokenSaverBypass marks ctx as a token-saver bypass: no compression or
// prompt injection may run on this request.
func WithTokenSaverBypass(ctx context.Context) context.Context {
	return context.WithValue(ctx, tokenSaverBypassKey{}, true)
}

// TokenSaverBypassEnabled reports whether the request opted out of token saving.
func TokenSaverBypassEnabled(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, ok := ctx.Value(tokenSaverBypassKey{}).(bool)
	return ok && v
}

// isTokenSaverBypassValue reports whether an X-PanRouter-Token-Saver header value
// opts the request out of every token saver. The spellings mirror a boolean
// switch, so "off" and "0" mean bypass, not "enable".
func isTokenSaverBypassValue(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "false", "0", "off", "bypass":
		return true
	default:
		return false
	}
}

// claudeCacheTTL is the window the Claude head anchors ask for, read from the
// operator's setting rather than fixed, so a 1h cache is a config change and
// not a rebuild. The chat handler holds the saver config; a handler without one
// (tests) reads as the 5m default.
func (h *ChatHandler) claudeCacheTTL() string {
	if h.TokenSaver == nil {
		return translator.ClaudeCacheTTL5m
	}
	return h.TokenSaver.ClaudeCacheTTL()
}

// recordPersonaInjection accounts for a persona/system-prompt injection that
// added tokens: the saving numbers are recomputed from the lengths so the
// reported saving stays truthful even when the saver made the request bigger.
func recordPersonaInjection(origTokens, savedTokens *int, beforeLen, afterLen int) {
	if afterLen <= beforeLen {
		return
	}
	added := (afterLen - beforeLen) / 4
	if added <= 0 {
		return
	}
	*origTokens += added
	*savedTokens -= added
	if *savedTokens < 0 {
		*savedTokens = 0
	}
}

// opencodeCarrierRetryAfter reads a Retry-After header in either form the HTTP
// spec allows: delta-seconds, or an HTTP-date whose distance from now is the
// wait. It reports ok=false when the header is absent or unreadable.
func opencodeCarrierRetryAfter(h http.Header, now time.Time) (time.Duration, bool) {
	raw := strings.TrimSpace(h.Get("Retry-After"))
	if raw == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(raw); err == nil {
		if secs <= 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	if when, err := http.ParseTime(raw); err == nil {
		if wait := when.Sub(now); wait > 0 {
			return wait, true
		}
	}
	return 0, false
}
