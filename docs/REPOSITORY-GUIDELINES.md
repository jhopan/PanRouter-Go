# Repository Guidelines

Practical map of `PanRouter-Go` for AI assistants and new contributors: what the
code is, where things live, how to build and test it, and which patterns are
non-negotiable.

**This file is a map, not the law.** [`AGENTS.md`](../AGENTS.md) is the binding
engineering contract — §0 lineage, §1 parity protocol and open drift, §3 provider
isolation, §4 Go rules, §5 testing, §6 Svelte 5, §7 PR workflow, §9 "never" list.
Where this file and `AGENTS.md` disagree, `AGENTS.md` wins; where `AGENTS.md` and
the tree disagree, the tree is what compiles — see [Drift](#drift-and-gotchas).

## Project Overview

A single-binary LLM API gateway: it fronts 100+ upstream providers and many
accounts, and speaks the OpenAI Chat Completions, Anthropic Messages, Gemini and
OpenAI Responses wire formats at the edge. Supporting systems are provider
connections, combo/fallback routing, proxy pools, per-key and per-node quota,
usage accounting in SQLite, and an embedded Svelte 5 dashboard served from the
same process.

- **Module**: `panrouter/proxy` (`go.mod:1`); Go `1.27`, toolchain `go1.27.3` (`go.mod:3`, `go.mod:5`).
- **Binary**: `panrouter-go` on disk (`Makefile:2`), but `binaryName()` reports `panrouter-go`, or `opencode-go` when invoked under that name (`cmd/panrouter-go/main.go:26-31`).
- **Behaviour reference**: PanRouter (Node) — `C:/Users/ACER/Documents/Project/Panrouter`. This repo *reproduces*; a fix landed here first is drift. See `AGENTS.md` §0–§1.
- **Lineage**: panrouter (decolua and contributors) and panrouter-go (Luqmanul Hakim), both MIT. Their notices in `LICENSE` are the entire attribution and must never be removed.
- **Deployment target**: a 1 vCPU box with a small memory ceiling (`AGENTS.md` §4.G). Streaming instead of buffering, bounded caches, no per-request goroutine without a lifetime, and a pooled `http.Client` are correctness requirements here, not polish.

## Architecture & Data Flow

### Boot and dependency injection

`main` → `internal/app/app.go` `NewApp`, which composes four fx modules:
`ConfigModule` (`internal/app/config.go`), `DatabaseModule`
(`internal/app/database.go`), `HandlersModule` (`internal/app/handlers.go`),
`ServerModule` (`internal/app/server.go`).

- CLI: `urfave/cli/v2`, subcommands `run` (default), `daemon`, `service`, `menu` (`cmd/panrouter-go/`).
- `ProvideTokenSaverConfig` resolves RTK/Caveman/Ponytail/ADHD from CLI flags, then DB settings, then defaults (`internal/app/handlers.go:26`).
- `ProvideRouter` builds the chi router with `RequestID`, `MaxBodyByPath`, `Recoverer`, `RequestLogger` and hands it to `handlers.SetupServerRouter` (`internal/app/handlers.go:69`).
- `ProvideServer` owns lifecycle: 15s start timeout, graceful stop on first SIGINT/SIGTERM, `os.Exit(1)` on the second; restart-after-stop goes through `internal/shutdown` because Windows cannot rebind a live port (`internal/app/server.go`).
- fx style: injected dependencies are never nil-checked defensively — a missing provider must fail at container start.

### Route table, in two layers

1. `internal/handlers/router.go` `SetupServerRouter` (`:505`) builds middleware groups and calls the two route registrars:
   - public: `/health`, version GETs, `/dashboard` page;
   - `RequireDashboardPage` (`:530`, `:572`), `RequireAdminAuth` (`:584`, also gates pprof behind `PPROF_ENABLED=true`);
   - **engine group** (`:649-652`): `RequireApiKey` + `RequireRateLimit` + `guardrails.Inbound` → `handlers.SetupRoutes`;
   - dashboard group (`:691`): `RequireDashboardAuth` → `SetupDashboardRoutes`.
2. `internal/handlers/router.go` `SetupRoutes` (`:43`) mounts the `/v1`-compatible surface: `/chat/completions`, `/messages`, `/messages/count_tokens`, `/responses`, `/responses/compact`, `/embeddings`, `/rerank`, `/images/*`, `/audio/*`, `/videos/*`, `/models*`, `/changelog` (`:59-110`). Dashboard REST lives in `internal/handlers/dashboard/routes.go` `RegisterRoutes` (`:92`), all under `/api/*`; batches/files in `internal/handlers/batches/`.

**Invariant**: exactly one `*chat.ChatHandler` exists per process. `SetupRoutes`
creates it, returns it, and the dashboard must receive *that* instance — a second
one would be a second persistent semantic cache over the same SQLite file
(`internal/handlers/router.go:36-42`, `internal/app/server.go:57`).

### Chat request path

```mermaid
flowchart TD
  A[POST /v1/chat/completions] --> B[HandleChatCompletions<br/>chat/chat.go:49]
  B --> C[resolveModel / resolveModelEntryGuarded<br/>chat/resolution.go:116,256]
  C --> D[model access policy per API key<br/>chat/access.go:81,239,326]
  D --> E[handleAccountFallback<br/>chat/fallback.go:61]
  E --> F[tryForwardWithConnection<br/>chat/fallback.go:532]
  F --> G[connection select + pool + session affinity<br/>chat/connections.go]
  G --> H[executor registry Get(provider)<br/>proxy/executor/executor.go:70]
  H --> I[translator encode<br/>internal/translator]
  I --> J[upstream call: pooled client<br/>internal/proxy/*transport*.go]
  J --> K[SSE adapter / jsonResponse<br/>executor/openai.go:221,258,445]
  K --> L[usage accumulator]
  L --> M[Repo.InsertUsageHistory<br/>db/usage.go:54]
```

- Provider selection is registry-driven: executors register themselves by provider name (`executor.Register`, `internal/proxy/executor/executor.go:63`) and are looked up with `Get`, falling back to `Default() == ForwardOpenAI` (`internal/proxy/executor/openai.go:139`). An executor is a plain `func(w http.ResponseWriter, req *Request) error`.
- Failure handling: account rotation/fallback inside `handleAccountFallback`; the Antigravity strike-breaker blocks a connection+model for 15 minutes after 3 quota-429s within 60s and only counts 429s that carry an explicit quota marker (`internal/handlers/chat/antigravity_quota.go:51-55`, `:398-428`); credential locks live in `internal/handlers/chat/oauth_lock.go`; breaker/semaphore admission in `internal/handlers/chat/resilience_admit.go` + `internal/resilience`.
- OAuth token refresh is single-flighted per connection (`internal/proxy/oauth/`).
- Usage rows are written by the usage writer (`internal/handlers/chat/usage_writer.go`) into `usageHistory`; read paths that serve the dashboard are batched (`internal/db/quota_reset.go`, `internal/db/usage.go`).

### Dashboard and embedded assets

The SPA is compiled to `web/dist` and embedded with `//go:embed dist/*`
(`web/embed.go:18-19`); `web.Handler()` (`web/embed.go:35`) negotiates
`Accept-Encoding`, prefers the build-time `.br`/`.gz` sibling, sets
`Vary: Accept-Encoding`/`Content-Length`, keeps range support, and falls back to
`index.html` for client-side routes. Frontend code lives in `web/src/`
(`App.svelte`, `components/`, `lib/`, `api/client.ts`).

## Key Directories

| Path | Purpose |
|---|---|
| `cmd/panrouter-go/` | Gateway binary: `main.go`, `daemon_cmd.go`, `service_cmd.go`, `menu.go`, `default.pgo`. |
| `cmd/panrouter-tray/`, `cmd/changelog-merge/` | Tray companion (build-tagged, outside `CROSS_TARGETS`) and the release-time changelog CLI. |
| `internal/app/` | fx wiring, router construction, server lifecycle. |
| `internal/handlers/` | `router.go` route table; `chat/` engine (~200 files); `dashboard/` REST+SSE; `media/`; `batches/`; `oauth/`; `sso/`. |
| `internal/middleware/` | Auth gates (`RequireApiKey`, `RequireDashboardAuth`, `RequireAdminAuth`), rate limiting, body limits, logging, compression. |
| `internal/guardrails/` | Inbound/outbound scanning middleware, detectors, policy resolution. |
| `internal/translator/` | Protocol converters (OpenAI ↔ Claude Messages ↔ Gemini ↔ Responses) and usage parsing. |
| `internal/proxy/` | Transports, SSE plumbing, `executor/` (one file per provider family), `oauth/`. |
| `internal/providers/` | Provider registry, model catalogs, aliases, OAuth metadata. |
| `internal/db/` | SQLite layer (`modernc.org/sqlite`), schema/index bootstrap, usage and quota queries. |
| `internal/tokensaver/` | RTK / Caveman / Ponytail / ADHD prompt compression. |
| `internal/observ/`, `internal/log/` | Prometheus metrics and structured logging with a console ring buffer. |
| `internal/integration/` | Tagged feature suite + `bootfx/` full-fx boot test. |
| `web/src/`, `web/scripts/` | Svelte 5 dashboard and its build tooling (fingerprint rebuild, pre-compression, font vendoring, svelte-check ratchet). |
| `docs/`, `.changes/`, `version.json`, `VERSION` | Design/plan docs, pending changelog fragments, version manifest. |

## Development Commands

| Command | What it does |
|---|---|
| `make build` | `web-build`, then `go build` → `panrouter-go` and `opencode-go`. |
| `make run` | Build + run the gateway (default `PORT=20130`). |
| `make dev` | `go run ./cmd/panrouter-go/` with token-saver flags. |
| `make web-dev` | Vite HMR on `:5173`, API proxied to `:20130`. |
| `make web-build` | `bun web/scripts/web-build.ts`: fingerprint-guarded SPA build (no-op when fresh, `FORCE=1` to force). |
| `make test` / `test-short` | `go test ./... -v` / `go test ./...`. |
| `make test-race` | `go test -race -count=1 -timeout 10m ./...` (needs cgo). |
| `make test-integration` | `web-build`, then `go test -tags=integration -race -count=1 ./internal/integration/...`. |
| `make test-live` | `PANROUTER_LIVE_TESTS=1 go test ./internal/handlers/chat/ -run 'Live\|MuseSpark'` — local only, needs real credentials. |
| `make vet` / `make vet-integration` | `go vet ./...` / `go vet -tags=integration ./internal/integration/...`. |
| `make vet-svelte` | `bun install --frozen-lockfile && bun run ratchet:svelte` (svelte-check ratchet). |
| `make cross` | Builds the five `CROSS_TARGETS` binaries and writes `SHA256SUMS.txt`. |
| `make changelog-merge TAG=v1.9.17` | Files `.changes/*.md` into `CHANGELOG.md` (release time only). |
| `make clean`, `make help` | Remove binaries + `web/dist`; list targets. |
| `cd web && bun run build` | `tsc -b && vite build && bun scripts/precompress.ts`. |
| `cd web && bun test src scripts` | Frontend unit tests (bun:test). |
| `cd web && bunx oxlint --deny-warnings` | Frontend lint (CI gate). |
| `go test ./web/...` | Pins the embedded-asset/compression contract; needs `web/dist` built. |

CI equivalents worth reproducing before a PR: `go vet ./...`, `go test -count=1 ./...`,
`make test-integration`, `bun test src scripts`, `make vet-svelte`, plus
`golangci-lint run --new-from-rev="$(git merge-base origin/main HEAD)" ./...`
(the lint step is a diff ratchet). See `.github/workflows/ci.yml`.

## Code Conventions & Common Patterns

**Provider isolation is the first rule** (`AGENTS.md` §3). Never merge,
cross-alias or hijack providers; never pick a provider by inspecting a model
string (`if strings.Contains(model, "muse-spark")`); never add a
`provider == "x"` fallthrough into a sibling. Routing comes from the catalog,
the `provider/model` prefix, aliases in `internal/providers/aliases.go`, or
custom provider nodes — nothing else. Provider-specific quirks stay inside that
provider's executor.

- **Closed sets get named types**, never repeated literals (`AGENTS.md` §4.C); open-ended values (IDs, URLs, wire literals) stay strings.
- **Errors**: wrap with caller context — `fmt.Errorf("CreateBatch: %w", err)` (`internal/db/batches.go:68`); compare with `errors.Is`/`errors.As`, never `strings.Contains(err.Error(), …)`; typed errors expose `Is*` helpers (`internal/handlers/chat/access.go:30-33`, `:66-69`). No panics in handlers, services or executors — write an HTTP/SSE error payload instead.
- **Context everywhere**: pass `r.Context()` down to DB and upstream calls so a client disconnect aborts the work; every goroutine has an exit condition (context, channel, or a bounded lifetime).
- **Shared state**: unexported `sync.Mutex`/`RWMutex` guarding every map used concurrently, `defer mu.Unlock()` immediately after locking (e.g. the executor registry, `internal/proxy/executor/executor.go:58-66`). Collapse duplicate concurrent work (token refresh, catalog sync) with `singleflight`.
- **SQLite reads are batched and plan-pinned**: one statement per page (`IN (...)` list or `VALUES` CTE — see `GetAllowedModelsForKeys`, `GetPlansForKeys`, `GetApiKeyTokenUsageForKeys`), answered from an index (`internal/db/schema.go:173-174`), never 1+N inside a loop; hoist correlated subqueries out of per-row predicates; check `EXPLAIN QUERY PLAN` and pin it with a test (`AGENTS.md` §4.A).
- **Hot path**: stream through `io.Reader`/`bufio` and forward opaque JSON as `json.RawMessage`/`jsontext.Value`; no whole-body buffering unless the body is being repaired; reuse pooled clients, buffers and transports (`internal/proxy/*transport*.go`).
- **File shape**: cognitive complexity ≤ 15 per function, guard clauses over nesting, target ≤ 300 LoC per file, `github.com/samber/lo` for slice/map plumbing (`internal/db/connection_reorder_test.go:7`).
- **Frontend (Svelte 5 runes only)**: `$props()` with an explicit typed interface, `$state`/`$derived`/`$effect`/`$bindable`, `{@render children?.()}` instead of `<slot>`, `onclick={…}` instead of `on:click`, keyed `{#each … (item.id)}`. All HTTP goes through `web/src/api/client.ts`; styling uses the tokens declared in `web/src/index.css` (`bg-surface-2`, `text-text-muted`, `border-border`, `bg-brand-500`, `rounded-[10px]`); icons are Material Symbols (`<span class="material-symbols-outlined">`) or `lucide-svelte`.
- **Changelog**: one `.changes/<pr-number>-<slug>.md` per PR; never touch `CHANGELOG.md` (`AGENTS.md` §7.6).

## Important Files

| File | Why it matters |
|---|---|
| `AGENTS.md` | Binding rules; read before any non-trivial change. |
| `Makefile` | Targets, `CROSS_TARGETS`, version embedding, cross-compile. |
| `cmd/panrouter-go/main.go` | Entry point, CLI subcommands, binary aliasing. |
| `internal/app/app.go`, `handlers.go`, `server.go` | fx graph, router assembly, lifecycle. |
| `internal/handlers/router.go` | The whole route table and auth grouping. |
| `internal/handlers/chat/chat.go` | `HandleChatCompletions`/`HandleMessages`/`HandleResponses`. |
| `internal/handlers/chat/fallback.go` | Fallback loop and per-connection forwarding. |
| `internal/handlers/chat/access.go` | Per-key model policy (fails closed). |
| `internal/handlers/chat/resolution.go` | Model/combo → concrete `ModelInfo`. |
| `internal/handlers/chat/connections.go` | Connection selection, rotation, affinity, proxy pools. |
| `internal/proxy/executor/executor.go` | Executor registry contract (`Register`/`Get`/`Default`). |
| `internal/translator/types.go` | Wire-format contracts shared by translators and executors. |
| `internal/db/schema.go` | Table/index bootstrap and `supersededIndexes`. |
| `internal/integration/harness_test.go` | `newEnv`, the fixture every feature test builds on. |
| `internal/integration/upstream_test.go` | Fake upstream, connection/combo seeding, request assertions. |
| `web/embed.go` | Embedded SPA serving + compression negotiation. |
| `web/src/api/client.ts` | The only frontend HTTP surface. |
| `.github/workflows/ci.yml` | The gates a PR must pass. |

## Runtime/Tooling Preferences

- **Go 1.27** (`go.mod` pins toolchain `go1.27.3`); build with `CGO_ENABLED=0` — cgo is only acceptable for `make test-race`.
- **Bun is the frontend runtime and package manager** (CI pins `1.4.2`; `web/bun.lock`; `--frozen-lockfile`). Never `npm`/`yarn`/`pnpm`; Node is only a `@types/node` dependency.
- **Portability is a gate, not a preference.** `CROSS_TARGETS := linux-amd64 linux-arm64 darwin-amd64 darwin-arm64 windows-amd64` (`Makefile`); after any `go.mod` change run:
  ```bash
  for pair in $(grep -E '^CROSS_TARGETS *:=' Makefile | sed 's/.*:=[[:space:]]*//'); do
    CGO_ENABLED=0 GOOS=${pair%-*} GOARCH=${pair#*-} \
      go build -o /dev/null ./cmd/panrouter-go/ || echo "FAIL $pair"
  done
  ```
  followed by `go mod tidy` (must leave `go.mod` unchanged, new deps listed as direct). A cgo dependency, or a build tag that breaks any pair, is refused (`AGENTS.md` §4.F). `web/dist` must exist first because `web/embed.go` uses `//go:embed dist/*`.
- **Makefile recipes must survive cmd.exe**: GNU Make `export` instead of `VAR=x cmd` prefixes, `$(file <VERSION)` instead of `cat VERSION 2>/dev/null`, and no POSIX-only conditionals. This host is Windows, so those comments are load-bearing.
- **`rtk` prefix**: `CLAUDE.md` and `AGENTS.md` §8 tell agents to prefix shell commands with `rtk`; if `rtk` is not installed, run the plain command.
- **Ports and data**: gateway `:20130` (`PORT`), Vite dev `:5173`; data dir `~/.panrouter` (`%APPDATA%\panrouter`) with the DB at `DATA_DIR/db/data.sqlite` or `DB_PATH` (`internal/config/config.go:79-107`, `:172-186`).
- **Runtime knobs**: `PORT`, `DATA_DIR`, `DB_PATH`, `RTK_ENABLED`, `CAVEMAN_ENABLED`, `PONYTAIL_ENABLED`, `ADHD_ENABLED`, `AUTO_UPDATE`, `INJECTION_GUARD_DISABLED`, `PPROF_ENABLED`, `ALLOW_PRIVATE_NETWORKING`, `JWT_SECRET`, `PANROUTER_LIVE_TESTS`.
- **Dependency rule**: a new dependency is allowed if it is pure Go and keeps the module graph coherent — judge it by measurement, not by size.

## Testing & QA

**Frameworks**: stdlib `testing` only (no Ginkgo/Gomega/testify), `net/http/httptest` for HTTP, `bun:test` for the frontend, Playwright for dashboard E2E. Table-driven subtests with `t.Run` are the default shape; `t.Fatalf` when later checks are meaningless, `t.Errorf` for independent fields.

**Layers**:

| Layer | Location | Notes |
|---|---|---|
| Go unit | `*_test.go` next to the code | Temp-file SQLite (`t.TempDir()/data.sqlite`), never a real upstream. |
| Go feature integration | `internal/integration/` (`//go:build integration`) | Boots the production chi router + real schema against a fake upstream. `make test-integration`. |
| Full fx boot | `internal/integration/bootfx/` | The only place `app.DatabaseModule` is booted — `db.InitGlobalDatabase` is a process-wide `sync.Once` and fx stop closes it. |
| Frontend unit | `web/src/**/*.test.ts`, `web/scripts/*.test.ts` | `bun test src scripts`. |
| Frontend type ratchet | `web/scripts/svelte-check-ratchet.ts` | Fails on unresolved identifiers; total error count pinned in `svelte-check-baseline.json` and may only shrink. |
| E2E | `web/e2e/` | `bun run e2e` against a running gateway. |
| Live upstream | `internal/handlers/chat/live_*_test.go`, `internal/integration/live_*_test.go` | Env-gated (`PANROUTER_LIVE_TESTS`, `SAKOO_LIVE_KEY`, `NVIDIA_LIVE_KEY`, `PANROUTER_LIVE_OPENCODE`); CI never sets them. |

**Harness rules** (`internal/integration/harness_test.go`, `doc.go`):

- `newEnv(t)` → `newEnvWithTokenSaver` opens a private DB, applies `EnsureCoreSchema` + `EnsureUpstreamLeases`, seeds `sk-integration-test-key`, pins `DATA_DIR`/`JWT_SECRET`, and mounts `app.ProvideRouter` + `httptest.NewServer`.
- Every helper takes the running `*testing.T` as its first argument; `Env` deliberately holds no `*testing.T`.
- No test may reach the network: `env.NewUpstream(t, …)` supplies the fake, `env.AddConnection` seeds `providerConnections.data.baseUrl` and reads the row back to prove it persisted. Handler packages seal the default transport with `internal/testnet` in their `TestMain`.
- Assert observable behaviour (status a client sees, model the upstream received, row written to `usageHistory`), not that a route or helper exists.

**Query-plan pins**: batched dashboard reads must be answered from an index; the plan is asserted in tests such as `TestGetApiKeyTokenUsageForKeysQueryPlan` and the client-key index checks in `internal/db/`. A `SCAN` over `usageHistory` on a large table is a bug, not a benchmark result.

## Drift and Gotchas

- **Directory names lag the rename.** The module is `panrouter/proxy` and `UPSTREAM`/docs speak of `panrouter-go`, but the tree keeps `cmd/panrouter-go/`, `cmd/panrouter-tray/` and the binary name `panrouter-go`. `AGENTS.md` §2/§4.F/§8 reference `cmd/panrouter-go/`, which does not exist in this tree — use `./cmd/panrouter-go/` in every command.
- **Version embedding is mismatched.** `Makefile:43` passes `-X "panrouter/proxy/internal/updater.CurrentVersion=…"` while the module path is `panrouter/proxy`; the correct symbol form is documented at `internal/updater/updater.go:32`. `scripts/bump-version.sh` already writes the `panrouter/proxy` form.
- **Docs carry stale versions.** `VERSION` is `1.9.16-exp.1`, but `ARCHITECTURE.md:7` claims v1.9.9 and `COMPARISON.md:3` claims v1.9.1; `COMPARISON.md:103` itself flags the inconsistency. Trust `VERSION`, `version.json` and `internal/updater/updater.go:34`.
- **Two legacy data-dir spellings remain in tests/comments** (`~/.panrouter`): the live-test helpers and some Makefile comments predate the `~/.panrouter` default; the runtime default in `internal/config/config.go` is authoritative.
- **Graduated rules, not clean trees**: `golangci-lint` runs diff-only (141 pre-existing findings) and `svelte-check` is a ratchet (~75 pinned errors). Do not "fix" the baseline opportunistically in an unrelated PR, and never widen it.
- **Branch discipline**: every PR targets `main` and must branch from clean `origin/main`; `.changes/` fragments are the only changelog write surface.
