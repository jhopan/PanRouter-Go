### Added — capacity-adapter pools can point at a combo (server and dashboard)

`docs/PARITY-REMAINING.md` §5 (`#4360`). Upstream's capacity-adapter entry is
either an inline model list **or** a combo reference, and the reference is the
one the dashboard writes today: the combo already owns the models, their order
and the fallback/round-robin strategy, so listing them twice meant two orders
that could disagree.

- `normalizeCapEntry` now reads `comboName`, with `combo` as the stored alias
  (`open-sse/services/capacityAdapter.js:24-38`). An old row keeps working: the
  inline list stays parsed exactly as before.
- `adapterConfig` resolves the reference through
  `ChatHandler.adapterComboModels` — the named combo's models, flattened in the
  combo's order — and keeps the inline list only when the combo cannot be read.
  A miss with no inline list still lands on the default fallback pool, so an
  enabled toggle is never a no-op. The result is what
  `AugmentModelsWithCapacityAdapter` prepends, so a capability request the named
  model cannot serve now goes to the combo's models rather than to the default
  pool.
- The dashboard's section is upstream's shape: one combo selector per
  capability instead of up to three chips and a "+N more". The chip row could
  not show the pool it was describing — it truncated at three and its reorder
  arrows were the only way to control an order the combo editor also owns.

Tests: `TestNormalizeCapEntryAcceptsComboReference` (combo name, alias,
precedence, legacy list, disabled row, non-string reference) and
`TestAdapterConfigExpandsComboReference` (models come from the combo in order;
the combo wins over an inline list; an unreadable combo falls back to the inline
list and then to the default pool).

Verified end-to-end against a running gateway: with no combos the selector holds
only `None`; creating `vision-pool` adds it; selecting it writes
`{comboName:"vision-pool", enabled:true, models:[…]}` to `/api/settings`; a
reload shows it selected.

### Changed — the usage trend is loaded when the panel opens

`UsageChart.svelte` was imported statically by `AnalyticsView`, so the chart's
code shipped in the initial bundle for a panel that starts collapsed.

It is now an `{#await import(...)}` inside the panel's open branch. Measured
with `bun run build`: the chart becomes its own `UsageChart-*.js` chunk
**7.30 kB raw / 3.08 kB gzip**, and the main bundle drops from 1 412.23 kB to
1 403.62 kB. Verified in the browser: no `UsageChart-*.js` entry in
`performance.getEntriesByType('resource')` before the panel is opened, one after.

### Changed — the dashboard boot wait is capped at 20 seconds

The boot gate waits on five dashboard reads that carry no timeout of their own,
so a gateway that accepted the connection and then stopped answering left
`Connecting to panrouter-go Localhost Gateway (:20130)...` on screen with
nothing else said — the exact failure shape `AGENTS.md` §6.G.5 warns about,
where the spinner is the only thing the user ever sees.

`boot()` now races `loadData()` against a 20-second budget, and the spinner
switches to a sentence naming the 20 seconds plus a **Retry** button. The reads
keep running, so a late answer still lands and Retry only hastens it. The auth
status check uses the same budget: its failure branch already treats "could not
check" as "no login required".

Verified in the browser by stalling `/api/settings` for 25 s: at t+2 s the
spinner reads "Connecting…", at t+22 s the timeout message and Retry are up, and
Retry with the stall removed renders the dashboard.

### Still open — tray arm64

`cmd/panrouter-tray` is built only by the Windows CI job, because `systray`
reaches Win32 directly there and needs no cgo. An arm64 tray artifact needs a
`CGO_ENABLED=1` build on a macOS arm64 runner (Cocoa) and a Linux arm64 runner
(appindicator), plus release wiring for the second binary. No macOS runner
exists in `.github/workflows/` and this change cannot be verified from a Windows
machine, so it is recorded rather than attempted.