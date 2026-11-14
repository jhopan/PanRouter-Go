### fix(build,web): the Makefile and Dockerfile still said 9router-go; analytics fetched the full payload on every visibility change

Found by running the repository's own CI on the public remote, and then the
binary.

**Build files carried the pre-rename name.** `Makefile` set
`BINARY_NAME := 9router-go`, `CROSS_BIN := 9router-go` and injected the version
with `-X 9router/proxy/...` — a package that no longer exists, so the version
ldflag silently targeted nothing. `Dockerfile` built `-o 9router-go` with the
same dead ldflag and named its entrypoint after it. The cross-compile CI job
failed with `panrouter-go-linux-amd64 was not produced` because `make cross`
emitted `9router-go-linux-amd64`, and the docker job built a binary the Dockerfile
then could not find under the name it copied. Both files now use
`panrouter-go` / `panrouter/proxy`, and `make build` was re-run: it produces
`panrouter-go.exe` with the version injected.

**The analytics read re-ran on every visibility change.** The Overview effect was
`$effect(() => { if (section === 'overview' && overviewLive) loadStats(period) })`,
and `overviewLive` derives from the panel arrangement, which arrives
asynchronously and changes while the view is open. Every such change fetched the
*complete* payload — the four per-model maps that are 98% of it — on top of the
five-second totals-only poll. The read is now keyed on `(section, period)` with
`untrack` around the body, so a visibility flip cannot re-fetch what is already
loaded, while a period or section change still does.

Pinned by `web/e2e/usageStatsViews.test.ts`: every `/api/usage/stats` request in
the poll window carries `views=none`, and the 30 s breakdown tick still issues a
full read.

Verified: `make build` (binary named correctly, version ldflag now resolves),
`bun run build`, and the poll half of `usageStatsViews.test.ts` passing locally.