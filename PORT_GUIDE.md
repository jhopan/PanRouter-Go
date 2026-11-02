# PanRouter-Go — port guide

Go port of **PanRouter**. Single binary, embedded dashboard, no Node.js at runtime.

This repository exists for one reason: PanRouter's behaviour — which providers
work, which headers the CLI sends, which error means what — is the source of
truth, and this repo reproduces it in Go, faster and lighter. It is **not** a
fork to be maintained independently. When PanRouter changes, this repo follows.

---

## Lineage and credit

This codebase is a cleaned fork of two MIT projects. Both copyright notices stay
in `LICENSE` — do not remove them.

| Project | Author | What we took |
|---|---|---|
| **[9router](https://github.com/decolua/9router)** | decolua and contributors | The original router: provider registry, translators, executor design, dashboard, the whole idea |
| **[9router-go](https://github.com/luqman-v1/9router-go)** | Luqmanul Hakim | The Go port this repository started from — the proxy engine, the embedded Svelte dashboard, the benchmarks |

Behaviour reference: **[PanRouter](https://github.com/jhopan/PanRouter)**, which
is itself derived from 9router.

The git history of 9router-go was intentionally dropped when this repo was
created — the code is carried forward, the history is not. Attribution lives in
`LICENSE`, `README.md` and here.

---

## The rule: PanRouter is the reference, this is the port

**PanRouter (Node) is where behaviour is decided. This repo reproduces it.**

```
PanRouter (JS, Node)        <- source of truth: behaviour, headers, error handling
      |
      |  port the change, by hand
      v
PanRouter-Go (Go, binary)   <- reproduction: same behaviour, ~6x less RAM
```

Concretely:

- **A bug fixed in PanRouter is not fixed here** until it is ported. The two code
  bases drift silently; assume nothing.
- **A provider that changed its wire format** is updated in PanRouter first —
  that is where the MITM capture and the live test live — then mirrored here.
- **Never invent behaviour here.** If PanRouter does X and this repo does Y, that
  is a bug in this repo, even if Y looks better. Fix PanRouter first, then port.
- **When porting, port the comment too.** The comments in PanRouter explain *why*
  a header exists, why an error is classified a certain way, why a lock lasts a
  certain time. That reasoning is the valuable part; the code is the easy half.

### What "port" means

| PanRouter file | PanRouter-Go counterpart |
|---|---|
| `open-sse/executors/*.js` | `internal/proxy/executor/*.go` |
| `open-sse/providers/registry/*.js` | `internal/providers/*.go` |
| `open-sse/translator/**` | `internal/translator/**` |
| `open-sse/handlers/chatCore*` | `internal/handlers/chat/*.go` |
| `open-sse/services/tokenRefresh/**` | `internal/auth/`, `internal/proxy/` |
| `open-sse/services/*CapCache.js` | `internal/*/cache*.go` |
| `src/sse/services/auth.js` | `internal/handlers/chat/fallback.go` |
| `src/lib/apiKeyLimits.js` | `internal/auth/`, `internal/handlers/dashboard/` |
| `src/app/(dashboard)/**` | `web/src/**` (Svelte) |

---

## Known drift — port these first

These are fixed in PanRouter and **not yet** in this repo. They are the reason
accounts were getting banned, so they matter more than any feature.

### 1. CodeBuddy CLI identity is stale

| | PanRouter | This repo |
|---|---|---|
| `codebuddy.go` `cliVersion` | `2.178.0` | `2.108.1` |
| `codebuddy_identity.go` `codebuddyCLIVersion` | `2.178.0` | `2.133.1` |

The 2.178.0 identity sends `Accept: application/json`, `Content-Encoding: gzip`
on the request body, `X-User-Id` from the token's `sub` claim, Stainless SDK
`7.39.0`, Node `v26.7.0`, and **no** `X-App`, `X-Product-Version` or
`X-Stainless-Helper-Method` — the official CLI never sends those, and sending
them is itself a signal.

Port the whole identity, not just the version string. A wrong header set is what
the gateway flags.

### 2. A rate limit must stop the rotation, not walk the pool

PanRouter: a 429 on CodeBuddy Intl gets **zero retries**, the account is locked
until the provider's own reset time **plus ten minutes**, and the rotation
**stops** — the remaining accounts are never touched.

Observed here: `internal/handlers/chat/fallback.go` appends the account to
`excludeIDs` and continues. That is the pattern that produced
`11140 request illegal` and 30-day bans. 29 requests in two seconds across 29
accounts **is** the automation signature.

### 3. The reset timestamp carries its own offset, plus a margin

`"your usage will reset at 2026-11-02 10:05:00 UTC+8"` becomes a lock until
`10:15 WITA`. The offset comes from the body — the server runs WIB (UTC+7), the
account is on WITA (UTC+8), and 10:05 UTC+8 is 02:05 UTC either way. Building the
epoch from the server's local zone shifts the lock by an hour.

The ten-minute margin exists because the request in flight, the clock skew
between the box and the gateway, and the moment the counter rolls over are not
the same instant. Asking one second early earns another refusal.

### 4. Stream watchdog: two budgets, not one

PanRouter separates the **first-chunk** budget (slow prefill on a 120k-token
context) from the **inter-chunk** budget (silence after tokens start flowing).
One budget for both aborted healthy MiniMax turns. MiniMax also carries its own
`stallTimeoutMs: 15m` and `firstChunkTimeoutMs: 10m` in the registry.

The abort message names the phase and carries the numbers —
`stream stalled after first chunk (no data for 412000ms, budget 900000ms)` — and
the error carries `code: "STREAM_STALL"`, so a caller can tell a slow provider
from a dead one.

### 5. Auto-ping must be off unless switched on

PanRouter uses `CODEBUDDY_AUTOPING_ENABLED === "1"` — an env var that has to be
SET, not one that has to be unset. The daily chat burst is what got the accounts
flagged in the first place. Do not enable it by default here either.

---

## Conventions

- **Go 1.27.** `go.mod` pins the toolchain; `go vet` downloads it.
- **CGO-free.** The binary must cross-compile to any platform — that is the whole
  point. No `cgo` imports.
- **Config over hardcode.** Provider definitions live in `internal/providers/`,
  constants in `internal/constants/`, never inline in a handler.
- **Tests next to the code** (`*_test.go` in the same package). The repo ships
  ~500 test files; keep that ratio.
- **Embedded dashboard** (`web/embed.go`) — the Svelte app is built and embedded,
  not served from disk.
- **Comments explain why.** A header exists because the gateway rejects requests
  without it; a lock lasts ten extra minutes because the clock skew is real.
  Recording the reason is the difference between a port and a copy.

## What NOT to port

| PanRouter feature | Why not |
|---|---|
| MITM interception | Not used in this deployment |
| Tunnel (Cloudflare / Tailscale) | Not used in this deployment |
| Next.js dashboard | Replaced by the embedded Svelte app |
| `better-sqlite3` | Go uses `modernc.org/sqlite` (pure Go, CGO-free) |

The operator said so explicitly: MITM and tunnel are out of scope. If they are
needed later they are added deliberately, not by copying.

---

## Release

Same rule as PanRouter: **a release ends at the GitHub tag.** Deploying to a
server is a separate, explicit instruction — never fold it into a release.

```bash
make build          # -> panrouter-go(.exe)
make test
git tag v0.x.y && git push origin v0.x.y
```

## Never

- **Never touch a remote server without an explicit, absolute instruction for
  that exact change.** "Coba cek", "jelaskan" and a pasted log are not permission
  to SSH in and change something. Read-only inspection is fine; mutation is not.
- **Never remove the copyright notice** in `LICENSE` — it is the entire
  attribution for two MIT projects.
- **Never port a change here before it is settled in PanRouter.** This repo
  reproduces; it does not decide.
