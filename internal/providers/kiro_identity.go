// Kiro client identity — ONE source for every hop that talks to the Amazon
// surfaces (chat, model discovery, usage). Port of open-sse/shared/kiroIdentity.js.
//
// Why this matters: the Amazon Q / CodeWhisperer gateway parses the Kiro version
// out of the user agent and refuses old builds outright:
//
//	400 {"message":"This version of Kiro is no longer supported.
//	     Please update to the latest version of Kiro to continue."}
//
// It rejects the whole request, not just a model, so a stale constant takes the
// provider down. Verified against the live gateway: `KiroIDE-1.4.178-<machineId>`
// passes the version gate, while `kiro-ide/1.0.0` and the space-separated
// `KiroIDE 1.4.178 <machineId>` form are both refused.
//
// The version must track a real Kiro IDE build — the IDE derives it from its own
// product.json, so this value is read from an installed IDE, not invented.
// Cross-checked against <install>/resources/app/product.json:
// {"nameShort":"Kiro","version":"1.4.178"}.
package providers

import (
	"crypto/sha256"
	"encoding/hex"
	"runtime"
	"strings"
)

const (
	// KiroIDEVersion is the IDE build the version gate reads.
	KiroIDEVersion = "1.4.178"
	// KiroRuntimeSDKVersion is the `aws-sdk-js/<ver>` token the captured client
	// sends; also the `#<ver>` tail of the api/<service>#<ver> pair.
	KiroRuntimeSDKVersion = "1.0.0"
	// KiroAgentServerVersion is the Kiro Agent Server embedded in the 1.4.178
	// IDE, captured 2026-11-15 from a live request. Not part of the gated version
	// token, but it is what the real client sends:
	//   ... KiroIDE-1.4.178-<machineId>-KAS/0.66.102-hotfix.1 acp-client/kiro-ide
	KiroAgentServerVersion = "0.66.102-hotfix.1"
	// KiroClientAttribution closes the same captured user agent.
	KiroClientAttribution = "acp-client/kiro-ide"

	// KiroRuntimeTarget is the operation the 1.4.178 IDE asks the kiro.dev
	// gateway for, captured 2026-11-15: the path-style GenerateAssistantResponse
	// endpoint answers valid modern payloads with 400 REQUEST_BODY_INVALID, and
	// the operation now travels in the x-amz-target header instead.
	KiroRuntimeTarget = "KiroRuntimeService.GenerateAssistantResponse"
	// KiroCodeWhispererTarget is the Amazon-host operation, unchanged.
	KiroCodeWhispererTarget = "AmazonCodeWhispererStreamingService.GenerateAssistantResponse"

	// KiroNodeVersion is the Node the captured 1.4.178 IPC bundle reports. A Go
	// process has no Node version to report, and the token has to be plausible:
	// claiming `md/nodejs#go1.27` would describe a client that does not exist,
	// while this value describes the client the gateway expects to see.
	KiroNodeVersion = "24.22.1"
)

// KiroServiceIDForURL names the AWS service a URL belongs to, which is what goes
// into the `api/<service>#<ver>` user-agent pair (from the same capture):
//
//	runtime.<region>.kiro.dev   → kiroruntime   (KiroRuntimeService.*)
//	codewhisperer.<region>...   → codewhispererstreaming
//	q.<region>.amazonaws.com    → codewhispererruntime
func KiroServiceIDForURL(url string) string {
	switch {
	case strings.Contains(url, "://runtime."):
		return "kiroruntime"
	case strings.Contains(url, "://codewhisperer."):
		return "codewhispererstreaming"
	default:
		return "codewhispererruntime"
	}
}

// KiroOSPlatform is the `os/<platform>` token. The captured client reports
// os.platform() — win32 | linux | darwin — never the word "windows"; Go spells
// the same platform differently, so it is mapped here rather than sent raw.
func KiroOSPlatform() string {
	if runtime.GOOS == "windows" {
		return "win32"
	}
	return runtime.GOOS
}

// KiroMachineID is the stable per-account id the version token carries. Keyed off
// whatever durable identifier the credential holds, so the same account always
// presents the same fingerprint and keeps its queue/session standing.
func KiroMachineID(psd map[string]any, accessToken string) string {
	seed := ""
	if psd != nil {
		if s, _ := psd["clientId"].(string); strings.TrimSpace(s) != "" {
			seed = strings.TrimSpace(s)
		} else if s, _ := psd["refreshToken"].(string); strings.TrimSpace(s) != "" {
			seed = strings.TrimSpace(s)
		} else if s, _ := psd["profileArn"].(string); strings.TrimSpace(s) != "" {
			seed = strings.TrimSpace(s)
		}
	}
	if seed == "" && strings.TrimSpace(accessToken) != "" {
		seed = strings.TrimSpace(accessToken)
	}
	if seed == "" {
		seed = "kiro-anonymous"
	}
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])
}

// KiroUserAgents returns the two user-agent strings Kiro validates.
// `KiroIDE-<version>-<machineId>` is the part the version gate reads; the
// `-KAS/<agent server> acp-client/kiro-ide` tail and `api/<service>#<sdk>` token
// are what the 1.4.178 client puts around it (captured).
//
// The os/ and md/ pairs are not constants in the real client — the bundled AWS
// SDK builds them from the host that runs it. They are derived from this host for
// the same reason: a claim about the machine we run on should be true, and a Linux
// VPS announcing `os/windows#10.0.26200` is a fingerprint that belongs to nobody.
func KiroUserAgents(psd map[string]any, accessToken, serviceID string) (userAgent, amzUserAgent string) {
	if serviceID == "" {
		serviceID = "codewhispererruntime"
	}
	machineID := KiroMachineID(psd, accessToken)
	custom := "KiroIDE-" + KiroIDEVersion + "-" + machineID +
		"-KAS/" + KiroAgentServerVersion + " " + KiroClientAttribution
	userAgent = "aws-sdk-js/" + KiroRuntimeSDKVersion +
		" ua/2.1 os/" + KiroOSPlatform() + "#" + kiroOSRelease() +
		" lang/js md/nodejs#" + KiroNodeVersion +
		" api/" + serviceID + "#" + KiroRuntimeSDKVersion +
		" m/N,E " + custom
	amzUserAgent = "aws-sdk-js/" + KiroRuntimeSDKVersion + " " + custom
	return userAgent, amzUserAgent
}
