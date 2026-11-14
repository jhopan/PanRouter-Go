### fix(cli): a rebuilt binary no longer orphans the running daemon

Reproduced twice while working on this repo, then pinned.

`go build -o bin/panrouter-go.exe` replaces the file through
`bin/panrouter-go.exe~` and renames it over the target. A daemon started from the
old image is afterwards reported by Windows with that temporary name still
attached: `proc.Executable(pid)` answers `…\bin\panrouter-go.exe~` while the pid
claim recorded `…\bin\panrouter-go.exe`. Compared literally the two differ, so the
claim looked foreign, was deleted, and the still-serving daemon became invisible:

```
$ go build -o bin/panrouter-go.exe ./cmd/panrouter-go   # replaces a running image
$ panrouter-go status   → "panrouter-go is not running in background."   (it was)
$ panrouter-go stop      → "not running"                                  (nothing happened)
$ panrouter-go start     → "already running on …:20219 (started outside this binary)"
```

- `resolvedPath` folds a trailing `~` before comparing, on both sides. No install
  ships such a path, so this cannot make two different binaries compare equal.
- `claim.state()` is now three-way instead of a boolean: `claimVerified`,
  `claimUnverifiable` (alive, image unreadable or bare number) and
  `claimForeign` (alive, runs something else). Only the last two-way cases clear
  the file — a claim is deleted when the pid is gone or is provably somebody
  else's, never because its image could not be read for a moment.
- `panrouter-go status` reports an unverifiable claim instead of "not running".
- `stop` still asks the gateway to drain over its own authenticated endpoint
  first; it only falls back to a signal when the pid is verified, so an
  unverifiable claim is never killed blind.

Pinned by `TestResolvedPathFoldsTheBuildRenameSuffix` and
`TestClaimStateSeparatesUnverifiableFromForeign`; and proven live: `status` →
`go build` (replacing the running image) → `status` still reports pid 12760 on
:20219 → `stop` stops it and frees the port.