### fix(tokenharbor): park the free-allowance 429 at the instant it names, not a flat hour

The Token Harbor free tier answers a spent rolling allowance with 429 and the
exact boundary in the body:

```
You've used this period's free allowance. Your next rolling 7-day period starts on 2 Oct 2026 at 06:22 UTC.
```

The lane only had the classifier's `{ text: "free allowance", cooldownMs:
QUOTA_EXHAUSTED_COOLDOWN_MS }` row, so every refused account was parked for a
fixed hour: handed back days before the window it named, refused again, and
parked again. The body already states the reset.

Mirrored from PanRouter (behaviour reference):

- `src/sse/services/auth.js:20-60` — `tokenharborResetMs(status, errorText,
  provider)`: tokenharbor only (alias-resolved), 429 only, the body must carry
  `free allowance`, the date sentence must match `TH_RESET_RE`, and an instant
  already past returns null.
- `src/sse/services/auth.js:408-441` — the precise-reset branch decides before
  the rule table (`checkFallbackError`), and `cooldownMs = min(reset - now,
  quotaWindowFor(provider).maxCooldownMs)`.
- `open-sse/config/quotaWindows.js:83` — `tokenharbor: { scope:
  QUOTA_SCOPES.WEEKLY, usageKey: null }`, the weekly calendar being the
  documented fallback "when the message shape changes".
- `open-sse/services/quotaWindow.js` + `errorConfig.js:69-72` — a quota refusal
  with no timestamp resolves through `calendarResetMs(scope)`; for WEEKLY that
  is the next Monday 00:00 UTC (`weeklyResetWeekday: 1`).

What changed, all in `internal/handlers/chat/`:

- `retry_after.go` gains `tokenharborResetAt` (the parse), `isTokenharborAllowance`
  (lane + status + wording gate), `tokenharborParkDuration` (the park: parsed
  instant, else the weekly rollover) and `nextWeeklyRollover` (the calendar).
  The parse hands back an **instant**: a zone-less layout makes `time.Parse`
  yield UTC, so the host's zone cannot move it.
- `retryableCooldownSec` now delegates to `retryableCooldownSecAt`, which takes
  the clock. A Token Harbor free-allowance 429 overrides both the classifier's
  base and any Retry-After reading — the same precedence upstream gives the
  precise reset — and is then bounded by the provider's ceiling.
- The existing `monthlyCooldownProviders` table became `quotaWindowScopes`
  (provider → `quotaScope`), so one table answers both questions: the ceiling
  (`maxMonthlyResetCooldown` 40 days, new `maxWeeklyResetCooldown` 7 days) and
  which lanes have a calendar fallback. `cbai`/`cbcn` keep their MONTHLY
  ceiling; `th`/`thh` resolve to tokenharbor through `providers.ResolveAlias`.
- Untouched: the classifier row (`internal/providers/errorclassify.go:99`) still
  matches first and still returns `QuotaExhaustedCooldownMs`, and
  `stopsRotationOnRateLimit` (fallback.go) still stops only CodeBuddy Intl —
  tokenharbor keeps rotating, it just parks for the right window.

Tests, `internal/handlers/chat/tokenharbor_quota_test.go` (table-driven, injected
clock, no network):

- the exact body (plain, JSON-wrapped, `th`/`thh`, lowercase month, unpadded
  hour, the bare "starts on" clause) yields the instant `2026-10-02T06:22:00Z`
  with `now` supplied in a `UTC+7` zone;
- a different wording (no sentence, full month name, no `UTC`, another lane's
  `2026-10-02 06:22:00` shape, marker absent, 31 Feb) yields no reset;
- a non-tokenharbor provider (openai, codebuddy-intl, cbai, deepseek-web,
  gemini, "") or a non-429 status yields nothing;
- the parsed instant beats the classifier's fixed cooldown (3 days, not 1 hour)
  and an instant beyond the window is capped at the 7-day ceiling; a body with
  no parseable reset and an already-past instant both fall back to the next
  weekly rollover (pinned to 495480s from Tue 2026-09-29 06:22Z);
- `maxCooldownFor` answers WEEKLY for tokenharbor/th/thh and MONTHLY for every
  CodeBuddy spelling;
- other lanes are unaffected: their 429 keeps its own base, `cbai`/`codebuddy-intl`
  still stop the rotation, tokenharbor still does not.

Verified: `go test ./internal/handlers/chat/ ./internal/providers/ ./internal/db/`
and `go vet` on the same three packages.