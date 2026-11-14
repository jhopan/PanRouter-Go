### Fixed — the MiMo free lane was dialling a host that does not exist

`KnownProviders["mimo-free"]` and `internal/handlers/chat/mimofree.go` both spelled
the host `api.xiaomimimimo.com` — a doubled "mi". The reference
(`open-sse/providers/registry/mimo-free.js`, PanRouter `51e4310e`) uses
`api.xiaomimimo.com`, so every request on that lane resolved to a name with no DNS
record: the hidden free lane could never answer, and the failure read as an
upstream fault rather than a typo.

Both spellings are corrected (`bootstrap` and `openai/chat` endpoints), and the
provider/catalogue tests are green.

### Audit — provider parity is complete after the panrouter-go sync

Re-ran the PanRouter audit against the freshly synced tree (PanRouter HEAD
`51e4310e`):

- **Providers: 147 of 147.** Every entry in `open-sse/providers/registry/` exists in
  the Go registry — the ten that only PanRouter carries come from
  `internal/providers/panrouter_extras.go`.
- **Executors: no missing lane.** `gemini-cli` and `antigravity` are served by the
  native Gemini branch (`IsGeminiNative`), the rest dispatch through the 108
  registered executors or the OpenAI default.
- **Endpoints: 12 groups still unported**, enumerated in
  `docs/PARITY-REMAINING.md` §7 with what each one is (backup/restore, pricing,
  tags, init/locale, shutdown, the MCP bridge, pxpipe, models availability and
  capability detection, providers test-batch/test-models/free-models, usage
  history/logs, OAuth import-token lanes, and the Node-shaped auth paths). The
  engine `/api/v1/*` aliases are Node-internal: this SPA calls `/v1/*`.

Verified: `go build ./...`, `go test -count=1 ./internal/providers/
./internal/handlers/chat/` green, `go test -count=1 ./...` green before this slice.