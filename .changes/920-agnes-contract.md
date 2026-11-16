### Agnes AI: the 4096 output cap, the daily quota, and the registry entry

Port of `open-sse/providers/registry/agnes.js`, `tests/unit/agnes-max-tokens-cap.test.js`
and `tests/unit/agnes-daily-quota.test.js`.

**`max_tokens` is clamped to 4096.** The hub answers
`400 "max_tokens exceeds the limit of 4096"` above its cap, so the request is lowered
before it is sent (upstream `STRIP_RULES { provider: "agnes", maxOutputCap: 4096 }`).
`FilterOptions` gained `MaxTokensCap` and the clamp runs after the format filter, so a
value at or below the cap, another provider's value, and an absent field are all left
alone. The model's advertised ceiling is a different number and stays in the
catalogue.

**The free text quota is a calendar day, and its 429 is a spent budget.** The field
body — "You have used up today's text quota and are now limited to 1 request every 3
minutes" — also carries `code: "rate_limit_exceeded"`, which the generic rule read as
a 5-second burst; the account was then retried for the rest of the day. A
provider-scoped quota row now wins for that wording (the burst body has no such
wording and stays a burst), `agnes` declares the reference's DAILY scope, and a quota
verdict parks it until the next **00:00 UTC** rather than for a fixed hour.

**Registry parity.** An apikey lane at priority 61 in the apikey category, the
reference's colour and free-tier notice, `passthroughModels` (the catalogue is dynamic
upstream), and both alias spellings (`agnes-ai`, `agnesai`).

Verified: the daily-quota classification, the burst control, the park landing exactly
on 00:00 UTC within a day, the five clamp cases, and the registry fields. Full
`go test` for the touched packages.