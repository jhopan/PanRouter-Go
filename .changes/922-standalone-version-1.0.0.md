### chore(version): standalone scheme, reset to 1.0.0, and a documented release model

`panrouter-go` now versions itself instead of trailing upstream PanRouter's
four-part number. `1.0.0` is the first release on that scheme.

Upstream's `0.5.88.40` cannot be adopted as this binary's version, and that is
mechanical rather than aesthetic: `internal/updater/semver.go` parses three
numeric fields, so a fourth component compares equal to its neighbours
(`0.5.88.40 == 0.5.88.39`), and a `0.5.x` manifest reads as a **downgrade** to
every installed `1.9.x`, which would silently stop updating. Upstream's baseline
stays where it belongs — a parity note in `ARCHITECTURE.md` — not a version.

- `scripts/bump-version.sh 1.0.0` moved `VERSION`, `version.json.latestVersion`
  and the `internal/updater.CurrentVersion` fallback together; `version.json`'s
  `releaseNotes` was rewritten, the old one still advertised v1.9.11.
- New `docs/RELEASING.md` documents the model the operator asked for and the
  workflow already implements: **two artifacts per release** — an immutable
  `vX.Y.Z` GitHub Release (five platform binaries + `SHA256SUMS.txt`, docker
  `:X.Y`/`:X.Y.Z`) and a rolling **`latest` pointer** (GitHub `releases/latest`,
  `version.json` on `main`, docker `:latest`) that `panrouter-go update` follows.
  Also: the `-exp` channel rules, the bump → commit → tag procedure, and that
  `version.json` can never apply an update by itself (three keys, no digest, so
  `CheckUpdate` falls through to the Releases API for `SHA256SUMS.txt`).
- README's "Version numbering" paragraph now states the standalone baseline and
  links the release doc; `COMPARISON.md` no longer claims v1.9.1 is current.

Behaviour change to state plainly: `1.0.0` is a semver **downgrade** from
`1.9.17`, so the updater will not offer it. An install on `1.9.x` must be
reinstalled once from the release assets; every release after that is a normal
forward step. The repository also has **no published releases yet**
(`GET /api.github.com/repos/jhopan/PanRouter-Go/releases` → `[]`), so
`releases/latest` 404s until `v1.0.0` is tagged and pushed — that tag is what
creates the `latest` pointer.

Verified: `go build ./...` clean, `go test ./internal/updater/...` ok, and a
binary built with the Makefile's ldflags reports `panrouter-go version 1.0.0
(windows/amd64)`.