### Changed — CodeBuddy now sends the CLI 2.178.0 identity on both lanes

CodeBuddy accounts kept coming back as `11140 account restricted due to unusual
activity`. The request itself passed channel verification (`11128`) — what the
gateway flagged was the identity: a stale CLI build, a header set the official
client never emits, and an event-stream `Accept` where the CLI asks for JSON.
Sending a header the real client does not send is the same tell as omitting one
it does.

Ported from PanRouter `open-sse/executors/codebuddy-intl.js` (the `CB_*`
constants, `buildHeaders`, `encodeRequestBody`) and
`open-sse/shared/codebuddyUserId.js`, verified there against a live MITM
capture of CLI 2.178.0 on Windows/x64.

- **One CLI build, both lanes.** `2.178.0` lives once, in
  `providers.CodeBuddyCLIVersion`. The executor no longer carries a second
  literal (`2.133.1` for CN), and the provider registry reads the same constant.
  Stainless SDK `7.39.0` and Node `v26.7.0` moved to `providers.CodeBuddy*` as
  well; the old `6.25.0` / `v24.11.1` values came from whatever host ran the
  gateway, which the gateway can compare against the build it ships.
- **`Accept: application/json`.** The CLI asks for JSON on the chat call and
  still receives an event stream back. `proxy.ForwardOpenAI` now treats `Accept`
  as a default (like `User-Agent` and `Accept-Encoding`) instead of overwriting
  it for streaming calls, so a lane that names its own value keeps it.
- **`Content-Encoding: gzip` with a gzipped body.** `ForwardCodebuddyCN`
  compresses the transformed body once per outgoing request into a pooled
  `*gzip.Writer` + buffer (`sync.Pool`), so the 32 KiB deflate window is not
  re-allocated on every turn, and the payload is copied out before the scratch
  buffer returns to the pool. The transport derives `Content-Length` from those
  bytes, so it now matches the compressed length rather than the JSON length. A
  transport-level retry replays the already-compressed bytes — nothing is
  recompressed.
- **`X-User-Id` from the token's `sub` claim, cached per token.** Decoded
  without verifying the signature (the token is the credential the caller
  already holds), memoized by SHA-256 of the token in `providers`, bounded at
  512 entries so a long-lived gateway does not retain every rotated token. A
  token with no `sub` — an opaque `sk-…` key — omits the header, exactly like
  upstream `userIdFromCredentials` returning `null`.
- **Removed `X-App`, `X-Product-Version`, `X-Stainless-Helper-Method`** from
  both lanes. Also added the two agent headers the CLI declares and this repo
  omitted: `X-Agent-Type: main` and `X-Agent-Runtime: other`.
- **Deterministic body under gzip.** A compressed body cannot be re-serialized by
  `proxy.canonicalChatBody`, which is what sorted map members for prompt-cache
  prefix stability. `transformCodebuddyBody` now marshals with
  `json.Deterministic(true)` itself, so gzipping does not reintroduce Go's
  randomized key order.

### Verified

- `go test ./internal/proxy/executor/ -run CodeBuddy -count=1` — table-driven
  over both lanes: the exact header key set, `2.178.0` in `User-Agent` and
  `X-IDE-Version`, `Accept: application/json`, `Content-Encoding: gzip`,
  `X-User-Id` from a token with a known `sub`, Stainless `7.39.0` / Node
  `v26.7.0`, and the three removed headers asserted absent.
- The same file asserts the wire body is real gzip, decompresses byte-for-byte
  to what the uncompressed path produced, and that `Content-Length` equals the
  compressed byte count — on both lanes.
- `go test ./internal/proxy/executor/ ./internal/providers/ ./internal/proxy/
  ./internal/proxy/oauth/ -count=1` green; `go vet ./internal/proxy/executor/`
  clean.