### Fixed — a nous-thinking model keeps the dialect its provider reads

`internal/providers/capabilities.go` declares `ThinkingFormat: "nous"` for six
Nous models (`meituan/longcat-2.0:free`, `meituan/longcat-2.5-preview:free`,
`stepfun/step-5-preview:free`, `upstage/solar-mini4:free`,
`stepfun/step-3.7-flash:free`, …), but the unified pass had no writer for that
dialect and its `hasWriter` gate — a hand-kept switch beside the writer map —
did not list it either. `resolveFormat` answered `nous`, `hasWriter` said no, and
`Apply` returned the body untouched: the client's flat `reasoning_effort` went
out to an API whose first-party client sends a nested `reasoning` object and
nothing else.

Ported `case "nous"` from `applyFormat`
(`open-sse/translator/concerns/thinkingUnified.js`): a nested
`{enabled: true, effort: <level>}`, `auto` becoming the provider's own middle
tier (`medium`), and the field omitted entirely on a disable the model accepts —
the same shape the first-party client sends.

`hasWriter` now reads the writer table instead of repeating the list. The
duplicate was the bug's other half: a format present in `formatWriters` and
missing from the switch resolves and then silently does nothing, and the two have
to be edited together every time. One source of truth removes the class.

Tests: `TestApplyNous` (level → `{enabled, effort:"high"}` with no flat field;
`auto` → `effort:"medium"`; disable → no field at all) and
`TestNousModelUsesTheNestedReasoningDialect`, which runs the real router against a
fake upstream and asserts the body that actually left the process — provider
prefix stripped, `reasoning_effort` absent, nested `reasoning` present.

Verified: `go test -count=1 ./internal/thinking/` and
`go test -tags=integration -count=1 -run TestNousModelUsesTheNestedReasoningDialect ./internal/integration/`
green.