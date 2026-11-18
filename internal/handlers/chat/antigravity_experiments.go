package chat

import (
	"bytes"
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

	// The captured request carries no body — the config is answered from the
	// authenticated identity alone.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		providers.AntigravityAICodeBaseURL+"/v1alpha:fetchConfig", bytes.NewReader(nil))
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
		log.Warn("ag_quota", "identity config read failed", "status", resp.StatusCode)
		return
	}

	generic, known := providers.ParseAntigravityGenericSystemPrompt(body)
	providers.StoreAntigravityGenericSystemPrompt(accessToken, generic, known)
	if known {
		log.Info("ag_quota", "identity config read", "genericSystemPrompt", generic)
	}
}
