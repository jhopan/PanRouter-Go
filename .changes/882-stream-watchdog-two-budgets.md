# The stream watchdog runs two budgets, one per phase

`AGENTS.md` §1 item 4. A single budget covered both phases of an upstream
stream, so a slow prompt prefill on a large context was charged against the
silence budget and healthy turns were aborted — MiniMax Code, whose 512k-context
turns prefill for minutes, is where it surfaced.

Port of upstream `pipeWithDisconnect` (`open-sse/utils/streamHandler.js:195`,
`armStall` at 224-241) and `chatCore/streamingHandler.js:114-121`.

## The two phases

- **First chunk (prefill).** Before any byte of the body arrives the provider is
  prefilling the prompt. On a 120k-token context 27-30 seconds is normal and
  minutes happen; slow here is not a symptom. Default 200s
  (`STREAM_FIRST_CHUNK_TIMEOUT_MS`, upstream `config/runtimeConfig.js:56`).
- **Inter-chunk (silence).** Once chunks are flowing, silence means the stream is
  gone. Default 360s (`STREAM_STALL_TIMEOUT_MS`,
  `config/runtimeConfig.js:53`); this budget keeps its old meaning and its old
  env spellings.

The watchdog arms the budget of the phase it is in and re-arms to the
inter-chunk one the moment a byte arrives. Both names also accept the legacy
`PANROUTER_`-prefixed spelling (`envx.Names`).

## Per-provider overrides

`ProviderConfig` carries `stallTimeoutMs` and `firstChunkTimeoutMs`
(`internal/providers/providers.go:94-101`), and
`proxy.ResolveStallBudgets` applies them field by field: an entry that declares
only the inter-chunk value keeps the default for prefill instead of being
silently given the same number for both. A declared value wins over the
environment, matching upstream's `PROVIDERS[provider]?.stallTimeoutMs ||
STREAM_STALL_TIMEOUT_MS`.

Both MiniMax Code registries (`minimax-code`, `minimax-code-global`) declare
upstream's pair — 15m between chunks, 10m before the first
(`internal/providers/panrouter_extras.go:13-18`, upstream
`providers/registry/minimax-code.js:26-27` and
`minimax-code-global.js:23-24`) — because their context window, not the 6-minute
default, sizes a turn.

## The abort says which phase and how long

`*StreamStallError` replaces the bare `"stream stall timeout"` string and carries
the phase (`no first chunk` / `stalled after first chunk`), the budget, the wait,
the silence since the last chunk, and the chunk/byte counts:

```
stream no first chunk (waited 30ms, budget 30ms)
stream stalled after first chunk (no data for 41ms, budget 40ms)
```

`ClassifyStreamAbort` now forwards that message instead of the generic prose, so
the in-band error frame names the phase too. The frame carries
`code: "STREAM_STALL"` rather than the status-derived `gateway_timeout`, the tag
upstream puts on the error (`streamHandler.js:239`); `errStallCause` recognises
both message shapes so the tag survives the message change. The error still
unwraps to `ErrStreamStall`, so every existing classifier is unchanged.

## Files

- `internal/proxy/stall_config.go` — `StallBudgets`, the defaults, the env
  resolvers and `ResolveStallBudgets`.
- `internal/proxy/stall.go` — `StreamStallError`, and the reader that arms the
  budget for its phase (`armLocked`).
- `internal/proxy/sse_error.go` — the phase-bearing abort message and the
  `STREAM_STALL` tag.
- `internal/proxy/stall_config_test.go` — the tests below.

## Verification

```
go test ./internal/proxy/ -run Stall -count=1
go test ./internal/providers/ -run MiniMax -count=1
go vet ./internal/proxy/ ./internal/providers/
```

- `TestStallReaderUsesTheBudgetForItsPhase` — table over both phases with
  millisecond budgets: a 30ms first-chunk budget against an hour-long
  inter-chunk budget yields "no first chunk" (an inter-chunk failure is
  impossible there); the mirror image, after one byte, yields "stalled after
  first chunk" with the chunk count.
- `TestStallTimeoutEnvEndToEnd` — the env budget wins over the compiled default
  and the frame carries `STREAM_STALL`, the phase and `budget 20ms`.
- `TestStreamStallPayloadNamesPhase` — both phases reach the client as
  `code: STREAM_STALL` with the phase and budget in the message, through the
  same `ClassifyStreamAbort` + `BuildStreamErrorBytes` pair the abort path uses.
- `TestResolveStallBudgets` — nil config, inter-chunk-only, both declared, and
  the MiniMax pair resolving to 15m/10m.
- `TestResolveDefaultFirstChunkTimeout` — the first-chunk env spelling, its
  `PANROUTER_` alias, and that the two defaults are not the same value.