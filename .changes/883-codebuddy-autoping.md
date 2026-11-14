### Added — CodeBuddy Intl daily auto-ping, off unless switched on

Port of `src/shared/services/codebuddyAutoPing.js`: one tiny chat request per
connection per day keeps the account's daily sign-in / activity state alive. Go
drives the existing `internal/schedule` `Scheduler` (`DueSource` + `PingFunc`)
rather than forking a second loop, and `internal/app/server.go` starts it beside
the quota auto-ping loop; it stops with the shutdown context.

**Off by default, and the switch has to be SET.** The scheduler refuses to start
unless `CODEBUDDY_AUTOPING_ENABLED` is exactly `"1"`
(`schedule.CodeBuddyAutoPingEnabled`; the prefixed `PANROUTER_`/`9ROUTER_`/
`NINEROUTER_` spellings are accepted through `internal/envx`). The daily chat
burst is what triggered the CodeBuddy Intl account restrictions —
11140 "request illegal" — so the feature must never default to on. Upstream
wrote this check as `!== "0"`, which defaults to ON: the operator was never
asked, the scheduler started anyway, and it kept pinging accounts that were
already restricted. The switch gates the scheduler itself, not only the
settings path, and `enabled: false` beside a leftover per-connection `true` does
not start it (the connection map says *which* accounts ping, not *whether* the
feature runs).

**The cycle is anchored to 07:00 WIB, computed with a fixed offset.** The cycle
key is the UTC calendar date (`CodeBuddyDayKey`), i.e. the date of the 07:00 WIB
reset: 07:01 WIB today through 06:58 WIB tomorrow share one key, and 07:00 WIB
starts the next one. The arithmetic is a fixed +07:00 (`CodeBuddyCycleMinute`) —
never `time.Local` — because the box runs in whatever zone the operator set and
the cycle must not move with it. Each connection's slot is derived
deterministically from `(connectionId, cycleKey)` (upstream's `h*131` fold mod
the span), spread over minute 1..1438 of the cycle: no RNG at tick time, so every
tick of a cycle and every restart agree on the slot. A connection with no attempt
record at all fires on the first tick that sees it (upstream's `isNew` arm): it
has not had a check-in in any cycle yet, and waiting would mean waiting for a slot
derived for a day it did not exist on. The one upstream arm deliberately NOT
ported is `immediateNextTick`, which fires every connection on the first tick
after start-up or after the settings are switched on — a burst of every account
at one instant is the shape of traffic the master switch exists to prevent, and a
restart would repeat it.

**One ping per connection per cycle.** A connection is skipped when the row
already records this cycle (`lastAutoPingDayKey`, or an older row's
`lastAutoPingAt`), inside the two-hour restart-safe touch throttle, or inside the
one-hour failure cooldown (`CODEBUDDY_AUTOPING_FAILURE` cooldown ported from
`failureCooldownMs`). A failed attempt does *not* consume the cycle — the slot
key carries the attempt number, so the connection is retried once the cooldown
has passed, as upstream does and unlike the quota scheduler, which leaves a
failed slot claimed.

**The ping itself.** The model is the settings override when the operator saved
one, else the tier-agnostic `fast-model` alias, then `default-model` /
`balanced-model`; the walk stops immediately on a 429 (credits exhausted — no
model helps) and only continues on a per-model gating rejection
(11158 / 11101 / "not allowed for user type" / "model … not allowed"). The
credential is refreshed before the request (the Go side of
`refreshAndUpdateCredentials`), the request rides the normal chat lane with the
connection pinned and marked as a probe, and the successful attempt is written
back onto the connection row (`lastAutoPingDayKey` / `lastAutoPingAt` /
`lastAutoPingModel`, merged into the row's data blob so nothing else on the
connection is dropped). Settings and connection rows are read on EVERY tick, so
a saved model override or per-connection switch applies without a restart.

Tests: `TestCodeBuddyAutoPingMasterSwitch` (switch off → zero pings on a due
tick; switch on → one; leftover connection switch → zero), 
`TestCodeBuddyAutoPingEnabled_EnvSwitch` (only `"1"`, respectively the prefixed
spelling, turns it on), `TestCodeBuddyAutoPingFiresOncePerCycle` (one per cycle,
pings again in the next), `TestCodeBuddyAutoPingTwoTicksInOneMinute`,
`TestCodeBuddyAutoPingWaitsForItsSlot` (nothing one minute before the slot,
exactly one at it) and `TestCodeBuddyAutoPingFiresANeverPingedConnection`,
`TestCodeBuddyCycleBoundaryIsWIBMidnight` and
`TestCodeBuddyCycleBoundaryAtPingLevel` (06:58/06:59 WIB belong to the previous
cycle, 07:00/07:01 to the next, asserted with `TZ=America/New_York`),
`TestCodeBuddyPingModelOverride` / `TestCodeBuddyPingModelIsRereadPerTick`,
`TestCodeBuddyPingWalksOnlyOnAModelGatedRejection`,
`TestCodeBuddyAutoPingFailureCooldown` (skip inside the cooldown, retry after
it), `TestCodeBuddyAutoPingSkipsInsideTheTouchThrottle`,
`TestCodeBuddySlotMinuteIsDeterministic`, `TestCodeBuddyPingedToday`,
`TestCodeBuddyPingBody`, `TestCodeBuddySettingsParsing`,
`TestCodeBuddySettingsFromBlob`, and
`TestCodeBuddyAutoPingListFailureIsNotFatal`. The clock is injected and the
sender is a stub, so no test touches the network or sleeps.

Verified: `go test -count=1 ./internal/schedule/` green (all of the above run in
well under a second), `go vet ./internal/schedule/ ./internal/app/` clean.
