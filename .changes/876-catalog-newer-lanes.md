### Added — five catalog lanes upstream publishes that the gateway could not route

`docs/PARITY-REMAINING.md` §5b. Each id was decided from the PanRouter checkout
rather than by preference, because the catalogue is what dispatch *and* the
model picker read: an id missing there is missing everywhere.

| Provider | Ids added | Why |
|---|---|---|
| `cline` | `cline-free/muse-spark-1.3-contributor`, `cline-free/solar-pro4`, `cline-free/longcat-2.0` | upstream's registry lists them (`registry/cline.js:55-59`); Cline's free tier is a lane separate from ClinePass and the ids are gated on Cline product headers, not on the account. |
| `qoder`, `qoder-cn` | `dfmodel_v41` | upstream lists it as "(paid)" and its capability block already exists here (`thinkingFormat: deepseek`, reasoning on, cannot disable). Omitting it left a model the account can buy unroutable while the free `dfmodel` worked. |
| `xiaomi-mimo` | `mimo-v2.6-pro`, `mimo-v2.6-flash`, `mimo-v2.6-pro-ultraspeed` | `registry/xiaomi-mimo.js:64-66`, served through their `xiaomi/…` upstream ids. |

Two ids were deliberately **not** added: `agnes-1.5-flash` / `agnes-2.0-flash`,
because this table already carries the newer generation upstream's snapshot does
not (2.5/3.0) and adding retired siblings to a superset list is churn. No
`codebuddy` alias change was made: no concrete diff against
`registry/codebuddy*.js` was found to justify one.

The dashboard catalogue is kept in step by the existing
`TestDashboardCatalogListsEveryRoutedModel`, which failed on the first attempt
with exactly the ids it needed (`xiaomi-mimo`'s trio and `qoder-cn`'s
`dfmodel_v41`) — the web block was edited until it passed.

Tests: `TestRegistryModelsCarryUpstreamNewerLanes` (new) pins every id above,
`TestDashboardCatalogListsEveryRoutedModel` pins the two catalogues against each
other.

Verified: `go build ./...`, `go test -count=1 ./internal/providers/`,
`cd web && bun run build`, `bun test src scripts` (710 pass).