### Route parity — upstream spellings for health, shutdown, reset credits, headroom and the translator playground

Five client-visible gaps against the Node reference's `src/app/api/**` surface.
Three were wrong paths on routes that already existed, one was a whole missing
playground, and one was already ported but had no test pinning its contract.

#### Routes added or moved

| Route | Status | Upstream source |
|---|---|---|
| `GET /api/health` | **added** — same handler and body as `/health` | `src/app/api/health/route.js` |
| `POST /api/shutdown` | **added** — same handler as `/api/version/shutdown` | `src/app/api/shutdown/route.js` |
| `GET /api/usage/{connectionId}/codex-reset-credits` | **moved** from `/reset-credits` | `src/app/api/usage/[connectionId]/codex-reset-credits/route.js` |
| `POST /api/usage/{connectionId}/codex-reset-credits` | **moved** from `/reset-credits/consume` | same file |
| `GET /api/usage/{connectionId}/claude-reset` | **moved** from `/claude-reset-credits` (upstream exports POST only; GET is the list the chooser reads) | `src/app/api/usage/[connectionId]/claude-reset/route.js` |
| `POST /api/usage/{connectionId}/claude-reset` | **moved** from `/claude-reset-credits/consume` | same file |
| `GET /api/translator/load` | **added** | `src/app/api/translator/load/route.js` |
| `POST /api/translator/save` | **added** | `src/app/api/translator/save/route.js` |
| `POST /api/translator/send` | **added** | `src/app/api/translator/send/route.js` |
| `POST /api/translator/translate` | **added** | `src/app/api/translator/translate/route.js` |
| `GET/POST/DELETE /api/headroom/extras`, any `/api/headroom/proxy/{path}` | **already present** — contract test added | `src/app/api/headroom/{extras,proxy/[...path]}/route.js` |

The old reset paths are gone, not aliased: `POST /api/usage/{id}/reset-credits/
consume` and `/claude-reset-credits/consume` now answer 404, which
`TestUsageResetOldNamesAreGone` pins so a stale client fails loudly rather than
silently using a route the next release could delete. The dashboard's own
callers in `web/src/api/client.ts` were updated in the same change, so the
embedded SPA and the server never disagree; a released dashboard build is
shipped from this tree, never separately.

#### `/api/health` and `/api/shutdown`

`/health` was the only liveness path, so a client probing upstream's
`/api/health` fell through to the SPA and got a 404 page. Both GETs now share
one closure — asserted by pointer identity through `chi.Walk`, not by comparing
two copies — and answer `200 {"status":"ok"}`. The body stays this gateway's
shape rather than upstream's `{"ok":true}` because the Go dashboard and the
container healthcheck parse `status`.

`/api/shutdown` was already listed in `middleware.IsAlwaysProtectedPath` but no
route answered it. It is now registered in the same `RequireAdminAuth` group as
`/api/version/shutdown`, pointing at the same `HandleShutdown`, so it inherits
the session-cookie/CLI-token gate instead of becoming a public stop button.

#### Reset credits

The handlers keep their dashboard contract (`{credits, availableCount}` for a
list, `{outcome, selectionToken, idempotencyKey}` for a spend, flat `{error}`
refusals) and the Codex POST still accepts an optional `selectionToken` so a
retried click cannot redeem twice; omitting it redeems the soonest-expiring
credit, which is what upstream's bodyless POST does. Claude's POST now reads
upstream's `{grantId}` instead of the invented `selectionToken`, and the client
sends the same key.

#### Translator playground

Port of the four Next routes into `internal/handlers/translator_playground.go`,
mounted in the dashboard auth group (upstream lists `/api/translator` under
`PROTECTED_API_PATHS`).

- **load/save** edit the same eight allowlisted dump names (`1_req_client.json`
  … `7_res_client.json`) under `DATA_DIR/logs/translator`. Upstream resolves
  them from the Next process cwd; a Go daemon started from another directory has
  no stable cwd, so the app data dir is the equivalent anchor. The allowlist is
  the traversal guard. Status codes and messages are upstream's: `400 File
  parameter required`, `400 Invalid file name`, `404 File not found`, `400 File
  and content required`.
- **translate** follows upstream's `{step, body}` contract. Step 1 returns
  `{provider, model, sourceFormat, targetFormat}` from the same divergence
  detection upstream uses (Responses and Gemini before the OpenAI/Claude split).
  Step 2 pivots source → OpenAI through the existing converters
  (`TranslateClaudeToOpenAI`, `GeminiClientToChatRequest`,
  `ResponsesToChatRequest`). Step 3 builds the provider body, URL and headers
  from the registry config plus the active connection, and refuses with
  upstream's `provider and model required` / `No active connection for
  provider: <id>`.
- A pair this port has no converter for — currently OpenAI → Claude and
  OpenAI → Responses — is a 400 naming both formats instead of a silent
  pass-through. That is a deliberate divergence: upstream registers both
  directions in JS, while this gateway has no OpenAI→Claude request converter;
  pretending to translate would hand the editor a body the gateway would never
  send.
- **send** runs the body through the real executor for the provider, with the
  connection's credential, proxy pool and client, and relays whatever the
  executor writes — the same response a real request would get. A failure the
  executor raised before writing anything becomes upstream's flat
  `{success:false, error}` 500; a failure after the stream started is only
  logged, because appending JSON would corrupt the body.

#### Headroom

`/api/headroom/{start,stop,restart,status,extras,proxy}` and the unprefixed
mirrors were already registered in both the API-key table and the dashboard
group, so no route changed. What was missing was a test: the extras store is
injected now (`extrasStatus` / `installExtras` / `uninstallExtras` fields on
`HeadroomHandler`, defaulting to the pip probes), which lets the CRUD round-trip
and the upstream `{available, installed, version, extras}` shape be pinned
without a Python 3.10 install on the test box. The reverse proxy test proves the
`{path}` suffix and query reach the Headroom host and its answer comes back
unchanged.

#### Tests

`TestHealthAndShutdownAliasesShareOneHandler`, `TestUsageResetOldNamesAreGone`,
`TestTranslatorLoadReturnsUpstreamShape`, `TestTranslatorSaveWritesTheAllowlistedFile`,
`TestTranslatorTranslateStep1DetectsFormats`,
`TestTranslatorTranslateStep2PivotsThroughOpenAI`,
`TestTranslatorTranslateRefusesMismatchedPair`,
`TestTranslatorTranslateRefusesPairThisPortCannotProduce`,
`TestTranslatorSendRefusesWithoutAPairOrConnection`,
`TestTranslatorRoutesAreMounted`, `TestHeadroomExtrasCRUDRoundTrip`,
`TestHeadroomExtrasErrorCodes`, `TestHeadroomProxyForwardsUpstreamPathAndQuery`;
the existing Claude/Codex reset tests were re-pointed at the new paths and the
Claude body at `grantId`.

Verified: `go test ./internal/handlers/ ./internal/handlers/dashboard/
./internal/handlers/media/` and `go vet` on the same packages; `cd web && bun
run build` after the client path change.