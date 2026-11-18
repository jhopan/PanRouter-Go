package providers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"time"
)

// AntigravityAccounts resolve the identity block they send through an experiment,
// not a constant: the shipped client's system-instruction template switches on
// `ExperimentEnabled "generic-system-prompt"`, and that flag arrives per account
// in the authenticated AICode config it fetches at startup
// (aicode.googleapis.com/v1alpha:fetchConfig → experimentConfig.experimentFlags).
//
// Captured live on 2026-11-18: 417 flags, `generic-system-prompt` = true for the
// account used, which is the branch this port sends by default. The machinery
// exists so an account that resolves to false is not answered with the other
// account's identity text.
//
// The cache is keyed by a hash of the connection's access token because that is
// what the forwarding path already carries: threading a connection id down to
// the wrap call would change a signature every provider shares. The token is
// never stored — only its digest, so a rotated token misses and refetches.
const (
	// AntigravityAICodeBaseURL serves the client config (and its experiment flags).
	AntigravityAICodeBaseURL = "https://aicode.googleapis.com"
	// AntigravityGenericSystemPromptFlag is the flag whose value picks the identity block.
	AntigravityGenericSystemPromptFlag = "generic-system-prompt"

	agExperimentTTL        = 6 * time.Hour
	agExperimentFailureTTL = 15 * time.Minute
	agExperimentMaxSlots   = 4096
)

type agExperimentEntry struct {
	generic bool
	expires time.Time
}

var (
	agExperimentMu      sync.Mutex
	agExperimentByToken = make(map[string]agExperimentEntry)
)

// agExperimentKey digests an access token so the cache never holds the secret.
func agExperimentKey(accessToken string) string {
	if accessToken == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(accessToken))
	return hex.EncodeToString(sum[:8])
}

// StoreAntigravityGenericSystemPrompt records an account's identity decision for
// as long as the flag is worth trusting. known=false records a failed read with a
// short TTL, so a transient error is retried soon but not on every request.
func StoreAntigravityGenericSystemPrompt(accessToken string, generic, known bool) {
	key := agExperimentKey(accessToken)
	if key == "" {
		return
	}
	entry := agExperimentEntry{generic: generic, expires: time.Now().Add(agExperimentTTL)}
	if !known {
		entry = agExperimentEntry{generic: true, expires: time.Now().Add(agExperimentFailureTTL)}
	}
	agExperimentMu.Lock()
	defer agExperimentMu.Unlock()
	if _, present := agExperimentByToken[key]; !present && len(agExperimentByToken) >= agExperimentMaxSlots {
		now := time.Now()
		for k, v := range agExperimentByToken {
			if !v.expires.After(now) {
				delete(agExperimentByToken, k)
			}
		}
	}
	if _, present := agExperimentByToken[key]; present || len(agExperimentByToken) < agExperimentMaxSlots {
		agExperimentByToken[key] = entry
	}
}

// AntigravityNeedsExperimentRead reports whether the account's decision is
// unknown or stale, so the caller can fetch it on the path that already holds the
// token.
func AntigravityNeedsExperimentRead(accessToken string) bool {
	key := agExperimentKey(accessToken)
	if key == "" {
		return false
	}
	agExperimentMu.Lock()
	defer agExperimentMu.Unlock()
	entry, ok := agExperimentByToken[key]
	return !ok || !entry.expires.After(time.Now())
}

// AntigravityGenericSystemPromptForToken reports the account's decision. An
// unknown or stale account answers true — the identity this port has always sent,
// and the branch the captured accounts resolve to.
func AntigravityGenericSystemPromptForToken(accessToken string) bool {
	key := agExperimentKey(accessToken)
	if key == "" {
		return true
	}
	agExperimentMu.Lock()
	defer agExperimentMu.Unlock()
	entry, ok := agExperimentByToken[key]
	if !ok || !entry.expires.After(time.Now()) {
		return true
	}
	return entry.generic
}

// ResetAntigravityExperiments clears the cache. It exists for tests.
func ResetAntigravityExperiments() {
	agExperimentMu.Lock()
	defer agExperimentMu.Unlock()
	agExperimentByToken = make(map[string]agExperimentEntry)
}

// ParseAntigravityGenericSystemPrompt reads the flag out of a fetchConfig body.
//
// The payload is `{"experimentConfig":{"experimentFlags":[{"name":…,"boolValue":…}]}}`.
// A body that does not carry the flag, or carries it with no boolean, returns
// known=false rather than guessing a default: the caller decides what an absent
// answer means, and for this flag that is the generic block.
func ParseAntigravityGenericSystemPrompt(body []byte) (generic bool, known bool) {
	var payload struct {
		ExperimentConfig struct {
			ExperimentFlags []struct {
				Name      string `json:"name"`
				BoolValue *bool  `json:"boolValue"`
			} `json:"experimentFlags"`
		} `json:"experimentConfig"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return true, false
	}
	for _, flag := range payload.ExperimentConfig.ExperimentFlags {
		if strings.EqualFold(flag.Name, AntigravityGenericSystemPromptFlag) && flag.BoolValue != nil {
			return *flag.BoolValue, true
		}
	}
	return true, false
}
