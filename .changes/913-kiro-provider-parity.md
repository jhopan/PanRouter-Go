### Kiro: full parity port from PanRouter

Port of the Kiro provider from the PanRouter reference, with the behaviour each
upstream test pins reproduced here as a Go test. Source of truth for the whole
batch: `open-sse/shared/kiroIdentity.js`, `open-sse/config/kiroConstants.js`,
`open-sse/services/kiroCapCache.js`, `open-sse/services/usage/kiro.js`,
`open-sse/translator/{request,response}/*kiro*`, `open-sse/executors/kiro.js`, and
the 24 `tests/**/kiro-*` suites next to them.

**Identity — the version gate.** `KiroIDE-1.4.178` replaces the stale `1.4.121`,
and the user agent now ends with `-KAS/0.66.102-hotfix.1 acp-client/kiro-ide`, as
the captured IDE does. `api/<service>#<ver>` is derived per surface
(`kiroruntime`, `codewhispererstreaming`, `codewhispererruntime`), the `m/N,E`
capability pair is present, and the `os/`+`md/` tokens describe the host the
gateway runs on instead of every host claiming `os/windows#10.0.26200`. One
source — `internal/providers/kiro_identity.go` — feeds chat, the model list and
the quota read, so the three hops cannot diverge. Per-OS release helpers are
build-tagged (`linux`, `darwin`, `windows`, other) and never claim a machine the
process is not running on.

**Wire — the surface decides the shape.** `runtime.<region>.kiro.dev` gets
`x-amz-target: KiroRuntimeService.GenerateAssistantResponse` with
`application/x-amz-json-1.0` and the `x-amzn-kiro-*` hints; `codewhisperer.*`
gets the CodeWhisperer target; `q.*` gets none. The Amazon-only markers
(`x-amz-sso-bearer`, `x-amzn-kiro-agent-mode`, `x-amzn-codewhisperer-machine-id`)
are scoped away from the runtime gateway, which does not send them.

**Monthly cap — stop spending the month.** The 402 body
`{"message":"You have reached the limit.","reason":"MONTHLY_REQUEST_COUNT"}`
carries no quota keyword, so it used to earn the generic two-hour cooldown and the
account was re-dialled every couple of minutes for the rest of the month.
`internal/handlers/chat/kiro_cap_cache.go` remembers the cap (in memory and in the
`kiro_cap` kv scope, restored at startup, dropped when its reset passes), the
fallback skips the account before the request goes out, and the cooldown resolves
to the usage API's `nextDateReset` or the first instant of the next UTC month.
Kiro is registered as a MONTHLY quota window so an announced reset is not clamped.

**Conversation — structural or nothing.** `KiroCanonicalizeConversation` is the
port of `kiroConversation.js`: strict turn alternation, `origin`/`modelId` on
every user turn, adjacent one-to-one tool pairs, duplicate-id remapping, empty
tool results given a placeholder, orphan/unmatched calls and results flattened
into text, and a final validation that refuses a conversation the gateway would
answer with `400 REQUEST_BODY_INVALID` — locally, without spending the call.

**Request intent — reasoning, agentic, native effort.** `-thinking` and
`-agentic` are consumed in that order (agentic first: the old order left
`-thinking` on the wire for `…-thinking-agentic` and the upstream answered
`INVALID_MODEL_ID`), the `model(level)` override lands in the body the resolver
reads, `thinking` becomes the `<thinking_mode>` prefix or the native
`additionalModelRequestFields` envelope for Claude 4.6+/GPT 5.6, and the agentic
protocol prompt travels with the model that asked for it.

**Session replay.** The opening user turn is frozen per (connection, conversation)
and replayed byte-identically on later turns, with the fresh time context injected
only into the current turn. That is what keeps the upstream prompt cache prefix
stable; the store is bounded (5000 sessions, 24h TTL).

**Claude wire.** `ClaudeToKiro` is the direct `claude:kiro` request translator
(system block arrays, screenshots inside tool results as user images, Anthropic
tool ids), and a `/v1/messages` client now receives Claude frames from Kiro's
EventStream instead of OpenAI chunks, with sanitized tool names restored to the
client's own spelling.

**Stream integrity.** `messageStopEvent`/`metadataEvent` stop reasons drive
`finish_reason` (`max_tokens` → `length`, `tool_use` → `tool_calls`, a refusal →
`content_filter`), merged so a later clean stop cannot hide an earlier truncation;
EventStream `error`/`exception` frames reach the client as
`kiro_upstream_eventstream_error` instead of a short "successful" answer;
`metricsEvent` token counts replace the `len(content)/4` estimate; and inline
`<thinking>` markup is stripped across fragments.

**Enterprise auth.** `external_idp` refreshes as an OAuth `refresh_token` grant
against the account's own Microsoft endpoint (validated to a Microsoft login host,
because it receives the refresh token), instead of failing against Kiro's token
URL. The endpoint rules live in one place, shared by the import handler and the
refresher.

Verified: `go test ./internal/translator/ ./internal/proxy/... ./internal/handlers/...
./internal/providers/ ./internal/app/`, plus
`go test -tags=integration -run Kiro ./internal/integration/` — the latter boots the
production router against a fake Kiro upstream and asserts the outbound
`conversationState` envelope and the client-visible frames on both wires. No live
Kiro account exists on this machine, so the upstream fingerprint itself is
verified against the captured values in the reference, not against AWS.