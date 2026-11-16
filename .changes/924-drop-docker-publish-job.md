### ci(release): drop the Docker Hub publish job

Every tag published a red run: the `docker` job logged in with
`DOCKERHUB_USERNAME`/`DOCKERHUB_TOKEN`, neither of which exists, so it failed
with `Username and password required` and `notify` never ran (`needs: [binaries,
docker]`). The image was also never pullable in practice — the workflow pushed
`luqmenul/panrouter-go`, a namespace this repository's owner does not control.

- `release.yml`: the `docker` job is gone, along with `REGISTRY_IMAGE`, the
  `docker_tags` output and its per-channel tag computation, and the now-unused
  `packages: write` permission. `notify` is `needs: [binaries]`, which is what it
  actually needed — the message links the release page, not an image.
- The experimental banner on the release page no longer tells people to
  `docker pull` a tag that does not exist; it pins the install script
  (`VERSION=v1.1.0-exp.1 install.sh`) or points at the asset.
- `scripts/notify-telegram.sh` printed `docker pull <image>:<tag>` in every
  Telegram message. It now prints the install command, pinned to the tag for a
  prerelease. Verified with `--dry-run` for both a stable and an `-exp` tag.
- README: the Docker block is a self-build (`docker build -t panrouter-go .`)
  with the reason no image is published; the experimental section pins the
  installer instead of pulling `:exp`. `docs/RELEASING.md` records the removal
  and that a release now needs no secrets at all; `docs/TELEGRAM_RELEASE_NOTIFICATIONS.md`
  matches the new message.

`ci.yml`'s `Build Docker image (verify only)` job stays — it builds the image on
every PR with no credentials, which is what keeps `Dockerfile` and the PGO
assertion alive.

Verified: `actionlint` clean on the edited workflow, no `REGISTRY_IMAGE` /
`docker_tags` / `luqmenul` references remain, `bash -n` plus two `--dry-run`
renders of the notifier, and `go run ./cmd/changelog-merge --tag v1.0.1 --dry-run`
unaffected.