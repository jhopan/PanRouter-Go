### Added — correlated per-request console lines: which account served the turn, and what it cost

An operator watching the console panel could see that a request happened and
nothing about who served it. PanRouter prints four correlated lines per attempt —
a coloured dot identifying the session, a request summary naming the account, the
token-saver lines that shaped the body, and a completion line with the token
counts and cache split. Ported verbatim in shape, so the two gateways read the
same way:

```
[21:11:36] 🟡 ▶ POST th/deepseek-v4.1-flash:free → tokenharbor/deepseek-v4.1-flash:free · FMT: openai→openai · STREAM · 245 MSG · 13 TOOL · ACC:Key 84 · REQ:9c6d61
[RTK] saved 4596B / 170413B (2.7%) via [dedup-log,git-status,build-output,ls] hits=12
[21:11:36] 🟡 ⚙ CAVEMAN:full
[21:11:44] 🟡 📊 DONE 7622ms · TTFT 6903ms · IN 133730 (CACHE ↻132352) · OUT 118
```

Upstream sources, per format:

| Line | Upstream |
|---|---|
| `[HH:MM:SS] <tag> <symbol> <message>`, the eight-dot tag set, `tagForSession` (`h*31 + codePoint`, int32-wrapping, `abs(h) % 8`), the rotating `nextTag`, six-hex `requestId`, `fmtThink` | `src/sse/utils/logger.js:33-50,60-62,69-73,88-97` |
| `POST <clientModel> → <provider>/<model> · FMT: <src>→<tgt> · STREAM\|JSON · <n> MSG · [<n> TOOL] · [THINK:<x>] · ACC:<name\|id8\|-> · REQ:<id6>` | `open-sse/handlers/chatCore.js:258-279` |
| `DONE <total>ms[ · TTFT <ttft>ms] · IN <in>[ (CACHE ↻<read>[ +<create>])] · OUT <out>` | `open-sse/handlers/chatCore/requestDetail.js:86-101` |
| `[RTK] saved <bytes>B / <total>B (<pct>%) via [<filters>] hits=<n>` | `open-sse/rtk/index.js:151-157` |
| `⚙ CAVEMAN:<mode>` | `open-sse/handlers/chatCore.js:318-322` (printed at `:348`) |

`internal/log/line.go` carries the primitives (`NextTag`, `TagForSession`,
`RequestID`, `Line`, `Raw`, `LineEnabled`, `FmtThink`) and the formatters
(`FormatRequestLine`, `FormatDoneLine`, `FormatRTKLine`, `FormatCavemanLine`,
`FormatAccount`). `Line` writes to the process log *and* into the console buffer
`internal/handlers/consolelog.go` streams to the dashboard, so the panel an
operator watches shows exactly what the terminal does; `Raw` is for the RTK line,
which upstream prints with a bare `console.log` and which therefore carries no tag
or timestamp. No ANSI colour is emitted — the dot is the emoji, and the panel
strips escapes anyway.

Every line is gated on the INFO level (`log.LineEnabled()`), and the request line
— which walks the final body for its message and tool counts — is built only when
the level will print it. `bodyShapeCounts` reads those two lengths with a
streaming `jsontext` walk rather than materialising the conversation a second
time on the box that serves it.

The RTK line needed two figures this port did not measure: the tool-output bytes
weighed and the filter behind each saving. `tokensaver.CompressMessagesWithConfigStats`
is `CompressMessagesWithConfig` plus that accounting — same body, same changed
flag, with `BytesBefore`/`BytesAfter`/`Hits`/`Filters` collected at the point each
tool result is compressed (filter ids in first-seen order, as upstream's
`formatRtkLog` prints them). The plain function is now a thin wrapper, so no
caller changed behaviour.

The request line is emitted once per attempt in `tryForwardWithConnection`, where
the upstream body is final; the DONE line uses the usage that path has already
parsed and the latency/TTFT it has already measured. Nothing written to the
database, the usage ledger or the structured logs changed — these are additional
console lines, and `ACC` falls back through connection name → first eight
characters of its id → `-` exactly as upstream does.

Tests: `internal/log/line_test.go` (tag values pinned against the reference
implementation's own output, including two non-BMP seeds that only match if the
hash walks UTF-16 code units; six-hex ids including a broken entropy source; the
full `FmtThink` table; request, DONE and RTK lines field for field against the
operator's reference output; `ACC` fallback; and that a `Line` and a `Raw` are
readable from `ConsoleEntries()` at `LevelInfo` and suppressed at `WARN`),
`internal/tokensaver/compress_stats_test.go` (bytes weighed on both sides,
first-seen filter order, one filter with two hits, and that the stats variant
returns byte-identical bodies to the plain pass), and
`internal/handlers/chat/console_lines_test.go` (body shape counts across Chat,
Claude, Responses and Gemini bodies and a truncated one; client/upstream wire
formats; and the three-line order an operator reads).

Verified: `go test -count=1 ./internal/log/ ./internal/tokensaver/
./internal/handlers/chat/` green, `go vet` clean on all three.