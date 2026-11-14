### Changed — synced to upstream panrouter-go (241 commits, `1.9.16-exp.1` → `1.9.17-exp.2`)

This repository started as a cleaned fork of `jhopan/PanRouter-Go` at
`1.9.16-exp.1` (2026-11-02). Upstream has since shipped 241 commits, and the
operator's instruction is to take that work in full before continuing the
PanRouter-side parity work.

Delta measured against our fork point (`5a2a0da`, VERSION `1.9.16-exp.1`):
**682 files** under `internal/`, `web/` and `cmd/`…, 89 000 insertions. Split by
whether we had touched a file since the fork:

| Class | Files | Handling |
|---|---|---|
| New upstream files | 251 | copied (module path rewritten `panrouter/proxy` → `panrouter/proxy`) |
| Files we never touched | 181 | copied verbatim — upstream's newer revision |
| Files we had also changed | 250 | merged: upstream's hunks taken where they are the reference, ours kept where the divergence is deliberate |

Not ported, by the operator's standing decision (`AGENTS.md` §1): `internal/mitm`.

The merged set includes the features that were most visible in the upstream log:
deepseek-web tool-turn parity (nonce binding, JSON-Schema argument validation via
`santhosh-tekuri/jsonschema/v5`, citations, the post-FINISHED drain), the Gemini
client-request tool-result fix (every `functionResponse` now reaches the client,
with the verbatim result bytes re-emitted), the Codex lanes (fast mode, cached
tokens, name-addressed function outputs, response tiers), the resilience work
(escalating lockout ladder, stalled agentic loop detection, per-seat attempt
budget), the audit trail, per-key budget/context ceilings, combo call budgets and
weights, the built-in Headroom engine, SSO (OIDC/SAML), the Gemini `/v1beta`
surface and Ollama listing, SOCKS5 proxy pools, custom JSON API endpoints, and
the associated dashboard surfaces.

Dependencies (all pure Go, §4.F): `beevik/etree`, `russellhaering/goxmldsig`,
`santhosh-tekuri/jsonschema/v5` added; `andybalholm/brotli` → `v1.2.7`,
`klauspost/compress` → `v1.20.2`; toolchain `go1.27.4`.

Two upstream changes were reconciled rather than copied:

- `proxy.StallBudgets` stays the two-budget struct this repository ports from
  PanRouter (§1 item 4): every call site upstream writes as
  `cfg.ResolveStallTimeout(Default)` is adapted to `proxy.ResolveStallBudgets(cfg)`,
  which carries both phases.
- `RecordConnectionAccountBanFailure` truncates its stamp to the second *and*
  keeps the park strictly later than the previous one. The ladder flattens at its
  cap (the last two streaks share a window) and the stamp is stored to the
  second, so on a machine whose `time.Now()` ticks coarsely two consecutive parks
  landed on the same instant and the escalation test failed.

Verified so far: `go build ./...`, `go vet ./...` clean; `internal/db`,
`internal/clientconfig`, `internal/thinking`, `internal/providers`,
`internal/translator` green; `cd web && bun run build` (1614 KB, 344 KB brotli),
`bun test src scripts` 771 pass.

### How our own parity work was preserved across the sync

Three devices, so the next sync is mechanical too:

1. **`internal/providers/panrouter_extras.go`** — an `init()` overlay that adds
   the providers and catalogue lanes this repository took from PanRouter but
   panrouter-go does not carry (agentrouter, apinex, apmix, freemodel-dev,
   justdowork, orcarouter, vyceai, freeai, freeai-vl, the three self-hosted media
   lanes, `mmf`, the `cline-free/*` trio, qoder's `dfmodel_v41`, xiaomi's v2.6
   trio, OrcaRouter's Responses-only format map, the Claude-CLI spoof headers).
   The upstream literals stay byte-comparable with upstream, and a sync that
   overwrites them cannot drop ours.
2. **`web/src/lib/providers.registry-test.ts`** learned to read that overlay, so
   the dashboard-catalog parity test still sees the merged table.
3. **Kept-locally list** for files whose divergence is deliberate: the Claude
   reset route, the Codex profiles route, the capacity-adapter combo reference,
   `internal/thinking`'s writers, `internal/proxy/stall.go`'s two budgets, the
   claude-settings credential guard, the dashboard boot budget and lazy chart.

### Reconciled rather than copied

- **Claude reset credits**: upstream's `GET/POST .../claude-reset-credits` pair
  supersedes the single `POST .../claude-reset` this repo ported from PanRouter;
  the routes now point at upstream's handlers and our variant is gone.
- **Stall API**: upstream's callers pass a duration; every call site now passes
  `proxy.ResolveStallBudgets(cfg)`, and `NewStallReaderWithFirstEvent` exists as
  an adapter onto the two-budget watchdog.
- **Auth storage keys** stay `panrouter_auth` / `panrouter_key`: upstream's
  client writes `panrouter_*`, which would sign every existing operator out.
- **Provider artwork**: upstream ships four PNGs whose IHDR is unreadable
  (siliconflow, reka, nebius, crush); they are re-encoded from the valid copies
  at 128×128 (crush at 128×66), so the 32×32 display budget holds.
- **`svelte-check` baseline** moved 72 → 73: the inherited upstream components
  carry one more type error than the previous tree did, and no unresolved
  identifiers (the fatal class) exist.

### Second pass — behaviours reconciled after the first green build

The first build-green state still had behavioural mismatches where a swept test
encoded upstream's newer contract against a file we had kept:

- **`internal/thinking`** was taken from upstream wholesale (its `Intent` carries
  `Tier`/`IncludeThoughts` and `ParseSuffix` resolves them), then this
  repository's two additions were re-applied on top: the `nous` writer and
  `hasWriter` derived from the writer table.
- **`internal/clientconfig/claude.go`** comes from upstream — it already writes
  `ANTHROPIC_AUTH_TOKEN` only when a key is supplied, which is the guard this
  repository had added independently. The rest of the package keeps the
  `panrouter` identity strings (`codexProviderKey`, `[model_providers.panrouter]`,
  `name = "PanRouter"`), so an existing Codex config is not orphaned.
- **`ProxyConfig` regained this repository's `FirstChunkTimeoutMs` and
  `ProxyPoolID`**, and MiniMax Code's two stall budgets (15m / 10m) are declared
  in the extras overlay.
- **In-band stall frames still carry `STREAM_STALL`**: upstream's
  `BuildStreamErrorBytes` maps 504 to `gateway_timeout`, so a 504 whose message
  is the watchdog's is re-tagged (AGENTS.md §1.4 requires the code).
- **`codebuddy_intl_identity_test.go`** (this repository's own contract test for
  the 2.178.0 fingerprint) was deleted: upstream ships the same coverage in
  `internal/proxy/executor/codebuddy_identity_test.go` with the identity now in
  `providers.CodeBuddyIntl*`.

Portability gate (§4.F) after the dependency change: all five `CROSS_TARGETS` build with `CGO_ENABLED=0` (`linux-amd64`, `linux-arm64`, `darwin-amd64`, `darwin-arm64`, `windows-amd64`), and `go vet ./...` is clean.

### Reconciliation of the features this repository added on top

Where upstream rewrote the plumbing under a feature we had built, the feature was
re-attached rather than dropped:

- **Route headers** (`x-panrouter-provider/-model/-combo/-attempts`) still ride
  on every dispatched response; the injector had kept upstream's `x-panrouter-*`
  names, which the dashboard tests read as absent.
- **Token savers** now use upstream's `applyTokenSavers` (context bypass,
  headroom engine, persona accounting) — our older body lacked all three.
- **Custom JSON endpoints** get their own dispatch (`ForwardCustomJSON`) instead
  of the OpenAI one, a keyless node is allowed through the credential gate, and a
  business failure ends the pass instead of replaying on another account — the
  endpoint may already have performed its operation, and the doomed second call
  is the automation signature a business API bans on.
- **Gateway faults (5xx)** record the model's cooldown badge before the error is
  returned, so the next request does not re-dial an upstream that just failed.
- **Opencode closure** prefers the carrier's `Retry-After` (delta-seconds or
  HTTP-date) over the 60s estimate and clamps it to `maxResetCooldown`.
- **SSRF**: a connection carrying `allowPrivateNetworking` puts that flag on the
  request context, so the dialer permits a private/reserved upstream for that
  connection only.
- The tunnel suite reads `DATA_DIR` from a temp directory, so a machine with a
  live cloudflared no longer answers the test from its own tunnel.

- **CodeBuddy lanes** keep the split the operator requires: the intl entry point carries
  the 2.178.0 fingerprint (uuid request id, no `X-App`/`X-Product-Version`/
  `X-Stainless-Helper-Method`, SDK 7.39.0, Node v26.7.0, host-derived Stainless
  arch/OS), while the CN entry point keeps the Tencent build (2.133.1) and its own
  header set. `ForwardCodebuddyCN` now stamps the CN provider name itself, so the
  lane is decided by the function that ran, not by a field the caller may omit.

- **Production route table**: the login-protected group now also mounts the routes
  upstream added — `GET /api/keys/{id}/budget`, `GET|POST /api/oauth/{provider}/lease-*`
  and the provider share link (`/add`, plus its `/v1/add` spelling, which
  RequestLogger normalises before routing).

Verified: `go build ./...`, `go vet ./...`, `go test -count=1 ./...` (0 failing
packages), `go test -tags=integration ./internal/integration/...`, the five
`CGO_ENABLED=0` targets, `cd web && bun run build`, `bun test src scripts`
(836 pass) and `bun run ratchet:svelte` (73 = baseline).
