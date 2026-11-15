package providers

import (
	"strings"
	"testing"
)

func TestNormalizeAntigravityClientProfile(t *testing.T) {
	tests := []struct {
		name  string
		input any
		want  AntigravityClientProfile
	}{
		{name: "ide", input: "ide", want: AntigravityProfileIDE},
		{name: "cli", input: "cli", want: AntigravityProfileCLI},
		{name: "uppercase cli", input: "CLI", want: AntigravityProfileCLI},
		{name: "whitespace", input: "  cli  ", want: AntigravityProfileCLI},
		{name: "legacy harness", input: "harness", want: AntigravityProfileCLI},
		{name: "legacy sdk", input: "sdk", want: AntigravityProfileCLI},
		{name: "empty string", input: "", want: AntigravityProfileIDE},
		{name: "unknown value", input: "something-else", want: AntigravityProfileIDE},
		{name: "nil", input: nil, want: AntigravityProfileIDE},
		{name: "non-string", input: 42, want: AntigravityProfileIDE},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NormalizeAntigravityClientProfile(tt.input); got != tt.want {
				t.Errorf("NormalizeAntigravityClientProfile(%v) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestIsAntigravityClientProfile(t *testing.T) {
	valid := []string{"ide", "cli", "harness", "sdk", "IDE", "CLI"}
	for _, v := range valid {
		if !IsAntigravityClientProfile(v) {
			t.Errorf("IsAntigravityClientProfile(%q) = false, want true", v)
		}
	}
	invalid := []any{"", "unknown", nil, 42, " id "}
	for _, v := range invalid {
		if IsAntigravityClientProfile(v) {
			t.Errorf("IsAntigravityClientProfile(%v) = true, want false", v)
		}
	}
}

// TestAntigravityUserAgentFor pins the identity contract after the capture.
//
// The profile used to choose between two strings (an ide shape and the agy CLI
// one). PanRouter's 2026-11-14 capture shows the shipped client sends a single
// value — antigravity/hub/2.32.1 (aidev_client; os_type=…; arch=…;
// cl=1021296516) — so a profile that changed the header would present a
// fingerprint no real client has. The profile still exists because other
// behaviour is keyed on it; the header no longer varies.
func TestAntigravityUserAgentFor(t *testing.T) {
	want := AntigravityUserAgent()
	for _, profile := range []AntigravityClientProfile{AntigravityProfileIDE, AntigravityProfileCLI, "bogus"} {
		if got := AntigravityUserAgentFor(profile); got != want {
			t.Errorf("AntigravityUserAgentFor(%q) = %q, want the one captured identity %q", profile, got, want)
		}
	}

	// The captured shape, not merely a non-empty string.
	prefix := "antigravity/hub/" + AntigravityHubVersion + " (aidev_client; os_type="
	if !strings.HasPrefix(want, prefix) || !strings.HasSuffix(want, "; cl="+AntigravityClientLabel+")") {
		t.Errorf("default identity = %q, want the captured hub descriptor %q…", want, prefix)
	}

	// The escape hatch restores the pre-capture CLI string, which is what makes an
	// A/B test on a live account possible without rebuilding.
	t.Setenv("ANTIGRAVITY_IDENTITY", AntigravityIdentityCLI)
	legacy := AntigravityUserAgent()
	if !strings.HasPrefix(legacy, "antigravity/cli/") {
		t.Errorf("ANTIGRAVITY_IDENTITY=cli produced %q, want the legacy cli identity", legacy)
	}
	if legacy == want {
		t.Error("the override did not change the identity")
	}
}

func TestAntigravityClientProfileString(t *testing.T) {
	if got := AntigravityProfileCLI.String(); got != "cli" {
		t.Errorf("String() = %q, want cli", got)
	}
	if got := AntigravityProfileIDE.String(); got != "ide" {
		t.Errorf("String() = %q, want ide", got)
	}
}

// TestAntigravityIdentityOverride pins which values the escape hatch accepts.
//
// The variable is this port's own (the reference has one identity and no
// switch), so a typo must not silently invent a third identity: only the two
// known names count, and anything else leaves the captured Hub value in place.
func TestAntigravityIdentityOverride(t *testing.T) {
	tests := []struct {
		value string
		want  string
	}{
		{"", ""},
		{"hub", AntigravityIdentityHub},
		{"HUB", AntigravityIdentityHub},
		{" cli ", AntigravityIdentityCLI},
		{"ide", ""},
		{"garbage", ""},
	}
	for _, tc := range tests {
		t.Setenv("ANTIGRAVITY_IDENTITY", tc.value)
		if got := AntigravityIdentityOverride(); got != tc.want {
			t.Errorf("AntigravityIdentityOverride() with %q = %q, want %q", tc.value, got, tc.want)
		}
		if tc.want == "" && !strings.HasPrefix(AntigravityUserAgent(), "antigravity/hub/") {
			t.Errorf("identity %q changed the User-Agent away from the captured hub value", tc.value)
		}
	}
}
