### fix(antigravity): a served effort variant is no longer blocked by the roster

`gemini-3.8-flash-high` answered on startup and then started answering 502
"provider antigravity/gemini-3.8-flash-high is unhealthy" minutes later, while the
same model answered 200 in the account's own client.

The cause is the roster read, not quota and not 429. The discovery RPC
(`v1internal:fetchAvailableModels`) enumerates the FAMILY — a captured account
lists `gemini-3.8-flash-tiered` and never `gemini-3.8-flash-high`, `-medium` or
`-low` — while the shipped client sends those very effort ids and the backend
serves them (captured 2026-11-18: three IDE turns, three different effort ids, all
200). `IsAntigravityModelServed` read that roster as an exact allow-list, so the
first quota refresh that populated it silently removed a model the account does
serve. Before the refresh the roster is empty and the check is optimistic, which is
why the failure always appeared later.

An effort suffix (`-high`, `-medium`, `-low`, `-agent`, `-thinking`, `-extra-low`)
now resolves to its family base, and the base's `-tiered` sibling is accepted too:
`gemini-3.8-flash-high` is served when the roster carries `gemini-3.8-flash` or
`gemini-3.8-flash-tiered`. A model from a family the account does not carry still
gets no vouch, which is what the check exists for — a 2.5-only roster does not
vouch for a 3.8 model.

Pinned by `TestIsAntigravityModelServed_EffortVariantIsVouchedByItsFamily`
(captured roster, foreign family, 2.5-only roster, unknown roster).

`go test ./internal/handlers/chat -run Antigravity` → ok
`go build ./...` → ok

**This does not lift blocks already written.** Stale weekly parks from before
v1.0.4 still hold `claude-sonnet-4-6` and `gemini-3.8-flash-high` on their
connections until 2026-11-23; clear them with
`POST /api/usage/{connectionId}/check-release`.