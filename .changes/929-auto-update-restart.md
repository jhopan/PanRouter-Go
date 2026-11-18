### Auto-update survives a service install

The self-update applied the new binary and then failed to restart into it. On
a gateway installed as a systemd service the two defects compounded: the
gateway kept serving the old binary, and the update looked like it had done
nothing.

**1. The restart resolved a path that the swap had already deleted.**

`PerformSelfUpdate` swaps the running binary: the live file is renamed to
`.old`, the new one is renamed into place, and the `.old` name is deleted. The
restart then asked the OS where the running executable was.

On Linux `/proc/self/exe` is the inode being executed, so once the swap
renamed the binary aside the link followed it there:

```
/proc/PID/exe BEFORE the swap:  /tmp/repro-bin
/proc/PID/exe AFTER  the swap:  /tmp/repro-bin.old (deleted)
```

The `.old` name no longer existed, so resolving it failed with `lstat
…/panrouter-go.old: no such file or directory` — and the gateway kept running
the old binary while the file on disk was already the new one. Measured on the
operator's VPS: the binary on disk read `1.0.1` while the running process was
still `1.0.0`.

The path is now captured **before** the swap, recorded by the update, and
named by the restart — so the restart names the path the new binary was
written to.

**2. A supervised process spawned its own replacement.**

`RestartSelf` spawned a child to take over. Under systemd that puts a second
process in the same cgroup, competing with the supervisor for the listening
port while systemd still counts the outgoing one as alive.

A supervised process now hands the restart back to its supervisor: it exits
with status 256 — the status systemd reserves for a deliberate restart request
— and systemd brings the new binary up. The unit template declares that status
as successful (`SuccessExitStatus=256`), so a planned restart does not land the
unit in the failed state and `panrouter-go service status` keeps reporting the
gateway as active.

The number lives in `internal/constants` because both ends of the protocol need
it: the process exits with it, and the unit declares it. Two copies of the
number would let them drift.

Detection is deliberately narrow: systemd's `INVOCATION_ID` must be present,
and `SYSTEMD_EXEC_PID` must name this process. A shell that inherited the
environment from an earlier unit must not look supervised — it would then hand
its restart to a supervisor that is not watching it, and the gateway would
simply exit.

**Verified.** The Linux-only detection test was cross-compiled and run on the
operator's own VPS: `TestUnderSystemd_NeedsTheMarkerAndThisPID` passes there,
as does the race regression. The mechanism itself was reproduced on that host
(`/proc/PID/exe` → `…old (deleted)`).

`go test ./internal/updater/ ./internal/service/ ./internal/app/
./internal/daemon/ ./internal/shutdown/` → ok. `GOOS=linux go vet
./internal/updater/` → clean.
