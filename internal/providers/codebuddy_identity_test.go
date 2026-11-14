package providers

import (
	"encoding/base64"
	"strings"
	"testing"
)

// The CodeBuddy Intl registry entry and the shared CLI-identity module must
// agree: the intl lane advertises CLI 2.178.0 for chat and for OAuth, not the
// old "IDE/2.160.0" one that made the gateway see two clients behind one
// account.
// (upstream tests/unit/codebuddy-intl-cli-identity.test.js, "advertises the CLI
// user agent everywhere, not the IDE one")

func TestCodeBuddyIntlRegistry_AdvertisesTheCLIUserAgent(t *testing.T) {
	cfg, ok := KnownProviders["codebuddy-intl"]
	if !ok {
		t.Fatal("codebuddy-intl is missing from the registry")
	}
	for header, want := range map[string]string{
		"User-Agent":    "CLI/2.178.0 CodeBuddy/2.178.0",
		"X-IDE-Type":    "CLI",
		"X-IDE-Name":    "CLI",
		"X-IDE-Version": "2.178.0",
		"X-Product":     "SaaS",
	} {
		if got := cfg.StaticHeaders[header]; got != want {
			t.Errorf("codebuddy-intl %s = %q, want %q", header, got, want)
		}
	}
	if CodeBuddyUserAgent != "CLI/2.178.0 CodeBuddy/2.178.0" {
		t.Errorf("CodeBuddyUserAgent = %q", CodeBuddyUserAgent)
	}
	if CodeBuddySDKVersion != "7.39.0" || CodeBuddyNodeVersion != "v26.7.0" {
		t.Errorf("Stainless SDK/Node = %q/%q, want 7.39.0/v26.7.0", CodeBuddySDKVersion, CodeBuddyNodeVersion)
	}
}

func TestCodeBuddyIntlRegistry_NoIDEIdentityLeft(t *testing.T) {
	cfg := KnownProviders["codebuddy-intl"]
	for header, value := range cfg.StaticHeaders {
		if strings.Contains(value, "IDE/") {
			t.Errorf("codebuddy-intl %s = %q still advertises an IDE client", header, value)
		}
	}
	if got := KnownOAuthConfigs["codebuddy-intl"].TokenURL; strings.Contains(got, "tencent.com") {
		t.Errorf("codebuddy-intl refresh endpoint = %q, want the intl host", got)
	}
}

func TestCodeBuddyUserIDFromToken(t *testing.T) {
	jwt := func(sub string) string {
		return "eyJhbGciOiJIUzI1NiJ9." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"`+sub+`"}`)) + ".sig"
	}
	const sub = "85482e42-2454-468d-a2b0-f1f6311f0f81"

	tests := []struct {
		name  string
		token string
		want  string
	}{
		{name: "sub claim", token: jwt(sub), want: sub},
		{name: "padded base64url payload", token: "eyJhbGciOiJIUzI1NiJ9." + base64.URLEncoding.EncodeToString([]byte(`{"sub":"`+sub+`"}`)) + ".sig", want: sub},
		{name: "opaque token", token: "sk-opaque", want: ""},
		{name: "two segments", token: "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4In0", want: ""},
		{name: "empty", token: "", want: ""},
		{name: "payload without sub", token: "eyJhbGciOiJIUzI1NiJ9." + base64.RawURLEncoding.EncodeToString([]byte(`{"email":"a@b.c"}`)) + ".sig", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CodeBuddyUserIDFromToken(tt.token); got != tt.want {
				t.Errorf("CodeBuddyUserIDFromToken(%q) = %q, want %q", tt.token, got, tt.want)
			}
		})
	}
}
