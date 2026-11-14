### fix(engine): the Gemini `/v1beta` surface is registered, and answers under both prefixes

`internal/handlers/chat/gemini_v1beta.go` had implemented `GET /v1beta/models`
and `POST /v1beta/models/{model}:generateContent|:streamGenerateContent` since
the port, but no route table registered either handler. A Gemini SDK pointed at
the gateway therefore got chi's 404 for every call — indistinguishable, from the
client's side, from a gateway with no models at all. Upstream serves this surface
from `src/app/api/v1beta/models`, and its `next.config.mjs` rewrite makes
`/v1beta/...` and `/api/v1beta/...` the same endpoint.

- Four routes are now mounted in the engine group (`mountEngineRoutes`), inside
  the `RequireApiKey → RequireRateLimit → guardrails` chain: `/v1beta/models`,
  `/v1beta/models/*`, `/api/v1beta/models`, `/api/v1beta/models/*`.
- `apiV1Alias` learns the version segment: a pattern that already starts with
  `/v1` aliases to `/api` + pattern, so `/v1beta/models` becomes
  `/api/v1beta/models` instead of `/api/v1/v1beta/models`, a path neither
  upstream nor any client asks for.
- `geminiActionSuffix` takes the action from the last `/models/` marker instead
  of a hardcoded prefix, so the alias parses the same model as the bare spelling.
  Before this, the alias would have answered `unknown model action`.

Pinned by `TestGeminiActionSuffixAcceptsBothSpellings` (unit),
`TestAPIv1AliasCoversEveryEngineRoute` (walks the real router: every engine
pattern has its alias, both behind the same 401 gate) and
`TestV1BetaGeminiSurface` (integration: 200 with a `models` key on both
spellings, and the alias action route is not a 404).

Verified: `go vet ./...` clean, `go test -p 1 ./...` (73 packages, 0 failures),
`go test -tags=integration ./internal/integration/...` (0 failures).