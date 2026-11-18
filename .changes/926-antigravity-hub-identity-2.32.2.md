### fix(antigravity): the Hub identity moved to 2.32.2 / cl=1022991777

The shared `User-Agent` this port sends was captured on 2026-11-14 and had not
moved since:

```
antigravity/hub/2.32.1 (aidev_client; os_type=<host>; arch=<host>; cl=1021296516)
```

A MITM capture of the **live** client on 2026-11-18 — app 44.9.1,
`language_server.exe` 215543296 bytes, one real agent turn — reads:

```
antigravity/hub/2.32.2 (aidev_client; os_type=windows; arch=amd64; cl=1022991777)
```

Same shape, both numbers advanced. That header is the one thing about a caller
Google reads on every RPC, and an install still claiming the previous Hub build is
a fingerprint no current client sends.

**Everything else in that capture matches this port byte for byte**, so nothing
else moved:

| field | captured live | this port |
|---|---|---|
| system instruction head | `<identity>\nYou are Antigravity, a powerful agentic AI assistant designed by the Google Deepmind team.\nYou are working with a USER to solve their task.\nThe USER will send you requests … <USER_REQUEST> tags.\n</identity>` | identical |
| `generationConfig` (claude) | `maxOutputTokens 64000`, `thinkingBudget 1024` | identical |
| `generationConfig` (gemini high/medium) | `65536`, `thinkingLevel HIGH`/`MEDIUM` | identical |
| envelope | `{project, model, userAgent, requestType:"agent", requestId}` | identical |
| inner keys | `contents, generationConfig, labels, sessionId, systemInstruction, tools` | identical |

The binary carries the prompt as a template with a `generic-system-prompt`
experiment branch:

```
…AI assistant designed by the Google Deepmind team.  (flag on)
…AI coding assistant … working on Advanced Agentic Coding.  (flag off)
```

The flag is per account and arrives in the authenticated AICode config
(`aicode.googleapis.com/v1alpha:fetchConfig` → `experimentConfig.experimentFlags`,
417 flags captured; `generic-system-prompt` was `true` for the accounts here). It is
now read and honoured:

- `providers/antigravity_experiments.go` parses the flag and caches the decision for
  6h, keyed by a **hash** of the access token — the forwarding path already carries
  that, so no provider-wide signature changed and the token is never stored.
- `chat/antigravity_experiments.go` performs the read, riding the existing quota
  refresh because it already holds the connection's token and client. A failed read
  is recorded as unknown with a 15m TTL and keeps sending the generic block.
- `translator.WrapForAntigravityWithIdentity` selects the block;
  `WrapForAntigravity` keeps the old signature and the generic branch, so every
  other caller and test is untouched.

**Every turn now carries the identity block, even when the caller forwarded no
system instruction.** The client has no turn without one — its ~49 KB system
instruction opens with this block — so a request with no system instruction was
going out with no identity at all: a shape the client never sends. An absent or
empty system instruction now gets the block alone.

Verified end to end through the gateway (mitm, 2026-11-18 19:56): the gateway calls
`v1alpha:fetchConfig` itself, then both turns go out with the 295-character identity
block and the sticky flags, at `hub/2.32.2 … cl=1022991777`.

Observed and **ported** — the `labels` bag is conversation state, not a
per-request description:

- `used_claude`, `used_claude_conservative` and `used_non_gemini_model` stay
  `"true"` for the rest of a cascade once any non-Gemini model ran in it; the
  captured client carries all three on the Gemini turns that follow. This port
  derived them from the current model, so a Claude turn claimed
  `used_claude_conservative: "false"` (a combination the client never emits) and a
  later Gemini turn reset the conversation's history. They are now sticky per
  conversation, in a bounded cache (`antigravity_session_flags.go`: 6h TTL, 4096
  slots, degrades to per-request when full) keyed by the cascade id the envelope
  already computes.

Still not ported, with reasons:

- `last_execution_id` is the client's own tool-execution id; this port has no such
  execution to name. The client also omits it on a conversation's first turn,
  which is the shape this port always sends.
- `fetchUserInfo`, `listExperiments`, `cascadeNuxes`, `writeTrajectoryAcls` and
  `retrieveUserQuotaSummary` are client-side RPCs. None gates a completion — the
  capture shows the turn going to `streamGenerateContent` regardless — so they stay
  unported until something depends on them.

Verified: `go build ./...`, `go test ./internal/providers ./internal/translator`
→ ok, and the shipped binary now carries `antigravity/hub/2.32.2` and
`1022991777` with neither old value present.