### feat(provider): the `devin-cli` lane speaks ACP over the local CLI's stdio

`devin-cli` (alias `dv`, alias `devin`) was a registry entry with no transport:
its base URL read `devin://acp/stdio`, it declared a bearer header it could never
send, and its models were absent from the catalogue. Every `swe-*` request
therefore died before any upstream was dialled — either as
`HTTP 502: upstream error: no active connections for provider: devin-cli` (no
`NoAuth` field, so the fallback path injected no synthetic connection) or, once
that was passed, as an OpenAI POST at a `devin://` URL. It now works end to end.

**The protocol.** This lane's upstream is not HTTP. A request spawns
`devin acp` (the CLI in Agent Client Protocol mode) and speaks JSON-RPC 2.0 over
its stdin/stdout, exactly as `open-sse/executors/devin-cli.js` does:

1. `initialize` — `protocolVersion: "0.3"`, `clientInfo: {name: "9router", version: "1.0"}`.
2. `session/new` — `cwd` (the client's workspace), `model`, and `mcpServers: []`
   (devin 3000.2.x rejects a session/new without the field: `-32602 missing field
   mcpServers`).
3. `session/prompt` — `prompt: [{type: "text", text: <whole conversation>}]`
   (not `content`: `-32602 missing field prompt`).
4. `session/update` notifications stream back: `agent_message_chunk` becomes
   OpenAI delta text, `agent_thought_chunk` is dropped, `tool_call` /
   `tool_call_update` feed the client-tool bridge, and
   `_cognition.ai/agent_stopped` (or the `session/prompt` result) ends the turn.
   `session/request_permission` is auto-approved on its first allow option —
   unanswered, the CLI blocks on the first shell/exec tool call.

**What is ported.**

- `internal/proxy/executor/devin_cli.go` — the executor, registered in
  `init.go` in place of the shared passthrough.
- `devin_cli_acp.go` — the JSON-RPC state machine and its alert shapes.
- `devin_cli_request.go` — the multi-turn → single-prompt builder, the `<cwd>`
  workspace scan (an absolute existing directory, else the temp dir), and the
  `clientTools` MCP declaration.
- `devin_cli_process.go` — binary resolution and the child's lifecycle.
- `devin_cli_emitter.go` — OpenAI chunks when the caller streams, one
  `chat.completion` when it does not; Claude and Responses clients are
  translated out of the same chunks.
- `devin_clienttools_mcp.go` — the stdio MCP server behind the client-tool
  bridge. Upstream spawns a node script for this; this port has no node to
  assume, so the same server is a mode of this binary
  (`panrouter-go devin-clienttools-mcp`), with the same environment contract.
- Catalogue: all 35 registry models (including `swe-1.6-fast`, `swe-1.6`,
  `swe-1.5*`, `claude-opus-4.7-*` and the Gemini/GPT/Kimi/GLM rows) are in
  `registry_models.go` under `devin-cli` and `dv`, with the registry's own
  context windows pinned in `providerTokenLimits`.
- `KnownProviders["devin-cli"]` is now `NoAuth: true` with no `AuthHeader`/
  `AuthScheme`: the child authenticates from the CLI's own stored credentials,
  and the fallback path's `cfg.NoAuth` branch is what dispatches the lane with
  no connection row.

**Client tools.** `body.tools` (OpenAI schema) is exposed to the CLI as a
`clientTools` MCP server written into a throwaway `XDG_CONFIG_HOME/devin/config.json`
(`session/new`'s `mcpServers` alone does not make devin connect them). The CLI
only discovers MCP tools carrying the `mcp_` prefix, so names are prefixed on the
way in and stripped on the way out. When the agent calls one, the turn ends with
`finish_reason: "tool_calls"` and the client runs it; the next request replays
the history, and the tool results the client already produced are seeded back
into the MCP server (`DEVIN_MCP_RESULTS`) so the agent's retry gets real data
instead of the `(awaiting client tool_result)` placeholder.

**Lifecycle.** One child per request. It is killed on context cancellation, its
stdin is closed when the turn ends, and it is given 2s before being killed — the
same grace upstream uses — then always reaped, so no zombie survives a request.
stderr is drained into an 8 KiB bounded tail that is quoted in the error when the
child dies before answering; a protocol frame is bounded at 4 MiB rather than
read unbounded. There is no gateway-side deadline beyond the caller's own
context, matching upstream.

**Errors.** A missing binary answers
`502 {"error":{"message":"Devin CLI not found: <path>. Install via
https://cli.devin.ai or set CLI_DEVIN_BIN env var.","type":"devin_cli_error","code":"spawn_failed"}}`
— the lane's own error, not a blanket gateway message. A failed
`initialize`/`session/new`/`session/prompt` answers the ACP error verbatim
(`Devin ACP error -32602: Invalid params`), a child that exits early answers with
its exit code and stderr tail, and a malformed JSON-RPC frame is a protocol error
(a line that is not JSON at all — a banner — is ignored, as upstream ignores it).
Once the stream is open, the same failures travel as an error frame.

**Prerequisite.** The `devin` CLI must be installed on the host running the
gateway and logged in:

```bash
curl -fsSL https://cli.devin.ai/install.sh | bash   # macOS: brew install --cask devin-cli
devin auth login                                     # Windows: irm https://static.devin.ai/cli/setup.ps1 | iex
```

Binary resolution mirrors upstream: `CLI_DEVIN_BIN` first, then the installer's
own locations (including `%LOCALAPPDATA%\devin\cli\bin\devin.exe` on Windows),
then `PATH`. `CLI_DEVIN_AGENT_TYPE=summarizer` switches to the tool-less agent;
`DEVIN_PERMISSION_MODE` defaults to `bypass` so tool calls do not hang, and
`DEVIN_MCP_SERVERS` adds further MCP servers in devin's own config format.

**Verification.** The lane is tested against a fake ACP peer over `io.Pipe` with
the spawner injected — no `devin` binary and no network:

- `TestForwardDevinCLI_ProtocolSequence` pins the three requests, their order and
  their params (`mcpServers` as a sequence, `cwd`, `model`, the rendered prompt).
- `TestForwardDevinCLI_StreamFrames` / `_NonStreamAggregatesOneCompletion` pin the
  OpenAI stream (role chunk, deltas in order, usage-estimate terminal chunk,
  `[DONE]`) and the single `chat.completion` body, and that thinking is not
  surfaced.
- `TestForwardDevinCLI_ClientToolsExposedAsMCP`, `_SeedsClientToolResults`,
  `_ToolCallBecomesOpenAIToolCall`, `_ToolCallNonStream` and
  `TestDevinClientToolsMCP_AnswersInitializeListAndCall` pin both directions of
  the tool bridge.
- `TestForwardDevinCLI_MissingBinary`, `_ChildExitMidSession`,
  `_MalformedFrame`, `_BannerLinesAreIgnored` and `_ContextCancelKillsChild` pin
  the failure paths; the last one asserts the child is killed and reaped through
  channels, with no sleeps.
- `TestDevinCLICatalogCarriesEveryRegistryModel`, `_CatalogNamesMatchDashboard`,
  `_ContextLengthsMatchRegistry` and `_LaneIsNoAuthStdio` pin the catalogue and
  the no-auth/stdio nature of the lane.