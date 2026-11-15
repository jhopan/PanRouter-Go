### fix(service): the Windows autostart entry now actually starts the gateway

Reported as "jalankan terus di background" — it did not stay up. The
`windows-task` backend had two defects, both found by using it on a laptop:

**1. The task could not start at all.** `schtasks /create /tr …` leaves the task
settings at their defaults, and the defaults are hostile to a gateway:
`DisallowStartIfOnBatteries` is true, so on a machine running on its battery
`schtasks /run` succeeds, the task sits in "Queued", `Last Result` stays 0 — and
nothing starts. `StopIfGoingOnBatteries` would then kill it on an unplug, and
`ExecutionTimeLimit` (PT72H) would end it after three days. The task is now
registered from a definition file (`schtasks /create /xml`) that turns all three
off, sets `WorkingDirectory` to the directory install ran from (so the operator's
`.env`, port included, is the one the boot-time instance reads instead of
`%SystemRoot%\System32`), and adds `StartWhenAvailable` plus a one-minute
`RestartOnFailure` so it behaves like the supervision the POSIX backends get from
their service manager. The file is UTF-16LE with a BOM and declares UTF-16:
schtasks refuses a UTF-8 definition with `ERROR: The task XML is malformed.
(1,40)::ERROR: unable to switch the encoding`.

**2. The launched process served nothing.** Task Scheduler hands the task a
console, so `stdinIsTerminal()` was true and the CLI opened its interactive
picker — the task reported `Active: true` while the process waited for a
keypress: no port bound, no boot line in the run log. The autostart entry now
passes `--serve`, a flag that runs the server in this process and never shows the
picker.

`service install` also stopped claiming success from the `/run` request alone: it
reports the state its own probe reads, so "enabled and running" is now something
the task confirmed.

Verified live on Windows, on battery: `service uninstall` → `service install` →
`service start` reports `active: true`, `GET http://127.0.0.1:20219/health` is
200, the dashboard answers 200, and the login works. Pinned by
`TestWindowsTaskXMLKeepsTheGatewayAliveOnBattery`,
`TestWindowsTaskXMLBytesAreUTF16`, the rewritten install test (asserts `/xml`
with no `/tr`/`/sc`/`/it`/`/ru`, and the definition's battery, limit, working-dir
and user fields) and `TestServiceArgsAlwaysAskForTheServerPath`.