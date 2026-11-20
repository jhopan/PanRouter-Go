---
type: fix
title: The refusal fallback was dead code — a shadowed variable hid it
---
### Problem

Hermes kept answering `429 RESOURCE_EXHAUSTED` on every account after v1.0.11,
with the same detail-free body. The refusal fallback added in that release never
appeared in the log, which was the clue: it was not failing, it was not running.

`ForwardGemini` built the Gemini body with `geminiBody, err := ...` inside the
translation branch. That `:=` declared a **new local** shadowing the
function-scoped `geminiBody`, so the outer variable stayed `nil` and the
fallback called `MoveSystemInstructionToContents(nil)`, which returns `false`
without logging. The fallback shipped as unreachable code.

### What the capture showed

The failing request was captured through a temporary dump and replayed straight
at the Antigravity backend, with the gateway out of the picture:

| payload | verdict |
|---|---|
| captured envelope, 148 KB | 429 |
| same, tools removed | 429 |
| systemInstruction truncated to 51 KB | 200 |
| **systemInstruction moved into `contents[0]`** | **200** |

So the refusal follows the system instruction itself, and relocating it clears
the refusal — exactly what the dead fallback does.

### Solution

`internal/proxy/gemini.go` assigns the translated body to the function-scoped
variable instead of shadowing it. The fallback now runs, logs its outcome, and
turns the refusal into a served turn.

### Verification

```text
gateway log, 95 KB persona as the system message:
  retrying the same call ... attempt=1
  retrying the same call ... attempt=2
  systemInstruction moved into contents[0] cleared the refusal
  -> 200

go test ./...   73 packages ok
go vet ./...    ok
```

`TestForwardGemini_MovesSystemInstructionToClearABareRefusal` pins both halves:
the retry is sent at all, it carries no caller text in `systemInstruction`, and
the caller's instructions arrive in the first content turn.
