### Antigravity: in-place transient retry, the 400 verdict, and stream_options

Three behaviours from `open-sse/executors/antigravity.js` that this port answered
differently. All three are about not punishing an account for a fault the request
did not cause.

**In-place retry (`computeRetryDelay`).** Google's agent lane answers 429/5xx, and a
family of 400s whose body says "high traffic" or "agent terminated due to error".
Upstream repeats the SAME call while the provider's own wait is short; this port
parked the account instead, which on a single-account deployment fails the client's
turn for a fault that clears in seconds. Now: a header- or body-named wait is
honoured up to 10s and a longer one is a veto (the caller rotates), a transient fault
without a named wait backs off exponentially within 10s (429) / 15s (5xx), and the
call is repeated at most twice — every attempt is a request to a lane that bans
accounts over repeated request patterns. Only a retryable status has its body read,
so a normal answer is still streamed untouched.

**A 400 is fatal.** Google rejects a malformed request before looking at whose
account it is, so every account answers the same 400 and walking the pool parks all
of them for a payload that can never succeed. The provider-scoped status rule marks
it fatal (no lock, no rotation) and `error.details[].fieldViolations` now reach the
error text — Google's top level says only "Bad request", and the field is the part an
operator can act on. Status rules gained a provider scope to make that possible: a
lane whose 400 means something specific must not teach the rule to every provider.

**`stream_options` on a non-streaming call.** An OpenAI client may send it while
asking for no stream, and generateContent rejects that combination before processing
the request; the field is now stripped for that case and left alone when streaming.

Verified: `go test ./internal/proxy/... ./internal/proxy/executor/...
./internal/handlers/... ./internal/providers/ ./internal/translator/ ./internal/db/
./internal/schedule/ ./internal/app/` — all packages pass, including the new retry
and normalisation tests.