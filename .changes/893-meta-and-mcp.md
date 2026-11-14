### Dashboard boot surface, pricing, Ollama tags and the MCP stdio bridge

Five upstream dashboard routes that had no Go counterpart, ported from
`src/app/api/**/route.js` of the Node reference. Each is additive: no existing
route, payload or auth rule changed.

#### Routes

| Route | Auth group | Upstream source |
|---|---|---|
| `GET /api/init` (+ `HEAD`) | public | `src/app/api/init/route.js` |
| `POST /api/locale` | public | `src/app/api/locale/route.js` |
| `GET /api/locale` | public (405, `Allow: POST`) | same file — it exports no GET |
| `GET /api/pricing` | dashboard (session / API key / CLI token) | `src/app/api/pricing/route.js` |
| `GET /api/tags` + `OPTIONS /api/tags` | dashboard | `src/app/api/tags/route.js` |
| `GET /api/mcp/{plugin}/sse` | dashboard, then local-only | `src/app/api/mcp/[plugin]/sse/route.js` |
| `POST /api/mcp/{plugin}/message` | dashboard, then local-only | `src/app/api/mcp/[plugin]/message/route.js` |

`/api/init` and `/api/locale` are mounted in `SetupServerRouter`'s public block
because upstream lists both in `dashboardGuard.PUBLIC_API_PATHS`; the rest are
mounted in `SetupDashboardRoutes` (`PROTECTED_API_PATHS`), with the four
dashboard handlers also mirrored in `dashboard.RegisterRoutes` for that
package's tests.

#### `GET /api/init`

Byte-for-byte upstream: status 200, body `Initialized`, `text/plain;charset=UTF-8`,
and no cache directive, cookie, validator or body content that could go stale
(upstream sets none either). The test pins all of that, including that the body
carries no credential-shaped token.

#### `/api/locale`

`POST` is the whole of upstream's route: `{locale}` in, `locale` cookie (path
`/`, `Max-Age=31536000`) plus `{"success":true,"locale":"…"}` out; an
unsupported tag is 400 `{"error":"Invalid locale"}`, an unparseable body 500
`{"error":"Failed to set locale"}`. A bare `zh` is refused, because upstream
validates with `isSupportedLocale` before `normalizeLocale` ever sees it.

There is no `GET /api/locale` upstream — Next answers the unhandled method with
405 — and the dashboard's translations are static files under
`/i18n/literals/<locale>.json`, not an API response. The Go route therefore
answers 405 with `Allow: POST` rather than inventing a locale catalogue; the
embedded SPA ships English only (issue #240), so nothing consumes a payload
that does not exist upstream.

#### `GET /api/pricing`

Upstream answers `getPricing()`: the provider → model → rate table
(`input`, `output`, `cached`, `reasoning`, `cache_creation`, $/1M) merged with
the operator's stored overrides. This gateway stores no rate overrides — it
bills from the compiled tables in `internal/pricing` — so the response is that
default table, exposed by the new `pricing.DefaultPricing()`. That is the same
body an install with no overrides gets upstream, and the merge point stays in
the handler if an override store is ever added.

`PATCH` and `DELETE /api/pricing` are deliberately **not** registered: there is
no table for them to write, and accepting a write only to discard it would let
the dashboard's pricing editor report success while nothing changed.

#### `GET /api/tags`

`media.HandleOllamaTags` already existed but was never mounted, so the route
404'd. It is now registered in the dashboard group with the CORS preflight
upstream answers. It lists the models the gateway can actually route for
`ollama` (the handler's documented deviation from upstream's two-model fixture,
whose shape — `{"models":[{name,modified_at,size,digest,details}]}` — it
reproduces).

#### `/api/mcp/{plugin}/{sse,message}`

Full port of the inline bridge (`src/lib/mcp/stdioSseBridge.js` +
`src/app/api/mcp/[plugin]/*`) into `internal/mcpbridge`, with the HTTP contract
in `internal/handlers/dashboard/mcp.go`:

- One child per preset plugin, spawned on the first SSE subscriber and shared by
  every subscriber after that. The plugin table is the RCE boundary, exactly as
  upstream: `browsermcp` (`npx -y @browsermcp/mcp@latest`) is the only entry, and
  a name outside the table is never a command.
- SSE answers `text/event-stream` with upstream's four headers and the MCP
  handshake `event: endpoint` / `data: /api/mcp/<plugin>/message?sessionId=<uuid>`,
  flushed before the stream blocks. Child stdout is split on newlines and
  broadcast as `event: message` frames.
- `POST` writes the body verbatim to the child's stdin and answers an empty
  **202**; an unknown plugin is 404 `{"error":"Unknown plugin: …"}` (the SSE
  route answers the same sentence as plain text, as upstream does); an
  unparseable body or a dead child is 500, the latter with upstream's
  `{"error":"Bridge not running: <plugin>"}`.
- Upstream ignores `sessionId` on POST, so an unknown session is not a lookup
  failure — it is accepted whenever the plugin's child is running. The tests
  pin that shape rather than a 404 upstream never sends.
- The last subscriber leaving kills the child, and `mcpbridge.Default().KillAll()`
  runs in the server's `OnStop` hook, so an npx process never outlives the
  gateway (upstream `killAllBridges`).
- Upstream's `LOCAL_ONLY_PATHS` entry `/api/mcp/` is honoured: a peer that is
  not on this host and holds no CLI token gets 403
  `{"error":"Local only: CLI token required"}` before any process is spawned.
- The frame filter is ported too: only `result.content[]` text blocks past 2000
  characters are touched — noise lines dropped, sibling groups of 30+ folded to
  their first 10 and last 5, and a hard 50000-character truncation. Frames that
  are not result text are passed through byte-for-byte.

Not Node-bound: the bridge is transport-agnostic and Go spawns the child
itself. What remains a Node dependency is the preset itself — `npx
@browsermcp/mcp` needs Node on `PATH`, exactly as upstream does. Documented
divergences, all bounded-resource choices Go needs and Node's unbounded streams
do not: an 8 MiB POST cap, a 32 MiB child-line cap, and a 256-frame per-session
queue that drops the newest frames for a subscriber that stops reading instead
of stalling every other subscriber of the same plugin.

#### Verified

- `go build` + `go vet` clean on `internal/mcpbridge`, `internal/pricing`,
  `internal/handlers`, `internal/handlers/dashboard`, `internal/app`.
- `go test ./internal/mcpbridge/` — 9 bridge cases (session UUID, shared child,
  broadcast to both subscribers, unregister/kill, KillAll, Send without a child,
  preset-only plugin lookup) plus 9 filter cases (threshold, noise lines,
  group collapse, sub-threshold groups, truncation count, non-JSON passthrough,
  text-only rewriting, sibling parsing).
- `go test ./internal/handlers/dashboard/` — new cases for `/api/init` (constant
  body, no cache/cookie headers, no secret), `/api/locale` (9 POST cases plus
  the 405 GET), `/api/pricing` (fixed `gh/gpt-5.3-codex` row and key set) and
  the MCP routes (unknown plugin 404, bridge-not-running 500, delivery to the
  child proving the stdin line and the returning frame, invalid JSON, both
  routes refusing a remote caller with 403, the CLI token admitting one, and the
  SSE handshake with `Content-Type: text/event-stream`, flushed before blocking,
  ended by a cancelable context).
- `go test ./internal/handlers/ -run TestSetupServerRouter_MetaAndMCPRoutesAreLive`
  — boots the real `SetupServerRouter` and asserts every route above is mounted:
  init/locale answer without a session, and the dashboard-gated five answer 401
  anonymously (not 404) and serve with a session cookie.