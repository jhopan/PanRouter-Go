### MiniMax Code (Global + CN): the 24-hour plan block, and the cache that keeps it

Port of `open-sse/services/minimaxCapCache.js`, the two plan-exhaustion rules in
`open-sse/config/errorConfig.js`, and the scope the reference declares in
`quotaWindows.js`.

**A flat 24-hour block, provider-scoped.** "usage limit reached: insufficient
balance (1008)" means the M Plan window or the credit balance is spent. The plan does
announce a reset, but the reference pins a flat 24h — a plan that ran out while a
470k-token conversation was being sent is not worth retrying in a few hours, and each
retry is a real request. The rule is scoped to `minimax-code` and
`minimax-code-global`, so another provider's "insufficient balance" keeps the shorter
generic quota cooldown. The 24h is not clamped: both lanes now declare the reference's
session scope, whose ceiling is the generous default rather than this repo's generic
two hours.

**The block survives a restart.** `internal/handlers/chat/minimax_cap_cache.go` keeps
the verdict in memory and in the `minimax_cap` kv scope, restores it at startup,
drops an entry whose 24h window closed while the process was down, and is fail-open
throughout. A usage reading overrides it: when the plan refills, the tracker says so
and the account is eligible immediately instead of waiting out the day. A credit
balance of zero names no reset, so it parks for the whole window rather than being
re-probed every five minutes.

**Wired where it has to be.** The chat fallback skips a blocked account before the
request goes out (probes excepted), the plan refusal marks the connection, and the
dashboard's usage read records the reading — so routing and the panel cannot disagree
about whether the plan has room.

**One extraction fix the rules needed.** MiniMax words its refusals as
`{"base_resp":{"status_code":1008,"status_msg":"insufficient balance"}}`; the chat
error extraction read neither field, so the plan verdict never fired and the account
was retried instead of parked.

Verified: the cap-cache semantics (block, 24h window, persistence, restore, expiry,
release, fail-open), the 24h classification for both regions with the ceiling applied,
an unrelated 429 staying a burst, and a fallback test that refuses once and then skips
the account without dialling it again. Full `go test` for the touched packages.