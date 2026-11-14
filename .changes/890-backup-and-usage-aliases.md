### Added — database-backup routes and the older usage log/history aliases

Ports the last missing pieces of the Next dashboard's API surface:
`src/app/api/backup/{config,run,download,tokens}/route.js` and
`src/app/api/usage/{history,logs,request-logs}/route.js` plus
`src/app/api/usage/[connectionId]/check-release/route.js`.

**Routes added** (`internal/handlers/dashboard/routes.go`,
`internal/handlers/router.go` — both the tests-only dashboard table and the
real server table, so a route cannot be reachable in a test and 404 in
production):

| Route | Behaviour |
|---|---|
| `GET /api/backup/config` | enabled / intervalHours / githubUser / repo + owner + URL, and `githubTokenConfigured` — never the token |
| `POST /api/backup/config` | validates, requires a **private** repo (GitHub read is the hard gate), stores the config, starts/stops the scheduler |
| `POST /api/backup/run` | one forced backup: `{success, fileName, bytes}`, or `400 {error: reason}` |
| `GET /api/backup/download` | the raw SQLite file, authenticated by `x-backup-token` (header or `?token=`) only |
| `GET/POST/PATCH/DELETE /api/backup/tokens` | mirror-token CRUD; the full `bk-…` token is returned once, by the POST |
| `GET /api/usage/history` | the lifetime usage aggregate (upstream's route takes no parameters and calls `getUsageStats()` with its `"all"` default) |
| `GET /api/usage/logs`, `GET /api/usage/request-logs` | one feed: 200 recent rows as `DD-MM-YYYY HH:mm:ss \| model \| PROVIDER \| account \| sent \| received \| status` |
| `POST /api/usage/{connectionId}/check-release` | reads the provider's live quota; lifts the block only when the provider says the account is usable |

**Status codes, JSON keys and error strings are upstream's**, including the
`400 "GitHub username and repository are required"` / `"GitHub token is
required"` / `"Invalid repository name"` / GitHub privacy-gate refusals, and the
token verdicts `Malformed token` / `Invalid token` / `Unknown token` /
`Token has been revoked`. The backup routes answer the flat `{error: "…"}`
shape the dashboard reads as text (`writePlainError`), not the OpenAI error
envelope.

**The GitHub token is a secret and stays one.** `POST /api/backup/config`
writes it into the settings row; `GET /api/settings` now strips
`dbBackup.githubToken` and substitutes `githubTokenConfigured`, and a settings
write that carries a `dbBackup` block without one preserves the stored value
(upstream's `SECRET_SETTING_PATHS` guard). An empty token in the config POST
means "keep the stored one"; an explicit `""` in a settings write still clears
it, as upstream does.

**The download is the one route without the dashboard credential.**
`middleware.RequireDashboardAuth` admits exactly `GET /api/backup/download` and
nothing else, because the unattended mirror holds a `bk-…` token rather than a
session — upstream lists the same path in `PUBLIC_API_PATHS`. The handler runs
the full token check itself (CRC first, then the record, then `isActive`), so an
anonymous request without a valid token still gets `401`.

**Storage.** `backupTokens` joins the core schema (`internal/db/schema.go`) with
upstream's columns, and `internal/db/backup_tokens.go` ports the token format
(`bk-{machineId}-{keyId6}-{crc8}`, HMAC-SHA256 keyed by `BACKUP_TOKEN_SECRET`),
the CRUD and the verification order. The full token is never stored — only the
key id, machine id and CRC. The backup runner itself
(`internal/dbbackup`) ports `dbBackup.js`: `VACUUM INTO` snapshots (a plain copy
misses the WAL), the GitHub private-repo gate, the clone/write/prune/commit/push
cycle keeping 30 snapshots, interval resolution clamped to 1–168 hours, and the
5-minute scheduler started/stopped by the config write.

**Check & release** clears the per-model locks, the ban fields, the account
cooldown, the error code and the backoff counter through
`db.ReleaseConnectionBlocks` — and only after a live provider read. MiniMax Code
answers with its quota rows, Antigravity with its eligibility read, and every
other provider goes through the generic quota path; a provider with no usage API
leaves the block in place and says so.

Tests (no network, no sleeping, injected clock and side effects):
`TestBackupConfigGET`, `TestBackupConfigGETLegacyRepoShape`,
`TestBackupConfigPOST`, `TestBackupConfigPOSTRejections`, `TestBackupRun`,
`TestBackupDownload`, `TestBackupTokensRoutes` (asserts the created token never
comes back from the list), `TestSettingsHidesAndPreservesBackupToken`,
`TestCheckRelease` and `TestCheckReleaseRequiresDashboardAuth`;
`TestHandleUsageLogsFormatsTheFeed`, `TestHandleUsageLogsAccountFallsBackToIDPrefix`,
`TestHandleUsageLogsEmptyLedger`, `TestHandleUsageHistoryIgnoresQueryPeriod`,
`TestDashboardRouteTableServesTheNewRoutes`,
`TestDashboardRouteTableExposesBackupDownload` and
`TestNewRoutesRefuseAnonymousCallers`;
`TestBackupDownloadIsPublicAndSiblingsAreNot` (the middleware exception is one
path wide); `TestBackupTokenStoreKeepsOnlyTheParts`, `TestCheckBackupToken`,
`TestCoreSchemaIncludesBackupTokens`, `TestReleaseConnectionBlocks`;
`TestParseRepoRef`, `TestResolveIntervalHours`, `TestConfigFromSettings`,
`TestRepoNameLooksValid`.

Verified: `go test ./internal/handlers/ ./internal/handlers/dashboard/
./internal/db/ ./internal/dbbackup/ ./internal/middleware/` green,
`go vet` clean on the same packages plus `./internal/auth/`.