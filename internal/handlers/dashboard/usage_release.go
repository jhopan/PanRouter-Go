package dashboard

import (
	"context"
	json "encoding/json/v2"
	"math"
	"net/http"
	"strings"

	"panrouter/proxy/internal/handlerutil"
	"panrouter/proxy/internal/models"
)

// POST /api/usage/{connectionId}/check-release
//
// "Check & release": read the provider's live quota and, if the account is no
// longer exhausted, lift the block so the account is usable again.
//
// Why this exists: a MiniMax plan block lasts 24 hours, so a top-up (or a daily
// check-in that refills the plan) would otherwise leave the account unusable for
// the rest of the day even though the provider is serving it again.
//
// It is deliberately NOT "unblock on demand": the quota is verified first, so
// the button cannot be used to force a request against an exhausted account.
// The provider's own answer decides.
//
// Fail-open by contract: a tracker error leaves the block in place and reports
// the error, because releasing an exhausted account would only produce another
// 429.

// releaseResult is what one check decided.
type releaseResult struct {
	released bool
	reason   string
	quotas   map[string]any
}

// HandleCheckRelease handles POST /api/usage/{connectionId}/check-release.
func (h *DashboardHandler) HandleCheckRelease(w http.ResponseWriter, r *http.Request) {
	connID := getURLParam(r, "connectionId")
	if connID == "" {
		connID = getURLParam(r, "id")
	}
	if connID == "" {
		writePlainError(w, http.StatusNotFound, "Connection not found")
		return
	}

	conn, err := h.Repo.GetProviderConnectionByID(connID)
	if err != nil {
		writePlainError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if conn == nil {
		writePlainError(w, http.StatusNotFound, "Connection not found")
		return
	}

	result, err := h.checkAndRelease(r.Context(), conn)
	if err != nil {
		writePlainError(w, http.StatusInternalServerError, "Check failed: "+err.Error())
		return
	}
	// quotas is an untyped nil when the provider returned no reading, so the
	// key is JSON null exactly as upstream's `quotas: usage?.quotas ?? null`
	// produces. A nil map would encode as `{}` under this runtime's encoder,
	// which would read as "the provider reported zero quotas".
	var quotas any
	if result.quotas != nil {
		quotas = result.quotas
	}
	handlerutil.WriteJSON(w, http.StatusOK, map[string]any{
		"released": result.released,
		"reason":   result.reason,
		"quotas":   quotas,
	})
}

// checkAndRelease reads the provider quota and decides whether the account is
// usable again. The returned error is a write failure — the decision itself is
// never an error (fail-open).
func (h *DashboardHandler) checkAndRelease(ctx context.Context, conn *models.ProviderConnection) (releaseResult, error) {
	data := map[string]any{}
	if strings.TrimSpace(conn.Data) != "" {
		_ = json.Unmarshal([]byte(conn.Data), &data)
	}
	provider := conn.Provider

	// ── MiniMax Code ─────────────────────────────────────────────────────────
	if provider == "minimax-code" || provider == "minimax-code-global" {
		accessToken, _, psd := usageCreds(data)
		usage := fetchMiniMaxCodeUsage(ctx, provider, accessToken, psdStr(psd, "realUserID"))
		if usage.message != "" {
			return releaseResult{reason: usage.message}, nil
		}
		if len(usage.quotas) == 0 {
			return releaseResult{reason: "The tracker returned no quota rows"}, nil
		}
		// The provider is serving this account again — lift every block.
		if err := h.Repo.ReleaseConnectionBlocks(conn.ID); err != nil {
			return releaseResult{}, err
		}
		return releaseResult{
			released: true,
			reason:   "The plan has quota again — the account is usable",
			quotas:   usage.quotas,
		}, nil
	}

	// ── Antigravity ──────────────────────────────────────────────────────────
	// An account Google flagged as ineligible (VALIDATION_REQUIRED) needs a
	// browser re-verification. The provider decides whether the flag is gone:
	// while it is still set the quota read answers with a message, and this
	// branch leaves the account alone.
	if provider == "antigravity" {
		accessToken, _, _ := usageCreds(data)
		projectID, _ := data["projectId"].(string)
		if strings.TrimSpace(accessToken) == "" {
			return releaseResult{reason: "Usage API unavailable — the account was left in place"}, nil
		}
		usage := fetchAntigravityDashboardUsage(ctx, accessToken, projectID)
		if usage.message != "" {
			return releaseResult{reason: usage.message, quotas: usage.quotas}, nil
		}
		if err := h.Repo.ReleaseConnectionBlocks(conn.ID); err != nil {
			return releaseResult{}, err
		}
		return releaseResult{
			released: true,
			reason:   "The account is eligible again — it is usable",
			quotas:   usage.quotas,
		}, nil
	}

	// ── Generic path: any provider with a usage API ──────────────────────────
	usage, ok := fetchProviderUsage(ctx, provider, data)
	if !ok || usage.message != "" {
		reason := usage.message
		if reason == "" {
			reason = "Usage API not implemented for this provider"
		}
		return releaseResult{reason: reason}, nil
	}

	if quotasExhausted(usage.quotas) {
		return releaseResult{
			reason: "The provider still reports this account as exhausted",
			quotas: usage.quotas,
		}, nil
	}

	if err := h.Repo.ReleaseConnectionBlocks(conn.ID); err != nil {
		return releaseResult{}, err
	}
	return releaseResult{
		released: true,
		reason:   "The provider reports quota available — the account is usable",
		quotas:   usage.quotas,
	}, nil
}

// quotasExhausted reports whether any quota row says the account has nothing
// left. A credit-balance row is judged by its total; every other row by its
// remaining percentage (or its remaining count, which some providers report
// instead).
func quotasExhausted(quotas map[string]any) bool {
	for _, raw := range quotas {
		row, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if credit, isBool := row["isCreditBalance"].(bool); isBool && credit {
			if total, ok := quotaNumber(row["total"]); ok {
				if total <= 0 {
					return true
				}
			}
			continue
		}
		pct, ok := quotaNumber(row["remainingPercentage"])
		if !ok {
			pct, ok = quotaNumber(row["remaining"])
		}
		if ok && pct <= 0 {
			return true
		}
	}
	return false
}

// quotaNumber reads a numeric quota field. A non-finite value is not a zero:
// an absent reading must never be read as exhaustion, which is why the caller
// treats the second return as "there is a number here".
func quotaNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, !math.IsNaN(n) && !math.IsInf(n, 0)
	case float32:
		f := float64(n)
		return f, !math.IsNaN(f) && !math.IsInf(f, 0)
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}
