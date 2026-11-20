---
type: fix
title: Return 503 Service Unavailable with Retry-After when all accounts are locked instead of 502 Bad Gateway
---

### Problem
When upstream returned a 429 rate limit or quota exhaustion (e.g. on Antigravity or DeepSeek), the connection was locked with a cooldown (e.g. 2s). When the client SDK retried within that window, `handleAccountFallback` exited early at `!h.Repo.IsProviderAvailable(provider, model)` with a plain `fmt.Errorf("provider %s/%s is unhealthy", provider, model)`. Because the error was not an `*upstreamError`, `chat.go` rendered it as **HTTP 502 Bad Gateway**, causing AI client agents (Claude Code, Cursor, Cline, etc.) to immediately fail the conversation rather than backing off and retrying.

In addition, `connectionModelBlocked` in `connections.go` did not track `lockUntil` into `cooldownUntil`, so `getBestConnectionWithContext` could not report when the earliest lock expired.

### Solution
1. **Upstream Parity (`unavailableResponse`)**: In `internal/handlers/chat/fallback.go`, replaced the premature plain error abort at `!IsProviderAvailable` with `allConnectionsLockedError`, which calculates the shortest active `modelLock_*` remaining duration across connections and returns an `*upstreamError` with `StatusCode: 503` (Service Unavailable), `Retry-After: <seconds>`, and OpenAI-compatible JSON error body (`rate_limit_error`).
2. **Lock Expiry Extraction**: Added `ConnectionModelLockUntil(data, model string)` in `internal/db/accounts.go` and `connectionModelBlockedUntil` in `internal/handlers/chat/connections.go` to extract the lock expiry timestamp directly from connection data without N+1 SQLite queries, populating `cooldownUntil` for `Retry-After` calculation.
3. **Noisy 403 Silencing**: In `internal/handlers/chat/antigravity_experiments.go`, reduced the log level for HTTP 403 on `/v1alpha:fetchConfig` from `log.Warn` to `log.Debug`, as tokens without the internal `aicode` scope naturally fall back to the generic system prompt as designed.
4. **Hermes & OMP Neutralization**: In `internal/translator/antigravity.go`, strengthened prompt sanitization for Hermes and OMP (Oh My Pi) harness markers (`ompIdentityRegex`, `harnessBrandOMPVariants`, `harnessBrandOMPAgent`, and XML tag normalization). All system instructions disguise competing identities into neutral instructions and receive genuine Antigravity client wrapping (`<identity>` block and `<USER_REQUEST>` tagging) to prevent Google Cloud Code from triggering false 429 RESOURCE_EXHAUSTED.
5. **Unit Tests**: Added comprehensive test coverage in `internal/handlers/chat/model_lock_503_test.go` and `internal/translator/antigravity_identity_test.go`.
