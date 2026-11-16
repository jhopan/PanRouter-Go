### docs(readme): credit 9router, 9router-go and OmniRoute; finish the update section

The Credits section named only PanRouter, so the two MIT projects this repository
descends from — both of whose notices are in `LICENSE` — went unattributed on the
page most people read:

- **9router** (decolua and contributors) — the original router that PanRouter
  descends from.
- **9router-go** (Luqmanul Hakim) — the Go port this repository started from; its
  git history was intentionally dropped and its notice is retained in `LICENSE`.
- **OmniRoute** (diegosouzapw) — the secondary, feature-spec reference.

Each entry says what the relationship is (behaviour reference / licence credit and
not a sync source / feature spec), so nobody re-derives a sync from it.

The Update section now also names where release notes come from — the dashboard
update dialog, the releases page, and `CHANGELOG.md` — and points at
`SHA256SUMS.txt` for a manual download. Docs index gains
[`docs/RELEASING.md`](docs/RELEASING.md) and the stale `v1.9.9` release marker is
replaced with the standalone scheme.

Docs only; the license text is untouched.