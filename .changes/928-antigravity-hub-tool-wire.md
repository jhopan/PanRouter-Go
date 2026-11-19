### fix(antigravity): send the caller's actual tool wire, not fake IDE tools

A live Hub/CLI capture exposed the remaining agent-wire mismatch behind the
persistent detail-free `429 RESOURCE_EXHAUSTED` on long requests:

| field | Hub / CLI capture | panrouter-go before this fix |
|---|---|---|
| caller tools | Hub 16 / CLI 12 real declarations | 13 caller tools + 21 fake decoys |
| valid tool name | kept unchanged | rewritten to `<name>_ide` |
| history call/result name | same name as declaration | rewritten to `_ide` |
| `toolConfig` | `VALIDATED` | `VALIDATED` |

The test endpoint's two-message request had no tools and answered 200; the
failing agent turn carried 189 messages and 13 tools. Adding twenty-one invented
tools and changing every name made that request unlike either official client.

`CloakAntigravityRequest` now keeps the caller's actual declarations, flattening
them into one group exactly like the Hub. It still sanitizes names Gemini would
reject, cleans schemas, deduplicates declarations and changes history
functionCall/functionResponse names to the same sanitized spelling. It writes
`toolConfig.functionCallingConfig.mode = "VALIDATED"`. No decoys. No `_ide`.

The global legacy response decoder still understands old `_ide`/`proxy_` names for
other provider paths; the Hub request path no longer manufactures them.

The account identity config read was also corrected from POST to the captured
GET request:

```http
GET https://aicode.googleapis.com/v1alpha:fetchConfig
Authorization: Bearer …
User-Agent: antigravity/hub/2.32.2 …
```

The previous POST returned 404, so every account silently used the generic
identity branch. Hub `loadCodeAssist`/`onboardUser` metadata now also matches the
capture exactly:

```json
{"metadata":{"ideType":"ANTIGRAVITY"}}
```

**Verification**

- Regression test was red against the old tool wire: it observed
  `execute_code_ide` plus 21 decoys instead of the two caller declarations.
- `TestWrapForAntigravityAgentToolsKeepTheCallerContract` now pins real caller
  names, no decoys and `VALIDATED`.
- `TestAntigravityProbe_HeadersAndMetadata` and
  `TestRefreshAntigravityExperimentsReadsTheConfig` pin Hub metadata and GET.
- `go test ./internal/providers ./internal/translator ./internal/proxy
  ./internal/handlers/chat -run 'Antigravity|RefreshAntigravityExperiments'` → ok.
- Local `claude-sonnet-4-6` streaming smoke request with 13 real tools →
  `http 200`, six SSE frames, answer `ok`.

This fixes wire fidelity. It does not remove existing one-hour model locks;
release then use `POST /api/usage/{connectionId}/check-release` for connections
already locked by the old request shape.