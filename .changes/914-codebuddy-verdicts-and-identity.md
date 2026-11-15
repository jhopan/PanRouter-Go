### CodeBuddy (CN + Intl): verdicts, per-lane bodies, tracing identity

Port of the remaining CodeBuddy behaviour from the PanRouter reference. The
identity, gzip transport, 6004 reset parse (offset from the body, +10 min margin),
CN billing usage handler, catalogue and auto-ping chain were already at parity;
what follows is what differed. Sources: `open-sse/executors/codebuddy-intl.js`,
`open-sse/executors/codebuddy-cn.js`,
`open-sse/shared/codebuddySystemPrompt.js`, `open-sse/config/errorConfig.js`, and
the nine `tests/**/codebuddy-*` suites beside them.

**Three verdicts, not one cooldown.** CodeBuddy's refusals all arrive as 403 (or
400) with overlapping wording, and they mean different things:

- **11140 / "unusual activity" / "restricted due to"** — the ACCOUNT is banned.
  Parked for `BannedAccountCooldown` (30 days, upstream
  `BANNED_ACCOUNT_COOLDOWN_MS`) in the `bannedUntil`/`banReason` pair the repo
  already cleared but never wrote, and now readable by the selector through
  `ConnectionBlockedUntilAt`. A ban is not a cooldown, so it carries no reset — a
  timer would only re-dial the banned account, which is the automation pattern
  that earned the ban.
- **11128 "request illegal"** — the REQUEST is refused (channel verification). The
  account is healthy, so the verdict is fatal: no lock, no rotation, the error
  surfaces. This used to escalate to a 1800s model lock on a healthy account.
- **"request illegal" with no code** — the ban state before 11140 is confirmed.
- **11115 "prompt is too long" / 内容过长** — the request can never succeed. Fatal,
  never locked: an oversized prompt used to walk an 18-account pool locking each
  in turn.
- **"conversation id missing"** keeps its exponential backoff; nothing about the
  account is wrong, but the gateway throttles the caller over it.

`ErrorClassification` gained `Banned`/`Fatal`/`RequestRejected`/`RequestTooLarge`
so the decision survives the round trip, and the fallback and combo loops act on
it. A ban keeps rotating the pool (stopping after N bans failed a request the pool
could serve) and waits `accountRotationDelay` (3s, upstream
`ACCOUNT_ROTATION_DELAY_MS`) before dialling the next account.

**The codes have to reach the table.** The rule table keys on text, and
`extractErrorText` read neither `msg` nor `displayMsg`, so every CodeBuddy verdict
collapsed into the bare 403 rule. It now reads `msg`/`message`/`error.message` in
upstream's order plus every `displayMsg` language, and the two CodeBuddy lanes
classify against the body itself (bounded to 8 KiB), which is what upstream's
`parseError` does with the parsed object.

**The system prompt is the WAF's target.** Tencent reads third-party CLI agent
identity ("You are Claude Code, Anthropic's official CLI…", Cursor, Cline, Aider,
`<agent-identity>`, `cc_entrypoint=cli`) as unauthorized tooling: 11128, then a
restricted account. The ported rule neutralises the CLIENT's own prompt rather
than dropping it — dropping it lost every user and project instruction on every
request — and the previous substring redaction (`[external-client-identity]`) left
the surrounding harness text the WAF reads. The CN lane replaces the whole prompt
(also when it is past 2 000 characters, the catch-all for a harness the pattern
does not recognise) and preserves the content shape; the intl lane folds every
system/developer message into the ONE leading system message the gateway requires,
with `"You are CodeBuddy Code."` only when the client sent none. The
`x-anthropic-billing-header` marker that was in the CodeBuddy list belongs to
Antigravity and is gone from this path.

**Identity.** The intl lane now sends the CLI's per-turn id for the whole family
(`X-Conversation-Request-ID`/`-Message-ID`/`X-Request-ID`/`X-Root-Request-ID`),
the Zipkin B3 + W3C `traceparent` + `X-Trace-ID` block, and `baggage`
(`codebuddy.session_id=<uuid>,codebuddy.conversation_request_id=<hex>`). The
conversation id is now a version-1 (time-based) uuid with the CLI's constant node
id and a timestamp pinned per conversation — a content hash carried neither the
opening time nor a stable value across turns.

Verified: `go test ./internal/translator/ ./internal/proxy/... ./internal/handlers/...
./internal/providers/ ./internal/db/ ./internal/schedule/ ./internal/app/`, plus the
new fallback tests that drive a real 403 through the loop: a 11128 refusal must
leave the pool untouched and unwritten, and a 11140 ban must park one account while
the next one serves the turn.