### Fixed — the sync had reverted this fork's brand on four functional surfaces

Taking upstream's revisions wholesale overwrote the fork's renames in files it
also expected to keep its own name. Three of the four were functional, not
cosmetic:

1. **The data directory went back to `~/.panrouter`** (`internal/config/config.go`,
   both the Unix home join and the Windows `%APPDATA%` join, plus the bare
   `".panrouter"` fallback). A fresh run would have laid a new SQLite database down
   beside the old one, which reads as data loss. Restored to `~/.panrouter` /
   `%APPDATA%\panrouter`; a `panrouter.db` found in a directory the operator points
   `DB_PATH` at is still picked up as a legacy layout, so an existing panrouter-era
   file is not ignored either. `TestResolveDataDir_DefaultIsPanrouterNamed` pins
   the name so a later sync cannot revert it unnoticed.
2. **The updater pointed at upstream's releases** (`internal/updater/updater.go`):
   `DefaultUpdateURL`, `DefaultGitHubRepo`, the temp-file prefix, the
   `User-Agent` and three log strings all named `panrouter-go`. Self-update would
   have replaced this binary with upstream's. The channel is now
   `jhopan/PanRouter-Go`, and the release-artifact matcher accepts both
   `panrouter-go_*` (this fork's assets) and `panrouter-go_*` (a legacy asset).
3. **The SPA stored and read different keys.** `web/src/components/EndpointView.svelte`
   wrote the API key to `panrouter_key` while `web/src/api/client.ts` reads
   `panrouter_key` (and `App.svelte` looked for `panrouter_auth` /
   `panrouter_pending_*`, `AnalyticsView.svelte` for `panrouter_key`): a key pasted in
   the dashboard was saved where nothing looked for it. All four spellings now
   agree on `panrouter_*`.
4. **CLI, boot banner and telemetry identity**: `cmd/panrouter-go` →
   `cmd/panrouter-go` (and `cmd/panrouter-tray` → `cmd/panrouter-tray`, which is
   also the path the docs already told people to run), the usage/version/update
   strings, the server banner, the audit-boot service name and the OTLP
   `defaultServiceName`. The OAuth callback page's title and its page-scope
   storage key were renamed with them.

Deliberately left alone, all of them contracts rather than branding:

- env aliases `PANROUTER_*` / `PANROUTER_*` (75 references) — a runtime contract;
- the machine-local CLI token (`x-panrouter-cli-token`, `x-panrouter-peer-token`,
  `x-panrouter-real-ip`, salt `9r-cli-auth`) — the CLI derives the same token on the same
  machine, and other instances verify it;
- request/response headers clients already send or read
  (`X-PanRouter-Token-Saver`, `X-panrouter-Context-Window`, …);
- the audit chain's HMAC domain `panrouter/audit-chain/v1` — changing it would break
  verification of every row written before today.

Still upstream-branded and **not** changed, because each is a decision rather than
a rename: `install.sh` / `install.ps1` (they download
`jhopan/PanRouter-Go` release assets, so the one-line installer installs
upstream's binary) and the historical comments/fixtures that mention the two
upstream repositories.

Verified: `go build ./...`, `go test -count=1 ./internal/config/ ./cmd/...
./internal/updater/ ./internal/handlers/oauth/ ./internal/tray/` green, web
`bun run build` + `bun test src scripts` (836 pass).