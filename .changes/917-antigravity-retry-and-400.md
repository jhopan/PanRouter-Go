### Antigravity: in-place transient retry, 400 verdict, stream_options

Three behaviours from `open-sse/executors/antigravity.js` that were not held here,
all of them about not parking a healthy account over a fault that is not the
account's.

**In-place retry (`computeRetryDelay`).** Google's agent lane answers 429/5xx and a
family of 400s whose body says "high traffic" or "agent terminated due to error".
Upstream repeats the SAME call while the wait is short, instead of locking the
account and rotating; on a single-account deployment the alternative is failing the
client's turn for a fault that clears in seconds. Ported with the reference's
budgets: a provider-named wait (`Retry-After`, `X-Ratelimit-Reset[-After]`, or
"resets in 1h2m3s" in the body) is honoured only up to 10s and is a veto above it,
a transient fault with no named wait backs off exponentially (capped at 10s for a
429, 15s for a 5xx), and at most two repeats. Only a 429/5xx body is read at all, so
a normal answer is still streamed untouched.

**A 400 is the request, not the account.** Google rejects a malformed body before
looking at whose account it is, so the same 400 comes back from every account —
walking the pool locks each one in turn for a payload that can never succeed. It is
now fatal for this lane only (`{Provider: "antigravity", Status: 400, Fatal: true,
RequestRejected: true}`), which meant teaching the status branch of the rule table
the same provider scope the text branch already had, and `extractErrorText` now
carries `error.details[].fieldViolations` — Google answers a bare "Bad request" and
puts the real cause ("items.items: missing field.") in there.

**stream_options on a non-streaming call.** An OpenAI client may send it even when
it asked for no stream, and `generateContent` rejects the combination before
processing the request. Stripped for this lane when the call is not streaming; a
streaming request keeps the client's value.

Verified: `go test ./internal/proxy/... ./internal/handlers/... ./internal/providers/
./internal/translator/ ./internal/db/ ./internal/schedule/ ./internal/app/`, including
the retry-delay table (header, body, backoff caps, veto), a fake upstream that fails
once and then succeeds in place, a rejected 400 that is not retried, and the two
`stream_options` cases.