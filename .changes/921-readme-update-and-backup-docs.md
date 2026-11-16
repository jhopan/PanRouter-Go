### docs(readme): updating, the dashboard update check, and backup

The README documented install, run and autostart but never said how to **update**,
even though `panrouter-go version` / `panrouter-go update` and the dashboard's
update banner have existed for releases. A user reading only the README was left
to guess, and the dashboard's `New version available` banner had no documented
counterpart on the command line.

- **Quick Start** gains a "Keep it current (one command)" block: `panrouter-go
  version` (check) and `panrouter-go update` (apply), plus where the same thing
  lives in the dashboard.
- **Setup Guide → 🔄 Update** documents the real self-update path
  (`internal/updater`: release pick → `SHA256SUMS.txt` verification → extract →
  `.old`/`.new` swap → restart), the WebUI check (sidebar version, banner,
  **Update now** dialog, **Auto Update**), the six `/api/version/*` endpoints and
  their auth, the stable vs `-exp` channel rules (`autoApplyAllowed`), and why
  upstream PanRouter's four-part version is not adopted as this binary's version
  (`internal/updater/semver.go` reads three fields; a `0.5.x` latest would read as
  a downgrade for every `1.9.x` install).
- **Setup Guide → 💾 Backup & restore** separates the three things that get called
  "backup": the scheduled `VACUUM INTO` snapshot pushed to a private GitHub repo
  (API-configured — `internal/dbbackup`; there is no dashboard panel), a manual
  copy of `DATA_DIR/db/data.sqlite` (+ `-wal`/`-shm`), and the dashboard's JSON
  export in Settings → Runtime Storage, which is a configuration payload and not
  the database. Restore instructions rely on the additive-only schema bootstrap.

Docs only — no behaviour change. Verified against the source each claim cites:
`internal/updater/updater.go` (`PerformSelfUpdate`, `autoApplyAllowed`,
`restartArgs`), `internal/updater/semver.go` (`parseSemver`), `internal/dbbackup`
(`VACUUM INTO`, `DefaultIntervalHours = 24`), `internal/handlers/router.go`
(version and backup routes), `web/src/components/Sidebar.svelte` and
`UpdateModal.svelte` (banner, dialog, `Auto Update`).