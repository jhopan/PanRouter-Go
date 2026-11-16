# Releasing panrouter-go

Versioning is **standalone**: this binary numbers itself, it does not adopt
upstream PanRouter's `0.5.x.x`. Two reasons, both mechanical:

- `internal/updater/semver.go` parses three numeric fields, so upstream's fourth
  component (`0.5.88.40` vs `0.5.88.39`) would compare equal.
- An installed `1.9.x` reading a `0.5.x` manifest sees a *downgrade* and is never
  offered it — every existing install would stop updating silently.

The upstream baseline this gateway reproduces is tracked separately in
[`ARCHITECTURE.md`](../ARCHITECTURE.md); it is a parity note, not a version.

`VERSION` is the single source. `Makefile`, `Dockerfile` and
`web/vite.config.ts` (`__APP_VERSION__`) all read it at build time, and the
release workflow refuses to publish when the tag and the file disagree.

## Two artifacts per release

Upstream's model, kept: one immutable release and one rolling pointer.

| Artifact | What it is | Who moves it |
|---|---|---|
| **version release** | tag `vX.Y.Z`, GitHub Release (marked Pre-release when `VERSION` carries `-exp.N`), five platform binaries + `SHA256SUMS.txt`, docker `:X.Y` and `:X.Y.Z` | the tag push, never edited afterwards |
| **`latest` pointer** | GitHub `releases/latest`, `version.json` on `main`, docker `:latest` | the newest **non-prerelease** release only |

Clients pull the pointer, never a pinned version: `panrouter-go update` reads
`https://raw.githubusercontent.com/jhopan/PanRouter-Go/main/version.json`
(`version.json` → `latestVersion`, `downloadUrl`, `releaseNotes`), falls back to
the GitHub releases API, then downloads that release's asset and verifies it
against `SHA256SUMS.txt` before replacing the binary. `panrouter-go version` is
the same read without the apply.

The dashboard shows the same data: sidebar version + `New version available`
banner, and **Update now** → **Auto Update** calls `POST /api/version/update`.

## Experimental channel

A `VERSION` carrying a prerelease (`1.1.0-exp.1`) is published as a GitHub
**Pre-release** and docker `:exp`, `:1.1-exp`, `:1.1.0-exp.1`. GitHub excludes
pre-releases from `releases/latest`, and `bump-version.sh` leaves `version.json`
untouched for a prerelease bump, so a stable install is never handed one.
`autoApplyAllowed` enforces the same rule from the client side: a stable build
never auto-applies a prerelease.

## Procedure

```bash
./scripts/bump-version.sh 1.0.1        # VERSION, version.json, updater fallback
git add VERSION version.json internal/updater/updater.go
git commit -m "chore: bump version to 1.0.1"
git tag v1.0.1
git push origin <branch> --tags        # the tag is what publishes
```

`.github/workflows/release.yml` then builds every target, writes `SHA256SUMS.txt`,
publishes the GitHub Release (final or Pre-release, derived from the tag alone),
and pushes the matching docker tags. Never re-tag a published version: clients
already hold it, and the manifest would advertise the same number for different
bytes.

The changelog entry for a release is assembled from `.changes/*.md` fragments —
never by editing `CHANGELOG.md` in a feature PR (`make changelog-merge TAG=vX.Y.Z`
at release time). See [`.changes/README.md`](../.changes/README.md).

## The 1.0.0 baseline

`1.0.0` is the first release on the standalone scheme. It is a **downgrade** from
`1.9.17` as semver, so the updater will not offer it: an install still on `1.9.x`
keeps running that build until it is reinstalled once from the release assets
(or `install.sh` / `install.ps1`). After that, every release is a normal forward
step and the `latest` pointer does the work.

Until `v1.0.0` is tagged and pushed, the repository has **no published releases**:
`/releases/latest` returns 404, so `panrouter-go update`, `panrouter-go version`
and both install one-liners have nothing to fetch. The tag is what creates the
`latest` pointer — nothing else needs to move.