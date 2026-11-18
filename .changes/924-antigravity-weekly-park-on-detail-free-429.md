### fix(antigravity): a detail-free 429 no longer parks a healthy account for the week

`Resource has been exhausted (e.g. check quota).` with `status: RESOURCE_EXHAUSTED`
and no reset instant matched the generic quota wording, so `cooldownSecFor` took
the Antigravity branch and parked the account until the next weekly rollover —
measured at **111 hours** on 2026-11-18 while the account's own reading said
`gemini_weekly: 99% remaining, 1/100 used`.

That body is not a spent budget. The lane answers it for a request shape it
dislikes exactly as it does for an exhausted bucket, and the two are
indistinguishable from the text alone. The account's own reading is what
separates them, and the tracker already holds it in `agQuotaCache`.

`cooldownSecFor` now parks for the weekly window only when

1. the body names a reset instant (`extractResetDuration`) — the provider
   describing its own window, newer than any local reading, or
2. no live reading exists, or the reading says the bucket is spent.

With quota still on the bucket, the ordinary classifier cooldown stands, so the
account rotates and the next one serves the turn instead of the whole week being
lost.

**Not** changed: `503 MODEL_CAPACITY_EXHAUSTED` (`error_number: 2010`) was already
classified as a transient 2s throttle, checked live and left alone.

### Evidence

REA + MITM rerun, 2026-11-18, both accounts from the server, gateway built from
this tree:

- Shipped client (`language_server.exe` Hub 2.32.1) calls **only**
  `v1internal:streamGenerateContent?alt=sse` — 16 calls, zero
  `v1internal:generateContent`.
- `fetchAvailableModels` for both accounts: 17 models, and the 3.8 lane's wire id
  is `gemini-3.8-flash-tiered`. `gemini-3.8-flash-high` is **not** in the roster
  yet the backend serves it on one account, so the id is real and the refusals are
  per account/model.
- Live sweep, `stream: false` and `stream: true`, both accounts: 11 of 12 requests
  `200`. The one failure was adelia → `gemini-3.8-flash-high` →
  `503 MODEL_CAPACITY_EXHAUSTED`; the same model answered `200` on adisentosa.
- MITM wire diff vs the shipped client: identical identity block,
  `systemInstruction` camelCase, `thinkingLevel: high` for the Gemini families and
  `thinkingBudget: 1024` for the Claude families, wrapper
  `{project, model, userAgent, requestType: "agent", requestId}`. Nothing to
  port.

Pinned by `TestCooldownSecFor_AntigravityHealthyBucketIsNotParkedForTheWeek` and
`TestCooldownSecFor_AntigravityNamedResetStillParks`; the two existing tests
(spent bucket parks, no reading uses the calendar) still pass.

`go test ./internal/handlers/chat` → ok.

### site: a playground for the three Antigravity lanes

`site/antigravity.html` + `site/serve.py` — a page that sends one prompt to
`gemini-3.8-flash-high`, `claude-sonnet-4-6` and `claude-opus-4-6-thinking`, stream
or not, and shows status, latency and the answer per model. `/v1/chat/completions`
carries no CORS headers, so the 40-line stdlib server proxies `/v1/*` to keep the
page same-origin rather than widening the gateway's CORS policy.

```bash
python site/serve.py    # http://127.0.0.1:8088/antigravity.html
```