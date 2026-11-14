### Added — the six missing provider/model endpoints

Ports of the upstream Next routes that had no Go equivalent. Each one is
mounted in the same auth group as its neighbours, so the dashboard SPA reaches
it with the session cookie and the CLI token keeps working:

- `GET /api/models/availability` and `POST /api/models/availability`
  (`src/app/api/models/availability/route.js`). The GET reads every
  `modelLock_<model>` entry out of the connection data blob, reports the ones
  still in the future as `cooldown`, and reports an account whose own
  `testStatus` is `unavailable` and that holds no live lock as `__all`. The
  POST (`{action:"clearCooldown",provider,model}`) clears only the connections
  that actually hold that lock and, when the row itself read `unavailable`,
  restores `testStatus:"active"` with `lastError`/`lastErrorAt`/`backoffLevel`
  reset.
- `POST /api/models/detect-caps` (`src/app/api/models/detect-caps/route.js`).
  Heuristic capabilities from `internal/providers` first, then a text probe for
  liveness and a thinking field, then an image probe only when the heuristic
  does not already declare vision. A reply attributed to another model (the
  Vision Adapter switching a combo) counts as proof the target cannot see.
- `POST /api/providers/test-batch`
  (`src/app/api/providers/test-batch/route.js`). Selects active connections by
  `provider`, `oauth`, `free`, `apikey`, `compatible` or `all` — the auth group
  is the connection's own `authType`, then the registry category, then the
  custom-endpoint prefix — and runs the single-row probe once per connection,
  sequentially, reporting `{mode, providerId, results, summary, testedAt}`.
- `GET /api/providers/kilo/free-models`
  (`src/app/api/providers/kilo/free-models/route.js`). The Kilo registry lane's
  modelsFetcher feed, filtered to `isFree === true`, cached for an hour, and
  answered from the stale cache with a `warning` when a refetch fails.
- `GET /api/proxy-pools/netlify-cli-status`
  (`src/app/api/proxy-pools/netlify-cli-status/route.js`). Probes the runtime
  install, the legacy runtime location and the known PATH directories; reports
  `available/source/path` when found and the manual command, docs link and
  candidate list (plus a stale-install flag) when not.
- `POST /api/tunnel/tailscale-install`
  (`src/app/api/tunnel/tailscale-install/route.js`). Answers an SSE stream:
  `progress` events while `internal/tunnel.InstallTailscale` installs the CLI
  (brew / signed pkg / vendor script / MSI, per platform), then exactly one
  `done` with the login `authUrl` or one `error`. The sudo pre-flight is kept:
  macOS without Homebrew and Linux refuse a missing password as plain JSON
  before the stream opens, and sudo's own refusal reads "Wrong sudo password".

### Verified

- `go test ./internal/handlers/media/ -run TestHandleDetectCaps` — heuristic
  capabilities for a model that declares them (one text probe, zero image
  probes), probe-confirmed vision, combo-switch detection and the invalid-input
  refusal.
- `go test ./internal/handlers/dashboard/ -run
  'TestModelAvailability|TestProvidersTestBatch|TestKiloFreeModels|TestNetlifyCLIStatus|TestTailscaleInstall'`
  — status and body shape for each route's happy path plus one refusal, and the
  batch route's "one upstream attempt per connection" invariant asserted with a
  fake sender.
- `go vet` clean for `internal/handlers/dashboard`, `internal/handlers/media`
  and `internal/tunnel`.