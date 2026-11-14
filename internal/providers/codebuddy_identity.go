package providers

import (
	"crypto/sha256"
	"encoding/base64"
	json "encoding/json/v2"
	"strings"
	"sync"
	"sync/atomic"
)

// CodeBuddy CLI identity, shared by every lane and every surface that talks to
// a CodeBuddy gateway.
//
// Port of upstream `open-sse/executors/codebuddy-intl.js` (the CB_* constants)
// and `open-sse/shared/codebuddyUserId.js`, PanRouter v0.5.88.21/.22. The
// gateway treats one account presenting two different clients as automation:
// the chat lane advertised "CLI/2.178.0" while the registry OAuth lane still
// advertised "IDE/2.160.0" — one account, two identities, and the chat lane
// kept coming back restricted. Every value the CLI puts on the wire therefore
// lives here once and both lanes (intl and cn) read it from this file.
//
// Like the upstream module this file imports nothing of the router's own: the
// token refresh service and the executor both need the account id, and either
// importing the other closes a cycle (executors -> services -> executors) that
// fails at module-evaluation time.

// CodeBuddyCLIVersion is the official CodeBuddy Code CLI build every lane
// (intl and cn) mimics. Version drift is a ban signal in its own right: the
// gateway compares the client build against the one it shipped, and a build it
// no longer serves reads as a parked or hand-rolled client.
//
// Port of upstream `open-sse/executors/codebuddy-intl.js:37` (CB_IDE_VERSION),
// captured live from CLI 2.178.0 by MITM. This constant is the single source of
// truth — the provider registry, the chat executor and the CN telemetry all
// read it from here, so no lane can advertise a different build.
const CodeBuddyCLIVersion = "2.178.0"

// The Stainless SDK version the CLI embeds, and the Node runtime it reports.
// Both are observable on the wire, so they are pinned rather than taken from
// the host: the gateway compares them against the shipped CLI.
const (
	CodeBuddySDKVersion  = "7.39.0"
	CodeBuddyNodeVersion = "v26.7.0"
)

// CodeBuddyUserAgent is the CLI's User-Agent. The CLI sets its IDE headers
// to "CLI" as well, not "IDE": the gateway tracks the caller type, and "IDE"
// triggers IDE-mode fingerprinting.
const CodeBuddyUserAgent = "CLI/" + CodeBuddyCLIVersion + " CodeBuddy/" + CodeBuddyCLIVersion

// CodeBuddyIntlDomain is the product surface the intl lane belongs to. The CLI
// sends it on every chat call, every token refresh and every login step.
const CodeBuddyIntlDomain = "www.codebuddy.ai"

// codeBuddyUserIDCache memoizes the `sub` claim per bearer token.
//
// Every chat turn, refresh call and billing call carries X-User-Id, and the
// claim only changes when the credential is rotated. Decoding the same JWT
// payload per request is pure waste, so the parse is cached — keyed by a
// SHA-256 of the token rather than the token itself, so the cache never keeps a
// second copy of a secret in memory. The cache is bounded: a long-lived gateway
// sees many rotated tokens, and past the cap a new token is simply parsed every
// time instead of growing the map without limit. A token with no `sub` is
// cached too, so a bad token does not cost a decode on every turn either.
var (
	codeBuddyUserIDCache        sync.Map // [32]byte token digest -> string sub
	codeBuddyUserIDCacheEntries atomic.Int32
)

// codeBuddyUserIDCacheMax bounds the cache. Tokens are roughly one per
// connection per refresh window, so a few hundred entries covers a large fleet.
const codeBuddyUserIDCacheMax = 512

// CodeBuddyUserIDFromToken reads the CodeBuddy account id out of a bearer
// token.
//
// The official CLI sends an `X-User-Id` header on every request — chat, token
// refresh, account listing — and the value is the token's `sub` claim (a UUID).
// The gateway ties the request to the account behind the token, so the header
// has to match the token it was sent with. Returns "" when the token carries no
// account id, and the header is then simply omitted.
//
// The claims are decoded, never verified: the token is the credential the
// caller already holds, and the value here only labels a request.
func CodeBuddyUserIDFromToken(token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return ""
	}
	key := sha256.Sum256([]byte(token))
	if cached, ok := codeBuddyUserIDCache.Load(key); ok {
		return cached.(string)
	}
	id := codeBuddyParseUserID(token)
	if codeBuddyUserIDCacheEntries.Load() < codeBuddyUserIDCacheMax {
		if _, loaded := codeBuddyUserIDCache.LoadOrStore(key, id); !loaded {
			codeBuddyUserIDCacheEntries.Add(1)
		}
	}
	return id
}

// codeBuddyParseUserID decodes the `sub` claim of a JWT-shaped token.
func codeBuddyParseUserID(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[1] == "" {
		return ""
	}
	// JWT payloads are base64url; the CLI's Node decoder also tolerates the
	// standard alphabet and missing padding, so both are accepted here.
	payload := strings.NewReplacer("-", "+", "_", "/").Replace(parts[1])
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		raw, err = base64.RawStdEncoding.DecodeString(payload)
		if err != nil {
			return ""
		}
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		return ""
	}
	return claims.Sub
}

// CodeBuddyUserIDFromCredentials reads the account id the way the CLI's
// credentials object carries it: the first field that holds a token is the one
// used, and a token without a `sub` claim yields no id rather than falling
// through to the next field (upstream `userIdFromCredentials`).
func CodeBuddyUserIDFromCredentials(credentials map[string]any) string {
	for _, key := range []string{"accessToken", "access_token", "token", "apiKey", "api_key"} {
		token, ok := credentials[key].(string)
		if !ok || strings.TrimSpace(token) == "" {
			continue
		}
		return CodeBuddyUserIDFromToken(token)
	}
	return ""
}
