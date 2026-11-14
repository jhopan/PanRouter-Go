### Added — Claude's free "limit reset" grants, and the credential guard beside them

`docs/PARITY-REMAINING.md` §3 listed two Claude gaps. Both are closed, ported
from `open-sse/services/usage/claude.js` and
`src/app/api/usage/[connectionId]/claude-reset/route.js`.

**The grant read.** The usage endpoint now asks for the grants the way Claude
Code does (`?cedar_ember=1`; without the flag the response omits the block
entirely) and attaches `resetCredits` to the usage result, shaped exactly as
upstream parses it: `availableCount`, `nextGrantId`, `expiresAt`, `clears`,
`cooldownUntil`, `weeklyResetsAt`, and a `grants` list keeping the paused and
spent-out entries so the UI can say *why* one is not offered. Only addressable,
unpaused grants with a count left are counted, and the next grant is the one the
server names, falling back to the first available.

**The spend.** `POST /api/usage/{connectionId}/claude-reset` resolves the
organization from the OAuth profile and posts
`{program: "cedar_ember", grant_id, request_id}` scoped to it, with the Claude
Code client identity the endpoint is gated on. Spending a grant is irreversible,
so every local precondition is checked before the first request leaves: the grant
id must match upstream's own `^[a-z0-9_-]{1,40}$`, and the connection must be a
Claude OAuth row — the token sent is Anthropic's, so a Codex or Gemini account's
token must never reach that endpoint. The cached usage reading is invalidated
afterwards, so the next read shows the refilled limits instead of the reading the
call just made stale.

Status mapping is upstream's: the result body on success, the upstream status
passed through when it is a 4xx and a 409 otherwise, and a `message` that is the
account's own error when it sent one and `Reset not applied: <reason|result>` when
it did not. Failures outside a reset response are a 500 carrying the message —
never a 401, because this app reads a 401 as an expired dashboard session and
logs the user out.

The route is registered on **both** tables (`dashboard.RegisterRoutes` and the
production `SetupServerRouter`). The Codex reset credits first shipped wired into
only the first, which every dashboard test mounts, so the whole suite passed
while the real gateway answered 404; `TestSetupServerRouter_ClaudeResetGrantIsLive`
pins the production table this time.

**The credential guard.** `claudeApplyEdits` wrote `ANTHROPIC_AUTH_TOKEN` and
`ANTHROPIC_BASE_URL` unconditionally, so a connect that carried no key replaced a
working setup with `"ANTHROPIC_AUTH_TOKEN": ""` and a base URL of bare `"/v1"`
(`withV1("")`). Upstream sets the token only when there is one
(`if (apiKey) env.ANTHROPIC_AUTH_TOKEN = apiKey`, `cli/src/cli/menus/cliTools.js`)
and normalizes the base URL only when the request carried one; both writes now
follow that rule and an absent value leaves the file untouched.

Tests: `TestParseClaudeResetGrants` (ineligible → no block; paused and spent-out
grants excluded from the count but kept in the list; the named grant wins over
the first available; `useRequiresLimit` defaults true),
`TestConsumeClaudeResetGrant_RefusesBadInputLocally` (empty token and five
malformed grant ids, with an upstream stub asserting zero requests),
`TestConsumeClaudeResetGrant_HappyPath` (profile → organization → POST, asserting
the path, the program/grant/request-id body, and the CLI identity headers),
`TestConsumeClaudeResetGrant_UnresolvableOrganization`,
`TestClaudeResetRoute` (through the router, stubbed upstream),
`TestClaudeResetRoute_ReportsUpstreamRefusal` (429 passed through, composed
message), `TestClaudeResetRoute_RefusesUnusableConnection` (API-key row and a
Codex OAuth row both refused with upstream's message),
`TestSetupServerRouter_ClaudeResetGrantIsLive`, and
`TestClaudeApplyWritesOnlyProvidedCredentials` (an empty request leaves a working
token and base URL intact; a populated one still writes both).

Verified: `go build ./...`, `go test -count=1 ./internal/handlers/
./internal/handlers/dashboard/ ./internal/clientconfig/` green.