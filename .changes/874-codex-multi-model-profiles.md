### Added — Codex multi-model profiles (`~/.codex/*.config.toml`)

`docs/PARITY-REMAINING.md` §4. Codex runs a named profile with `codex -p <name>`,
and a profile is a file: `<CODEX_HOME>/<name>.config.toml` carrying the model it
points at. PanRouter exposes the three verbs over that directory; this repo had
none of them, so the dashboard had no way to create a second Codex lane.

Ported from `src/app/api/cli-tools/codex-profiles/route.js`:

- `GET /api/cli-tools/codex-profiles` lists every `*.config.toml` except
  `config.toml` (the active configuration, not a profile), reads each one's
  `model` line, and returns `{name, model, command: "codex -p <name>"}` sorted by
  name. A missing directory is an empty list, not an error — a machine that has
  never run Codex has no profiles.
- `POST` writes `# codex -p <name>`, `model = "<model>"`, and
  `model_provider = "PanRouter"` to `<name>.config.toml`, lowercasing the name.
- `DELETE` removes the file, and answers 404 when it is already gone rather than
  reporting a delete that did not happen.

Two guards are load-bearing, both upstream's and both tested with the directory
inspected afterwards:

- The name becomes a file path, so it is restricted to `[a-zA-Z0-9_-]`, at most 64
  characters, and never `config` in any case.
- The model is quoted into TOML, so a quote or a newline in it is refused instead
  of closing the string early.

`CODEX_HOME` wins over the home directory, because the CLI reads it too — a
profile written where Codex does not look is a profile nobody can run. `home=`
is the same preview/test override the sibling settings routes accept.

The `model` lookup is upstream's `^model = "..."` line match, spelled without a
backreference (Go's `regexp` has none): one alternative per quote style.

Tests: `TestCodexProfilesRoundTrip` (file bytes, list shape, delete, 404 on the
second delete), `TestCodexProfilesListIgnoresTheActiveConfig` (only
`*.config.toml` minus `config.toml`; a profile with no `model` line lists an
empty model rather than a guess), `TestCodexProfilesRejectsUnsafeNames` (eight
refused names, then the directory is asserted still empty) and
`TestCodexProfilesRejectsUnsafeModel`.

Verified: `go build ./...`, `go test -count=1 -run TestCodexProfiles ./internal/handlers/media/`
green.