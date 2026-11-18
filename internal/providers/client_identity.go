package providers

import (
	"os"
	"runtime"
	"strings"
)

// Upstream ships one version constant per client family and derives every
// identity header from it (open-sse/providers/registry/codex.js CODEX_CLI_VERSION,
// open-sse/config/grokCli.js GROK_CLI_VERSION). These are the same two values,
// so the Go port has exactly one literal per family instead of one per call
// site — the previous layout hardcoded "0.2.99" and "0.2.93" in six files,
// which is how the grok-cli proxy ended up rejecting every request with
// HTTP 426 once cli-chat-proxy required 1.0.13.
const (
	// CodexCLIVersion is the codex CLI identity OpenAI's backend sees.
	CodexCLIVersion = "0.167.1"
	// CodexCLIUserAgent is the User-Agent header the codex backend expects.
	CodexCLIUserAgent = "codex_cli_rs/" + CodexCLIVersion
	// CodexCLIVersionHeader carries the same value in the `version` header,
	// which upstream sends alongside the User-Agent. Omitting it is what
	// leaves newer Codex models gated (upstream decolua/9router ca6e8407).
	CodexCLIVersionHeader = CodexCLIVersion

	// GrokCLIVersion is the @xai-official/grok identity cli-chat-proxy.grok.com
	// validates; anything below GrokCLIMinimumVersion is refused with HTTP 426.
	GrokCLIVersion = "1.0.44"
	// GrokCLIMinimumVersion is that floor, kept beside the shipped version so a
	// bump that reintroduces an older identity fails loudly instead of at
	// runtime against the proxy.
	GrokCLIMinimumVersion = "1.0.13"
	// GrokCLIUserAgent is the inference-path identity (grok-shell).
	GrokCLIUserAgent = "grok-shell/" + GrokCLIVersion + " (linux; x86_64)"
	// GrokCLIPagerUserAgent is the billing/probe identity. Upstream keeps a
	// separate pager UA because those calls go through the pager client, not
	// the inference one (open-sse/config/grokCli.js GROK_CLI_PAGER_USER_AGENT).
	GrokCLIPagerUserAgent = "grok-pager/" + GrokCLIVersion + " grok-shell/" + GrokCLIVersion + " (linux; x86_64)"

	// GrokCLIClientIdentifier is the x-grok-client-identifier sent by the
	// inference path; the pager paths below use GrokCLIPagerIdentifier.
	GrokCLIClientIdentifier = "grok-shell"
	// GrokCLIPagerIdentifier is the identifier the probe and OAuth paths send.
	GrokCLIPagerIdentifier = "grok-pager"

// Antigravity identity. ONE captured value, not an assembly of guesses:
	//
	//   antigravity/hub/2.32.2 (aidev_client; os_type=<host>; arch=<host>; cl=1022991777)
	//
	// PanRouter's 2026-11-14 capture ran the shipped Hub client
	// (language_server.exe) with the Hub launcher's argument set —
	// `--subclient_type hub --override_ide_version 2.32.1` — and read
	// `antigravity/hub/2.32.1 (aidev_client; ...; cl=1021296516)`, which this port
	// shipped as the default identity.
	//
	// The Hub moved. A MITM capture of the LIVE client on 2026-11-18 (app 44.9.1,
	// language_server.exe 215543296 bytes, one real turn) reads
	// `antigravity/hub/2.32.2 (aidev_client; os_type=windows; arch=amd64;
	// cl=1022991777)` — the same shape with both numbers advanced. The old value is
	// a fingerprint no current client sends, and it is the only wire difference
	// found between this port and that capture (identity block, request tags,
	// thinking dialect, output budgets and the envelope all match byte for byte).
	//
	// os_type/arch describe the machine, because the real client always describes
	// the machine it runs on: a Linux box must not claim windows.
	//
	// This port also had a different capture — the standalone `agy` CLI 1.5.1 with
	// `antigravity/cli/...; auth_method=consumer` (2026-11-05) — and a note that the
	// non-cli flavour answered the Argon lanes with 404. Two captures cannot both
	// describe the same lane, so the parity value is the default and
	// ANTIGRAVITY_IDENTITY=cli restores the previous string without a rebuild
	// (see AntigravityUserAgent).
	AntigravityHubVersion  = "2.32.2"
	AntigravityClientLabel = "1022991777"
	// AntigravityIDEVersion is the value the X-Client-Version headers carry. It
	// tracks the Hub version: one client identity across every Antigravity call.
	AntigravityIDEVersion = AntigravityHubVersion

	// antigravityLegacyCLIUserAgent is the identity this port shipped before the
	// capture above, kept only behind ANTIGRAVITY_IDENTITY=cli.
	antigravityLegacyCLIUserAgent = "antigravity/cli/1.5.1 (aidev_client; os_type=windows; arch=amd64; cl=1014269460; auth_method=consumer)"
)

// AntigravityIdentities are the two values ANTIGRAVITY_IDENTITY accepts.
const (
	AntigravityIdentityHub = "hub"
	AntigravityIdentityCLI = "cli"
)

// AntigravityIdentityOverride reports the value ANTIGRAVITY_IDENTITY names, or
// "" when the captured Hub identity is in use. The variable is this port's own:
// the reference has one identity and no switch, so an override is a deployment
// decision that the boot log announces.
func AntigravityIdentityOverride() string {
	switch value := strings.ToLower(strings.TrimSpace(os.Getenv("ANTIGRAVITY_IDENTITY"))); value {
	case AntigravityIdentityCLI, AntigravityIdentityHub:
		return value
	default:
		return ""
	}
}

// AntigravityUserAgent returns the User-Agent every Antigravity call sends.
//
// ANTIGRAVITY_IDENTITY=cli selects the pre-capture CLI flavour, which is the one
// the repository's older note claims the Argon lanes require. It exists so the
// difference can be A/B tested on a live account without rebuilding the binary;
// the default is the captured Hub identity because PanRouter is the reference.
func AntigravityUserAgent() string {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("ANTIGRAVITY_IDENTITY")), AntigravityIdentityCLI) {
		return antigravityLegacyCLIUserAgent
	}
	return "antigravity/hub/" + AntigravityHubVersion +
		" (aidev_client; os_type=" + antigravityOsType() +
		"; arch=" + antigravityArch() +
		"; cl=" + AntigravityClientLabel + ")"
}

// antigravityOsType names the host the way the client descriptor does: the
// client's own word for macOS is "macos", not "darwin".
func antigravityOsType() string {
	switch runtime.GOOS {
	case "windows":
		return "windows"
	case "darwin":
		return "macos"
	default:
		return "linux"
	}
}

// antigravityArch maps the build's architecture onto the two names the client
// descriptor uses.
func antigravityArch() string {
	if runtime.GOARCH == "arm64" {
		return "arm64"
	}
	return "amd64"
}
