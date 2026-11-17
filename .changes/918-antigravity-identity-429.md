### Antigravity: the shipped client's wire, captured and reproduced

The Antigravity lane is now built from the **shipped client**, not from a
transcription of it. The authority is the language server the IDE installs
(`AppData/Local/Programs/antigravity/resources/bin/language_server.exe`, Hub
2.32.1), started with the argument set the Hub launcher passes
(`--standalone --override_ide_name antigravity --subclient_type hub
--override_ide_version 2.32.1 --override_user_agent_name antigravity`) and
MITM-captured through mitmdump while it answered one turn per model.

**The prompt fingerprint.** Measured live on one account and one model, only
the system prompt varying:

| system prompt | verdict |
|---|---|
| `You are Claude Code.` | **429 RESOURCE_EXHAUSTED** |
| `you are claude code, anthropic's official cli for claude.` | **429 RESOURCE_EXHAUSTED** |
| `You are Claude Code, Anthropic's official CLI for Claude.` | **429 RESOURCE_EXHAUSTED** |
| `You are an assistant built as Anthropic's official CLI for Claude.` | 200 |
| `You are a Claude agent, built on Anthropic's Claude Agent SDK.` | 200 |
| `x-anthropic-billing-header: cc_version=2.1.322.abc; …` | 200 |
| `You like the name Claude.` / `You are opencode …` | 200 |
| *(no system prompt)* | 200 |

The same account served a 200 for a neutral prompt seconds before each refusal
and its `gemini-weekly` bucket still showed 61% remaining — so the 429 is a
**filtering verdict, not quota**. The identity phrase is what the backend keys
on, and `Anthropic's official CLI for Claude` alone is accepted.

The lane now sends the client's own identity first, the way the shipped client
does:

```
<identity>
You are Antigravity, a powerful agentic AI assistant designed by the Google Deepmind team.
You are working with a USER to solve their task.
The USER will send you requests, which you must always prioritize addressing. User requests are enclosed within <USER_REQUEST> tags.
</identity>
```

and wraps a user turn in `<USER_REQUEST>` … `</USER_REQUEST>`. The caller's own
system text is kept after the identity block; the rewrite of competing
branding (the reference's `ANTIGRAVITY_PROMPT_REWRITES`) stays as well.

**What the capture changed in the wire.** Six divergences, each pinned by a
test in `internal/translator/antigravity_shipped_wire_test.go`:

- `labels.model_enum` — the client sends it on every agent turn. Upstream
  omits it because it had no mapping ("we have no mapping for it, so it is
  omitted rather than guessed"); the mapping is not a guess — the client
  publishes it in the catalog it fetches itself, and the captured table is
  now carried here (`internal/translator/antigravity_model_enum.go`).
- **The output budget is per model**, not one global cap: 65536 for the Gemini
  3.8 family, 65535 for the 2.5 family, 64000 for the Claude family, 32768 for
  gpt-oss. The lane now sends the model's published figure.
- **The Claude family speaks the budget dialect** on this lane
  (`thinkingBudget`), not the level dialect the Gemini families use
  (`thinkingLevel`). A level on a Claude model is a field the backend does not
  read for it.
- `systemInstruction` (camelCase), the key the Cloud Code API publishes and the
  client sends.
- The session id is a signed number (`toNumericSessionId` upstream): a numeric
  client session passes through, anything else folds onto a stable negative
  number. The previous fallback echoed the project id.
- The identity block and the request tags, as above.

**A defect the capture exposed.** A Claude turn that carried a thinking budget
and no output budget answered 400: *"`max_tokens` must be greater than
`thinking.budget_tokens`"*. The output budget now rides along with the
thinking config, which is what the shipped client sends.

**The cooldown.** A spent Antigravity quota is a weekly window, not a burst:
upstream registers the provider as `QUOTA_SCOPES.WEEKLY` with
`usageKey: "gemini_weekly"`, and `resource_exhausted` is one of upstream's
`QUOTA_EXHAUSTED_SIGNALS`. The classifier now answers that verdict with
`newBackoffLevel: 0` and a quota cooldown, the lane's ceiling is one weekly
window, and the account parks until the bucket's own reset instant (the
reading the tracker already holds) or the next Monday 00:00 UTC.

**Verified live** against two accounts copied from the operator's VPS into a
file outside the repository (never committed): `gemini-3.8-flash-medium`,
`gemini-3.8-flash-tiered`, `gemini-3.8-flash-high`, `claude-sonnet-4-6` and
`gpt-oss-120b-medium` all answer 200 through the gateway's own chain, on both
accounts, with the request bytes captured next to the verdict.

**For the reference.** PanRouter's `ANTIGRAVITY_PROMPT_REWRITES` table carries
the billing-header, Hermes and `opencode` rows but not the Claude Code identity
sentence, so it hits the same fake 429. That row, the `model_enum` mapping, the
per-model output budget and the Claude budget dialect belong in PanRouter
first (§0), after which the two trees agree.

Evidence: `internal/translator/antigravity_model_enum.go` (the captured
catalog), `antigravity_thinking.go` (the dialects),
`antigravity_budget.go` (the budget rule) and `antigravity_prompt.go` (the
prompting shape), with the measurements in the comments beside each.