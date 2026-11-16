### feat(dashboard): Automatic Backup panel — schedule, mirror tokens, shell snippets

The Go side of upstream's automatic backup has been ported for a while
(`internal/dbbackup`, `/api/backup/{config,run,download,tokens}`, byte-compatible
`bk-` tokens), but there was no way to turn it on from the dashboard: the README
had to say "there is no dashboard panel for it yet", and the schedule could only
be configured with curl. This is that panel, ported from the two cards in
upstream's profile page.

- **Settings → Automatic Backup** (`web/src/components/BackupPanel.svelte`): the
  enable toggle, the interval picker (1 h, 3 h, 6 h, 12 h, 24 h, 48 h, 7 d — the
  same options upstream offers), GitHub username, repository URL and token, plus
  **Save** and **Back up now**. The token field shows
  `•••••••• (saved — leave blank to keep)` once one is stored, because the server
  never returns it.
- The same card mints the mirror tokens (create / revoke / re-enable / delete),
  shows a new token exactly once, and renders the three shell snippets — one-off
  `curl`, `mirror.sh`, the cron line — with the gateway's own origin and a
  `<TOKEN>` placeholder, each with a copy button.
- `web/src/api/client.ts`: `BackupConfig` / `BackupMirrorToken` types and the
  seven `/api/backup/*` calls.
- `web/src/lib/backupMirror.ts` holds the shell text, because a lost trailing
  backslash turns one command into three that fail; `backupMirror.test.ts` pins
  the continuations, the token header, the origin (with and without a trailing
  slash, which would otherwise produce `//api/backup/download`) and that every
  offered interval sits inside the band `ResolveIntervalHours` accepts.
- README's backup section now points at the panel instead of saying it does not
  exist, and keeps the endpoints for scripting.

No Go change: the backend already implemented everything the panel drives.

Verified in a real browser against a local build (`DATA_DIR` = temp dir, port
20255, managed through the relay):

| Step | Result |
|---|---|
| Panel renders on Settings | toggle, interval, three fields, both buttons, snippets |
| Save with a fake token/repo | `POST /api/backup/config → 400`, panel shows the server's own reason: `GitHub: cannot read repo octocat/nonexistent-backup-repo (HTTP 401) — check the token and the repo name` (the private-repo gate runs) |
| Create token | `POST → 200`, `bk-<machineId>-<keyId6>-<crc8>` shown once, list row `active · never used` |
| Revoke → `curl -H "x-backup-token: …"` | `401` |
| Re-enable → same curl | `200`, `4096` bytes, magic `SQLite format 3` |
| Delete | row gone, same curl `401` |
| Accessible names | `Backup interval`, `GitHub username`, `Repository`, `GitHub token`, `Token name` |

The one path the browser test cannot reach is a *successful* save: it needs a real
private repo and token, and creating one on the operator's GitHub account is not
something a smoke test may do. That half is covered server-side by
`TestBackupConfigPOST` (persists the block including `enabled`, never echoes the
token, calls the privacy check once with the parsed owner/repo, and `Configure()`
restarts the scheduler to match).

`make vet-svelte` clean (0 unresolved identifiers; baseline tightened 73 → 72) and
`bun run build` unchanged in weight (4.2 MB `dist`, 1740 KB → 366 KB brotli).