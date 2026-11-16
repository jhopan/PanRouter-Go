### Proxy: a provider's 403 is no longer replayed as a proxy refusal

`isProxyFailure` treated any 403 whose body sniffed as `text/plain` as "the proxy
refused this request", and `FallbackTransport` replayed the identical request
through its direct leg — after which `DoRequest`'s own refusal path replayed it
once more. Measured on one call to a provider answering
`403 {"code":11140,"msg":"request illegal"}` with no `Content-Type`:
**three upstream requests**, same headers (per-turn ids included) and same gzipped
body.

For CodeBuddy Intl that is the automation signature the account is banned over: the
provider sees one logical turn arrive three times, from two egress paths, and the
extra copies are also invisible to the router (one fallback attempt is logged).
`FallbackTransportDoesNotReplayAProviderRefusal` now pins one request, and the
integration suite pins the same for the Intl lane end to end.

The sniff is gated rather than dropped:

- `X-Proxy-Error` is the proxy speaking for itself and still triggers the replay,
  whatever the body looks like.
- The text/plain sniff now requires a proxy that was actually **assigned**. An
  env-inherited proxy does not count — this repo already reads it that way when it
  decides whether a refusal may be answered directly (`proxiesViaAssignment`), and
  a process that merely has `HTTP_PROXY` set in its environment was replaying
  application refusals.
- A transport that is not an `*http.Transport` (pool-backed, tls-client) keeps the
  sniff: those are the transports whose refusals this wrapper exists to recover
  from.

Verified: `go test ./internal/proxy/... ./internal/handlers/... ./internal/translator/
./internal/providers/ ./internal/db/ ./internal/schedule/ ./internal/app/`
(18 packages, 0 failures) plus the integration suite for CodeBuddy Intl.