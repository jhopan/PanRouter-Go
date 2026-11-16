### fix(chat): a spent free window skips the account instead of retrying it

Cloudflare Workers AI meters its free allocation per calendar day. When that
allocation is gone its 429 states the daily cap — and nothing in this router
matched that wording, so the refusal fell through to the generic 429 backoff:
the ladder climbed 2s→5min, the account stayed in rotation, and every request in
between was sent to an account that could not answer until the next UTC day.

Two things were missing, both from upstream:

- **The free-window wordings.** Upstream builds its quota rules from
  `QUOTA_EXHAUSTED_SIGNALS` (19 wordings: `daily limit`, `daily quota`,
  `weekly limit`, `weekly quota`, `monthly quota`, `quota will reset`,
  `quota exhausted`, `free limit reached`, `daily free limit reached`,
  `inference_cap_error`, `usage limit has been reached`, `individual quota`,
  `upgrade your subscription to increase`, `exceeded your current quota`, …).
  The rule table carried three of them. All of them are now quota-class rules,
  and the shared `quotaExhaustedPatterns` list agrees with the table, so the
  dashboard's stored-error classification reads the same vocabulary.
- **Cloudflare's window.** `cooldownSecFor` only reaches the provider's calendar
  when the connection's provider declares one, and `cloudflare-ai` declared
  none — so even a quota verdict parked it for the flat hour and handed the
  account back mid-day. It now declares the daily window (next 00:00 UTC), which
  is what upstream's quota-window resolver gives every undeclared provider
  (`default: { scope: DAILY, resetUtcHour: 0 }`).

The result is the skip the question was about: limit reached → quota verdict →
parked until the day rolls over → the rotation moves to the next connection
instead of re-dialling the empty one.

Ordering is deliberate: the new rows sit **after** the explicit code rows
(`insufficient_quota`, `gousagelimiterror`, …), because an exact provider code is
more specific than prose — free-pi's taxonomy keeps its own fixed cooldown, which
`TestClassifyError_FreePiQuotaTaxonomy` still pins.

One upstream divergence kept on purpose: local treats `resource_exhausted` as a
burst (it is in `rateLimitPatterns`, with `TestClassifyError_ResourceExhausted429`
asserting the backoff), while upstream lists it among the quota signals. Flipping
it would change Google/Antigravity behaviour, so it stays as it is and is recorded
here rather than silently re-pinned.

Verified: `go test ./internal/providers/ ./internal/handlers/chat/` green, with
new tests for the wordings (all quota-class, backoff level unchanged), for a
Cloudflare daily-limit 429 parking to the next 00:00 UTC (bounded by a day, unlock
on the hour), for the `cf` alias resolving to the same window, and for a genuine
Cloudflare burst staying a short burst.