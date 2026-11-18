package providers

import (
	"testing"
)

// TestParseAntigravityGenericSystemPrompt pins the read of the identity flag out
// of the AICode config, whose captured shape is
// {"experimentConfig":{"experimentFlags":[{"name":…,"boolValue":…}]}}.
func TestParseAntigravityGenericSystemPrompt(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantFlags bool
		wantKnown bool
	}{
		{
			name:      "flag present and true",
			body:      `{"experimentConfig":{"experimentFlags":[{"name":"enable-hook-status","boolValue":true},{"name":"generic-system-prompt","boolValue":true}]}}`,
			wantFlags: true,
			wantKnown: true,
		},
		{
			name:      "flag present and false",
			body:      `{"experimentConfig":{"experimentFlags":[{"name":"generic-system-prompt","boolValue":false}]}}`,
			wantFlags: false,
			wantKnown: true,
		},
		{
			name:      "flag absent",
			body:      `{"experimentConfig":{"experimentFlags":[{"name":"other","boolValue":true}]}}`,
			wantFlags: true,
			wantKnown: false,
		},
		{
			name:      "flag carries no boolean",
			body:      `{"experimentConfig":{"experimentFlags":[{"name":"generic-system-prompt","stringValue":"yes"}]}}`,
			wantFlags: true,
			wantKnown: false,
		},
		{
			name:      "not a config payload",
			body:      `{"error":"nope"}`,
			wantFlags: true,
			wantKnown: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			generic, known := ParseAntigravityGenericSystemPrompt([]byte(tt.body))
			if generic != tt.wantFlags || known != tt.wantKnown {
				t.Errorf("ParseAntigravityGenericSystemPrompt() = (%v, %v), want (%v, %v)",
					generic, known, tt.wantFlags, tt.wantKnown)
			}
		})
	}
}

// TestAntigravityGenericSystemPromptCache pins the contract the forwarding path
// depends on: an account nobody has read answers true (the identity this port has
// always sent), a stored answer wins, and a lapsed one falls back to true.
func TestAntigravityGenericSystemPromptCache(t *testing.T) {
	ResetAntigravityExperiments()

	const token = "ya29.test-token"
	if !AntigravityGenericSystemPromptForToken(token) {
		t.Error("an unknown account must answer true")
	}
	if !AntigravityNeedsExperimentRead(token) {
		t.Error("an unknown account must need a read")
	}

	StoreAntigravityGenericSystemPrompt(token, false, true)
	if AntigravityGenericSystemPromptForToken(token) {
		t.Error("a stored false must win")
	}
	if AntigravityNeedsExperimentRead(token) {
		t.Error("a fresh answer must not need another read")
	}

	// A different token is a different account.
	if !AntigravityGenericSystemPromptForToken("ya29.other") {
		t.Error("another account must not inherit the answer")
	}

	// An empty token — a lane with no credential — answers true, never false.
	if !AntigravityGenericSystemPromptForToken("") {
		t.Error("an empty token must answer true")
	}

	// A failed read is unknown, not a false verdict.
	StoreAntigravityGenericSystemPrompt("ya29.failed", true, false)
	if !AntigravityGenericSystemPromptForToken("ya29.failed") {
		t.Error("a failed read must keep the generic identity")
	}
}
