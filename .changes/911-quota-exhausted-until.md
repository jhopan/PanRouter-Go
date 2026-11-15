### fix(accounts): persist the quota park and read it for routing and the panel

The tokenharbor lane already parked a spent free allowance for the right window,
but the window was not written anywhere a reader could see. `applyErrorState`'s
Go counterpart locked the account with `rateLimitedUntil` and nothing recorded
*why*: `quotaExhaustedUntil` was only ever **cleared** (`ReleaseConnectionBlocks`),
never written, so the router's own quota state and the dashboard's countdown had
no quota-specific date to show — and a park written by the shared Next.js
dashboard (which writes exactly that field) was invisible to the Go selector.

Mirrored from PanRouter (behaviour reference):

- `src/sse/services/auth.js:422` — `quotaExhausted = isQuotaExhaustedError(errorText) || !!thResetAtMs`,
  the verdict that decides the lock scope and the persisted park.
- `src/sse/services/auth.js:535-537` — the account-vs-model rule is untouched:
  `buildModelLockUpdate((quotaExhausted || githubResetAtMs || kiroMonthly || banned) ? null : model, cooldownMs)`.
- `src/sse/services/auth.js:539-553` — one update: the lock, `testStatus:
  "unavailable"`, `lastError: "Quota exhausted — resets <iso>"`, `errorCode`,
  `lastErrorAt`, `quotaExhaustedUntil`, `backoffLevel`, all from one
  `quotaResetAt` ("One reset timestamp for everything").
- `src/sse/services/auth.js:620-627` — `clearAccountError` drops
  `quotaExhaustedUntil` with the rest of the error state once the account is
  proven usable.
- `open-sse/services/routerQuotaState.js:56` — the whole-account quota park is
  read **first**; a quota refusal need not write a model lock.
- `src/shared/utils/cooldown.js:36-46` — `getCooldownUntil` counts from
  `quotaExhaustedUntil` when it is live and ignores an expired one.
- `src/app/api/usage/[connectionId]/check-release/route.js:75,143` — release
  clears it (the Go `ReleaseConnectionBlocks` already did).

What changed:

- **Write.** `internal/db/accounts.go` gains
  `LockConnectionQuotaExhausted(connID, until, backoffLevel, status)`: one
  `json_set` on `providerConnections.data` (no column) writing
  `rateLimitedUntil` + `quotaExhaustedUntil` at the same instant, `testStatus`
  `providers.StatusUnavailable`, `lastError` in this file's `{status, message,
  timestamp}` shape with the upstream sentence
  `"Quota exhausted — resets <iso>"` (`providers.errorTextOf` flattens it back to
  the string upstream stores), `errorCode`, `lastErrorAt`, `backoffLevel` and
  `status: "error"`. Two writers cannot disagree about the deadline because
  there is one statement and one `until`.
- **Verdict.** `internal/providers/errorclassify.go` now carries `ErrorRule.Quota`
  out of `ClassifyErrorForProvider` as `ErrorClassification.Quota` — the flag the
  rule table already had (insufficient balance/credit, CodeBuddy's frequency
  limit, Token Harbor's "free allowance") and threw away.
  `internal/handlers/chat/retry_after.go` gains `isQuotaExhaustionVerdict`
  (`cls.Quota` → `providers.LooksLikeQuotaExhausted`, upstream's
  QUOTA_EXHAUSTED_PATTERNS → the Token Harbor allowance sentence, whose 429 body
  is prose so no rule table sees it → `errclass` `KindQuotaExhausted`, which reads
  a bare 402 as a spent balance) and `parkQuotaExhausted`, which writes the park
  only when that verdict holds. Both account-lock sites use it —
  `fallback.go` (single-provider loop) and `combo.go` (`comboLockRetryable`) — and
  the account-vs-model decision in front of them is unchanged.
- **Read (rotation).** Extended `db.ConnectionBlockedUntil` (via new
  `ConnectionBlockedUntilAt` with the clock injected): `quotaExhaustedUntil` is
  now one of the account-scoped parks the selector answers from, and only parks
  still in the future count — a stale date can neither park an account nor hide
  a live park behind it.
- **Read (dashboard).** Extended `db.ConnectionCooldownUntil` (via
  `ConnectionCooldownUntilAt`): a live `quotaExhaustedUntil` answers first, the
  generic cooldown otherwise, matching upstream's `buildRouterQuotaState` order.
  `handlers/dashboard/usage.go`'s cooldown readout now asks that helper instead
  of reading `rateLimitedUntil` inline, so the panel renders the quota deadline
  and renders nothing for a lapsed one.
- **Clear.** `ClearConnectionRateLimit` (the served-request path) now drops
  `quotaExhaustedUntil` too. `ReleaseConnectionBlocks` (check-and-release)
  already did.
- Untouched on purpose: which lock is chosen (a quota verdict parks the account,
  a burst rate limit on one model locks the model), `HasActiveCooldown`/status
  rendering, and `ResetConnectionHealthState`'s field list.

Tests:

- `internal/db/accounts_quota_park_test.go` — the writer stores one deadline in
  both fields plus `testStatus`/`errorCode`/`lastErrorAt`/`backoffLevel` and the
  upstream sentence; `ConnectionCooldownUntilAt` prefers a live quota park, falls
  back on a lapsed or unreadable one, and parks nothing for a lapsed value;
  `ConnectionBlockedUntilAt` includes the quota park while live and lets a live
  cooldown through when the quota date has lapsed; `ClearConnectionRateLimit`
  nulls both fields.
- `internal/handlers/chat/quota_exhausted_until_test.go` — a tokenharbor
  free-allowance 429 parks the account at the instant its body names (72h, not
  the flat hour or the calendar fallback), with `rateLimitedUntil` and
  `quotaExhaustedUntil` equal and both readers answering that instant; a plain
  throttle 429 on the same lane writes no quota park and leaves `testStatus`
  alone; a 402 (errclass quota, no matching wording) is parked as quota-exhausted;
  a served request clears the park.
- `internal/handlers/dashboard/usage_quota_park_test.go` — the cooldown readout
  reports the quota park, not the shorter generic cooldown beside it, and renders
  no row for a lapsed quota park (while still reporting a live generic one).

Verified: `go test ./internal/handlers/chat/ ./internal/handlers/dashboard/
./internal/db/` (plus `./internal/providers/ ./internal/errclass/`) and
`go vet` on the same packages, gofmt clean on every touched file.