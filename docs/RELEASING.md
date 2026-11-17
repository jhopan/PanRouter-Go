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
| **version release** | tag `vX.Y.Z`, GitHub Release (marked Pre-release when `VERSION` carries `-exp.N`), five platform binaries + `SHA256SUMS.txt` | the tag push, never edited afterwards |
| **`latest` pointer** | GitHub `releases/latest`, `version.json` on `main` | the newest **non-prerelease** release only |

Nothing is published to an image registry. The release job used to push
`luqmenul/panrouter-go` to Docker Hub and that was removed: a static binary does
not need a container, the login step failed the whole run on every tag when the
secrets were absent, and an image takes `panrouter-go update` out of reach (the
binary being updated would live inside it). `Dockerfile` and `docker-compose.yml`
stay in the repo, and `ci.yml` still builds the image on every PR
(`Build Docker image (verify only)`) so they cannot rot — that job needs no
credentials.

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
**Pre-release** and nothing else: no `latest` pointer moves. GitHub excludes
pre-releases from `releases/latest`, and `bump-version.sh` leaves `version.json`
untouched for a prerelease bump, so a stable install is never handed one.
`autoApplyAllowed` enforces the same rule from the client side: a stable build
never auto-applies a prerelease. Taking one deliberately is a pinned install —
`VERSION=v1.1.0-exp.1 install.sh` — or the asset from the pre-releases page.

## Procedure

```bash
./scripts/bump-version.sh 1.0.1        # VERSION, version.json, updater fallback
git add VERSION version.json internal/updater/updater.go
git commit -m "chore: bump version to 1.0.1"
git tag v1.0.1
git push origin <branch> --tags        # the tag is what publishes
```

`.github/workflows/release.yml` then builds every target, writes `SHA256SUMS.txt`
and publishes the GitHub Release (final or Pre-release, derived from the tag
alone). For a stable tag that release is also the `latest` pointer clients read,
because `version.json` on `main` already carries the same number. Never re-tag a
published version: clients already hold it, and the manifest would advertise the
same number for different bytes.

The changelog entry for a release is assembled from `.changes/*.md` fragments —
never by editing `CHANGELOG.md` in a feature PR (`make changelog-merge TAG=vX.Y.Z`
at release time). See [`.changes/README.md`](../.changes/README.md).

### A release body has a size limit, and the merge has one

`make changelog-merge TAG=vX.Y.Z` folds **every** pending fragment into
`CHANGELOG.md` and empties the folder. That is right when the backlog is small.
It is wrong when the backlog is large, for two reasons that pull in opposite
directions:

- The release workflow takes the **first `## [tag]` section verbatim** as the
  GitHub Release body, and GitHub caps that body at **125,000 bytes**. A merge
  of the whole backlog produces a section far past that, and the release page
  is truncated or the publish fails.
- A fragment that is already in `CHANGELOG.md` and still sitting in
  `.changes/` is rendered **twice** by the dashboard, which assembles both
  (`GET /api/changelog`). So "merge everything, leave the fragments" is not a
  state the dashboard can show correctly.

`v1.0.1` hit both: 78 fragments, 267 KB merged, 1.6 MB section. It took the
same shape `v1.0.0` did — a **curated section** holding only the fragments
added since the last tag, with the older backlog left pending:

```bash
# 1. The fragments added since the last tag are the release's content.
STAGE="$(mktemp -d)"; mkdir -p "$STAGE/.changes"
cp CHANGELOG.md "$STAGE/"
for f in <fragments added since the last tag>; do cp ".changes/$f.md" "$STAGE/.changes/"; done

# 2. The repo's own merge tool writes the section — same code path as
#    `make changelog-merge`, so the section is byte-identical to a full merge.
go run ./cmd/changelog-merge --tag vX.Y.Z --root "$STAGE"

# 3. That section goes above the previous release in the real CHANGELOG.md.
# 4. The merged fragments are removed from .changes/ — otherwise the
#    dashboard shows them twice.
git rm .changes/<those fragments>.md
```

Check the size before tagging: the section must stay under 125,000 bytes.

**Never re-tag a published version**, and never delete a tag to redo a release:
the assets are already on the release page and the manifest would advertise one
number for two sets of bytes. A failed run is recovered with
`gh run rerun --failed` on the same tag.

## Repository secrets the release job needs

The `binaries` job publishes the GitHub Release with the default `GITHUB_TOKEN`
and needs nothing configured — a release with no secrets at all is a green run.
The Telegram job is the only one with a requirement:

| Secret | Used by | Failure if absent |
|---|---|---|
| `TELEGRAM_BOT_TOKEN`, `TELEGRAM_CHAT_ID` (and the topic id) | `notify` | skipped without failing the run (`continue-on-error: true`) |

Nothing here is recoverable-only-by-re-tagging: the release page and its assets
come from the `binaries` job alone.

## The 1.0.0 baseline

`1.0.0` is the first release on the standalone scheme, published 2026-11-16 from
tag `v1.0.0` (five platform binaries + `SHA256SUMS.txt`). It is a **downgrade**
from `1.9.17` as semver, so the updater will not offer it: an install still on
`1.9.x` keeps running that build until it is reinstalled once from the release
assets (or `install.sh` / `install.ps1`). After that, every release is a normal
forward step and the `latest` pointer does the work — `version.json` on `main`
and `releases/latest` move together when the next stable tag is pushed.