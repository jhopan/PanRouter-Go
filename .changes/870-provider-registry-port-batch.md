### Added — thirteen upstream providers that were missing from the Go registry

Thirteen providers that exist in `open-sse/providers/registry` had no transport
here, so a `provider/model` request for any of them answered "unknown provider"
at dispatch and stayed invisible in the dashboard. Ported with upstream's
declared transport verbatim:

| Provider | Transport | Notes |
|---|---|---|
| `agentrouter` (`AR`, `ar`) | `https://agentrouter.org/v1/messages?beta=true`, format claude, raw `x-api-key` | WAF-gated on client identity; carries upstream `CLAUDE_CLI_SPOOF_HEADERS` (full Claude CLI + Stainless fingerprint, `transport.urlSuffix` folded into the base URL). |
| `apinex` (`apn`) | `https://api.apinex.bond/v1/chat/completions` | live `/v1/models` catalogue wired. |
| `apmix` (`am`) | `https://api.apmix.ai/v1/chat/completions` | live catalogue wired. |
| `freemodel-dev` (`fmd`, `freemodel`) | `https://api.freemodel.dev/v1/chat/completions` | live catalogue wired. |
| `justdowork` (`jd`, `justwoker`) | `https://api.justwoker.icu/v1/messages`, format claude | upstream declares the Anthropic path because the OpenAI path is Cloudflare-blocked from some regions. |
| `orcarouter` (`orca`) | `https://api.orcarouter.ai/v1/responses` | Responses-only transport: all four models pin `openai-responses` in `ProviderModelFormats`, and no Chat Completions passthrough is declared, mirroring upstream. |
| `vyceai` (`vy`) | `https://vyceai.com/v1/chat/completions` | live catalogue wired. |
| `freeai` (`fai`, `free-ai`, `free.ai`) | `https://api.free.ai/v1/chat/` | one node, two endpoint families: `FreeAIExecutor` moves the three vision ids to `/v1/chat/completions` per request (port of `open-sse/executors/freeai.js`), and stands aside when the connection carries its own base URL. |
| `freeai-vl` (`fai-vl`, `free-ai-vl`, `freeai-vision`) | `https://api.free.ai/v1/chat/completions` | hidden; kept for connections created before the families merged. |
| `mmf` | `https://api.xiaomimimo.com/api/free-ai/openai/chat`, no auth | upstream's hidden MiMo lane. `mmf` already resolved to `mimo-free` through that provider's declared alias, so the id is reached by a stored connection row; the catalog row is shared. |
| `selfhosted-tts` / `selfhosted-stt` / `selfhosted-embedding` | `http://localhost:8880`, `http://localhost:8080`, `…/v1/embeddings` | the media lanes had model rows, kinds and dashboard cards but no transport, so the provider resolved and could not be dialled. |

Every entry landed in all five tables the dashboard and the request path read —
`KnownProviders` (transport), `ProviderAliasMap` (prefix resolution),
`RegistryAliases` (published prefix), `ProviderModels` (offline catalogue) and
`ProviderRegistry` (card, category, service kinds, live-catalogue fetcher) — plus
executor dispatch, because a provider that reaches `KnownProviders` but not the
executor registry fails at dispatch with no test in `internal/providers` able to
see it (that package cannot import `internal/proxy/executor` without a cycle).

Two upstream values were reconciled along the way: `mmf` is now marked
`noAuth`/`AuthType: "none"` (upstream `noAuth: true`), and the self-hosted TTS
and embedding cards now declare `authType: "apikey"` instead of `none`, which is
what upstream documents as the reason the per-connection `baseUrl` override is
possible at all — the credential row is where `providerSpecificData.baseUrl`
lives.

Tests: `TestRegistryOnlyProvidersParity` (transport, auth pair, alias table in
both directions, catalogue under every key, published prefix, dashboard metadata
and fetcher per provider), `TestAgentRouterCarriesTheClaudeCLIFingerprint`,
`TestOrcaRouterPinsTheResponsesLane`, `TestRegistryPortBatchHasAnExecutor`, and
`TestForwardFreeAIPicksTheEndpointFamilyPerModel` (asserts the dialled URL per
model through a recording transport, and that a connection override wins).
`TestAgentRouterWireRequest` asserts what actually leaves the process — path,
`?beta=true`, the CLI `User-Agent`, the Stainless block and the raw key — because
a config-only assertion cannot see a URL suffix. The dashboard catalogue and the
Go registry are kept in step by the existing
`TestDashboardCatalogListsEveryRoutedModel`, which already covers the new
providers through its alias mapping.

Verified: `go build ./...`, `go test -count=1 ./internal/providers/
./internal/proxy/ ./internal/proxy/executor/` green.

### Fixed — CodeBuddy's announced reset is honoured as an absolute instant, with a margin

Two halves of the same defect, both from `AGENTS.md` §1:

1. **The reset time now carries upstream's ten-minute safety margin.** A provider
   that names a reset states when the quota refills on *its* clock; the request
   in flight, the clock skew and the moment the counter rolls over are not the
   same instant, and asking one second early earns another refusal — which is
   itself the automation signal. Ported from `RESET_SAFETY_MARGIN_MS`
   (`open-sse/executors/codebuddy-intl.js`).
2. **A monthly quota's reset is no longer clamped to two hours.** The generic
   ceiling is right for a provider that names a wait in minutes and wrong for
   one registered as a MONTHLY scope upstream (`quotaWindows.js`:
   `codebuddy-intl` and `codebuddy-cn`). The body can name a reset days away;
   clamping it to two hours put an exhausted account back into rotation while it
   was still out of quota. `maxCooldownFor` now returns upstream's default
   40-day ceiling for those two, and keeps the two-hour ceiling for everyone
   else and the 7-day one for deepseek-web, whose payload carries a real mute
   release time.

Tests: `TestExtractResetDuration_CodeBuddyDate` (25m + the margin),
`TestExtractResetDuration_CodeBuddyDateHonoursTheBodyOffset` (UTC+7 and UTC+8
spellings of one instant must resolve together),
`TestExtractResetDuration_MonthlyResetIsNotClampedToTwoHours`,
`TestMaxCooldownFor`, `TestRetryableCooldownSec_CodeBuddyMonthlyWindow` (the
20-day window survives on codebuddy-intl and is capped for a provider with the
generic ceiling).

### Fixed — a CodeBuddy rate limit stops the rotation instead of walking the pool

`AGENTS.md` §1 calls this out as the pattern that got the accounts banned: a
quota refusal answered by retrying every other account turned one 429 into "29
requests in two seconds across 29 accounts", which the provider reads as
automation and answers with `11140 account restricted due to unusual activity`
and a 30-day ban.

`handleAccountFallback` now ends the pass on a 429 from CodeBuddy Intl
(`codebuddy-intl` and its `cbai` spelling): the account is locked first — for the
announced reset plus the margin, per the change above — and the refusal is
returned to the caller instead of rotating. Every other provider keeps the pool
walk, because there the next account is a real second chance rather than a
second signal; the new test's control half proves exactly that.

Tests: `TestHandleAccountFallback_CodeBuddyRateLimitStopsRotation` (the second
account is never dialled on codebuddy-intl, *is* dialled on deepseek, and the
stored `rateLimitedUntil` names the 30m + 10m window) and
`TestStopsRotationOnRateLimit` (predicate table, including the CN site and a
non-429 status).

### Fixed — the stream watchdog runs two budgets, not one

`AGENTS.md` §1 item 4. One timer covered both phases, so a slow prompt prefill on
a 120k-token context was charged against the silence budget and aborted healthy
turns — MiniMax Code, whose 512k-context turns prefill for minutes, is where it
showed.

Ported from upstream `pipeWithDisconnect` (open-sse/utils/streamHandler.js:194+):

- `StallBudgets{FirstChunk, InterChunk}` replaces the single timeout, with the
  two defaults upstream compiles in: 200s before the first byte
  (`STREAM_FIRST_CHUNK_TIMEOUT_MS`), 360s between chunks
  (`STREAM_STALL_TIMEOUT_MS`). Both are env-overridable under their own name and
  the legacy `PANROUTER_`-prefixed spelling.
- The watchdog arms the budget for the phase it is in and re-arms to the
  inter-chunk one as soon as a byte arrives.
- The failure is a typed `*StreamStallError` naming the phase ("no first chunk"
  vs "stalled after first chunk") and carrying the budget, the wait, the silence
  and the byte/chunk counts — the difference between "the provider is slow" and
  "the provider is dead" is exactly those numbers, and the old message
  ("stream stall timeout") could not tell them apart. It unwraps to
  `ErrStreamStall`, so every existing classifier keeps working.
- The in-band error frame carries `code: "STREAM_STALL"` (`StreamAbort` +
  `BuildStreamErrorFrame`), which is what upstream tags the error with, instead
  of the status-derived `gateway_timeout` — a client can branch on it.
- The registry now carries `firstChunkTimeoutMs` and both MiniMax Code entries
  declare upstream's pair (`stallTimeoutMs: 15m`, `firstChunkTimeoutMs: 10m`),
  sized for the context window rather than for the 6-minute default.
  `proxy.ResolveStallBudgets` applies a provider's declared values and leaves
  the default in place for whichever phase it does not declare, so a provider
  with only `stallTimeoutMs` is not silently given the same value for prefill.

Tests: `TestResolveDefaultFirstChunkTimeout`, `TestResolveStallBudgets` (nil,
inter-chunk-only, both, and the MiniMax pair), and
`TestStallReaderUsesTheBudgetForItsPhase` — the behavioural half, where a 30ms
first-chunk budget with an hour-long inter-chunk budget produces "no first
chunk", and the mirror image produces "stalled after first chunk" with the byte
count. `TestStallTimeoutEnvEndToEnd` and the SSE-copy tests now assert the
`STREAM_STALL` code.

### Fixed — the dashboard catalogue builds again, and a file-less provider stops paying a 404

Two defects the registry batch introduced into `web/`, both caught by the gates
rather than by a user:

1. **A duplicated catalog entry broke the build.** `selfhosted-embedding` landed
   twice in `web/src/lib/models.ts`, so `tsc -b` failed the whole SPA build with
   `TS1117` and `web/dist` could not be regenerated — every embedded-asset test
   then ran against a stale bundle. The later duplicate is gone; the surviving
   entry is byte-identical.
2. **Eight new providers had no artwork, and the suite said so.** Neither this
   repo nor PanRouter ships a PNG for `apinex`, `apmix`, `freemodel-dev`,
   `justdowork`, `orcarouter`, `vyceai`, `freeai` or `freeai-vl` — upstream draws
   a Material icon plus a `textIcon` for them — so `getIconPath` asked for a file
   that is not there. `ProviderArtwork` already degraded to the initials badge on
   `onerror`, which is one wasted request per tile on a dashboard that draws
   dozens. `ARTWORKLESS_PROVIDERS` (in `web/src/components/connections/types.ts`)
   now names them: `hasArtwork()` answers without a request, the badge renders
   directly, and `providers.test.ts` pins the list in both directions (every id
   exists in the catalog; no listed id ships a file that would now be ignored).

Verified: `cd web && bun run build` (TS + Vite + pre-compression green;
1527 KB → 289 KB brotli), `bun test src scripts` (710 pass, 0 fail),
`bun run ratchet:svelte` (0 unresolved identifiers, 75 errors = baseline),
`go test -count=1 -run 'TestSetupServerRouter_SPARoutes|TestEmbedCompression' ./internal/handlers/ ./web/...`
green against the rebuilt bundle, and `du -sh web/dist` = 4.5 MB.

### Fixed — test fixtures still pinned the pre-rename `panrouter` identity

The rename to PanRouter left eight packages of tests asserting names the source
had already stopped emitting, so the suite was red for reasons unrelated to any
behaviour change: `internal/handlers/media` (a compile error —
`claudeHasPanRouter`, `HasPanRouter`, `hasPanRouterConfig` no longer exist, so the
whole package's tests failed to build), `internal/clientconfig` (Codex/OpenCode/
Pi/OMP/Adapters fixtures and expectations), `internal/config` (`panrouter.db` vs
the `panrouter.db` the loader reads), `internal/handlers/sso`
(`urn:panrouter:sp` vs `urn:panrouter:sp`), `internal/mitm` (`# panrouter-mitm` vs
`# panrouter-mitm`), `internal/providers` (Cline identity UA/`X-CLIENT-TYPE`,
Nous attribution tags), `internal/proxy/oauth` (Cline refresh identity),
`internal/updater` (release asset names `panrouter-go_*`, which the archive
extractor scores by project name), and `internal/service` (launchd label, unit
name, OpenRC/runit/cron paths, desktop entry — the service-name constants are
all `panrouter-go` now).

Every one of those is an expectation or a fixture, not a behaviour: the source
already emitted the new identity, so the tests were the stale side. The env-var
names (`PANROUTER_SERVICE_BACKEND`, `PANROUTER_BACKGROUND`, `PANROUTER_LIVE_TESTS`)
are deliberately untouched — they are a runtime contract, not branding.

Verified after each package: `go test -count=1` green for `internal/handlers/media`,
`internal/clientconfig`, `internal/config`, `internal/handlers/sso`,
`internal/mitm`, `internal/providers`, `internal/proxy/oauth`,
`internal/updater`, `internal/service`.
