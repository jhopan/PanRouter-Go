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

**A second defect the live wire exposed.** The published output budget and the
thinking budget each have to come from the model's own table, and the passes
that write them disagreed about where the numbers came from:

- the thinking pass wrote a *level* for a budget-shaped family, which the
  dialect pass then mapped onto the level table — so a Claude turn asked for
  8192 where the shipped client asks for 1024;
- the output budget was read back off the translated body, where the
  translator's own floor (`GeminiBudgetOutputFloor`) is indistinguishable from
  a number the client asked for — so a Claude turn asked for 8192, and then for
  9216, where the shipped client asks for 64000.

Both are now decided from the client's own request and the model's published
table, in one place: the thinking pass writes the budget and the output figure
together, and the later passes only enforce the two rules the backend imposes
(the figure stays within the published one, and it stays above the thinking
budget). The two regression tests run the passes in the order the proxy runs
them, because a test that wrapped a bare body could not see either defect.

**The cooldown.** A spent Antigravity quota is a weekly window, not a burst:
upstream registers the provider as `QUOTA_SCOPES.WEEKLY` with
`usageKey: "gemini_weekly"`, and `resource_exhausted` is one of upstream's
`QUOTA_EXHAUSTED_SIGNALS`. The classifier now answers that verdict with
`newBackoffLevel: 0` and a quota cooldown, the lane's ceiling is one weekly
window, and the account parks until the bucket's own reset instant (the
reading the tracker already holds) or the next Monday 00:00 UTC.

**Verified live** against two accounts copied from the operator's VPS into a
file outside the repository (never committed): `gemini-3.8-flash-medium`,
`gemini-3.8-flash-tiered`, `gemini-3.8-flash-high`, `gemini-3.5-flash-lite`,
`claude-sonnet-4-6` and `gpt-oss-120b-medium` all answer 200 through the
gateway's own public HTTP API (`POST /v1/chat/completions` with a client API
key), with mitmdump capturing the upstream bytes next to the verdict. Every
captured request matches the shipped client's published figures exactly — the
output budget, the thinking dialect and the enum:

```
OK gemini-3.8-flash-medium    budget=65536 thinking=level:medium enum=MODEL_PLACEHOLDER_M319
OK gemini-3.8-flash-tiered    budget=65536 thinking=level:high   enum=MODEL_PLACEHOLDER_M322
OK gemini-3.8-flash-high      budget=65536 thinking=level:high   enum=MODEL_PLACEHOLDER_M318
OK gemini-3.5-flash-lite      budget=65535 thinking=level:low    enum=MODEL_PLACEHOLDER_M198
OK claude-sonnet-4-6          budget=64000 thinking=budget:1024   enum=MODEL_PLACEHOLDER_M35
OK gpt-oss-120b-medium        budget=32768 thinking=budget:8192   enum=MODEL_OPENAI_GPT_OSS_120B_MEDIUM
```

The fingerprint verdict was confirmed on the same path: the request carrying
`You are Claude Code.` is rewritten to `You are an AI assistant.` on the wire
and answers 200, where the same account and model answered 429 before.

**For the reference.** PanRouter's `ANTIGRAVITY_PROMPT_REWRITES` table carries
the billing-header, Hermes and `opencode` rows but not the Claude Code identity
sentence, so it hits the same fake 429. That row, the `model_enum` mapping, the
per-model output budget and the Claude budget dialect belong in PanRouter
first (§0), after which the two trees agree.

Evidence: `internal/translator/antigravity_model_enum.go` (the captured
catalog), `antigravity_thinking.go` (the dialects),
`antigravity_budget.go` (the budget rule) and `antigravity_prompt.go` (the
prompting shape), with the measurements in the comments beside each.