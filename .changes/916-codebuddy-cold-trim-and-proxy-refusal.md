### CodeBuddy Intl: cold-account trim, and a provider 403 is never replayed

Two more pieces of the reference, both about the same thing — CodeBuddy Intl is the
lane that bans accounts over payload/request patterns.

**Cold-account trim** (`open-sse/services/messageTrimmer.js` + its caller in
`src/sse/handlers/chat.js`). CodeBuddy's prompt cache is bound to the ACCOUNT, so a
rotation mid-request lands on an account with no cache for the conversation and the
FULL history goes up in one request — 412k tokens in the measured case, which is the
payload shape the gateway reads as automation. When an attempt lands on a different
account than the one that just ran, the oldest messages are now dropped until the
estimate fits `TRIM_BUDGET_TOKENS` (200k, characters/4), the leading
system/developer block is always kept, and a dropped turn takes its tool results
with it so the history never starts mid-tool-call. Once per request, codebuddy-intl
only — trimming again would keep eating history.

**A provider 403 is no longer replayed as a proxy refusal.** `isProxyFailure`
treated any 403 whose body sniffed as `text/plain` as "the proxy refused this", so
`FallbackTransport` replayed the identical request and `DoRequest`'s own refusal
path replayed it once more: **three upstream requests for one turn**, identical
headers (per-turn ids included) and identical body, from two egresses, while the
router logged a single attempt. That is the repeat pattern the ban is issued over.
The rule now matches the reference's `proxyAwareFetch`, which answers the question in
exactly two places and nowhere else:

- a proxied attempt that THREW told us nothing about the upstream, so it is retried
  on the direct egress (its `catch` branch);
- a response is never read as a proxy refusal, except when the proxy names itself
  with `X-Proxy-Error`. (The reference's one response-driven replay is OpenCode's
  403 `FreeTierError`; here that is a provider verdict and rotates the pool, which
  is where it belongs.)

An assigned pool that refuses to tunnel still never degrades to the host's IP, and
the test fixture for it now names itself the way a proxy does.

Verified: `go test ./internal/handlers/... ./internal/proxy/... ./internal/translator/
./internal/providers/ ./internal/db/ ./internal/schedule/ ./internal/app/`
(12 packages in this run, 0 failures) plus the CodeBuddy integration suite.