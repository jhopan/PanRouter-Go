package chat

import (
	"context"
	"io"
	"net/http"

	"panrouter/proxy/internal/log"
	"panrouter/proxy/internal/providers"
)

// antigravityExperimentBodyLimit caps the config read. The captured payload is
// ~82 KB; the limit is a guard against a runaway answer, not a shape.
const antigravityExperimentBodyLimit = 4 << 20

// RefreshAntigravityExperiments reads the account's `generic-system-prompt`
// decision out of its AICode config, so the identity block this lane sends is the
// one that account's own client would send.
//
// Called from the quota refresh, which already runs with the connection's token
// and client. The read itself is throttled twice: the caller's own 30s quota
// throttle, and the 6h TTL the stored decision carries — `NeedsExperimentRead`
// answers for the second, so a steady-state refresh buys no extra upstream call.
//
// A failed read is recorded as unknown with a short TTL rather than as false: the
// caller then keeps sending the generic block, which is this port's long-standing
// behaviour and the branch the captured accounts resolve to.
func RefreshAntigravityExperiments(ctx context.Context, client *http.Client, accessToken string) {
	if accessToken == "" || client == nil || !providers.AntigravityNeedsExperimentRead(accessToken) {
		return
	}

	// The captured request is a GET with no body — the config is answered from the
	// authenticated identity alone. A POST answers 404, which is how this call
	// originally failed and left every account on the generic branch.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		providers.AntigravityAICodeBaseURL+"/v1alpha:fetchConfig", nil)
	if err != nil {
		providers.StoreAntigravityGenericSystemPrompt(accessToken, true, false)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("User-Agent", providers.AntigravityUserAgent())

	resp, err := client.Do(req)
	if err != nil {
		providers.StoreAntigravityGenericSystemPrompt(accessToken, true, false)
		return
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, antigravityExperimentBodyLimit))
	if readErr != nil || resp.StatusCode != http.StatusOK {
		providers.StoreAntigravityGenericSystemPrompt(accessToken, true, false)
		// 403 is expected when the token does not carry the aicode scope
		// (e.g. OAuth grants from older login flows). It is not actionable:
		// the code already falls back to the generic system prompt, so this
		// is a debug-level observation, not a warning (parity: the upstream
		// never surfaces this 403 to the operator either).
		if resp.StatusCode == http.StatusForbidden {
			log.Debug("ag_quota", "identity config read: no aicode scope (403), using generic system prompt")
		} else {
			log.Warn("ag_quota", "identity config read failed", "status", resp.StatusCode)
		}
		return
	}

	generic, known := providers.ParseAntigravityGenericSystemPrompt(body)
	providers.StoreAntigravityGenericSystemPrompt(accessToken, generic, known)
	if known {
		log.Info("ag_quota", "identity config read", "genericSystemPrompt", generic)
	}
}
