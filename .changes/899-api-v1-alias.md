### feat(engine): serve the data plane under `/v1` and `/api/v1`

Upstream registers every engine route twice. The Next routes live in
`src/app/api/v1/**` (`chat/completions`, `messages`, `responses`, `embeddings`,
`images/generations`, `audio/*`, `models`, `videos/*`, `search`, `web/fetch`,
`systemone`, and Ollama's `api/chat`), and `next.config.mjs` rewrites
`/v1/:path*` onto `/api/v1/:path*`. Both spellings therefore address one
endpoint, and a client written against the Node tree may call either.

This port registered the routes once, bare, and `middleware.RequestLogger`
strips a leading `/v1/` before chi matches — so `/v1/chat/completions` worked
while `/api/v1/chat/completions` fell through to chi's 404. The `/models` pair
was doubled by hand; nothing else was.

#### The prefix rule

`SetupRoutes` now mounts the engine surface through `apiV1Mounter`
(`internal/handlers/router.go`), which registers each pattern twice:

| Registration | Serves |
|---|---|
| `pattern` (e.g. `/chat/completions`) | `/chat/completions` and, via the `RequestLogger` strip, `/v1/chat/completions` |
| `apiV1Prefix + pattern` | `/api/v1/chat/completions`, untouched by that strip |

Both registrations receive the **same `http.HandlerFunc` value**, so a prefix
cannot drift onto a second handler, and a route added to `mountEngineRoutes`
cannot be missing one of the spellings. That function is the one place the
engine surface is listed; the dashboard-facing mirrors (`/api/models`,
`/api/changelog`, `/api/media-providers/tts/*`, `/headroom/*`, `/api/usage/*`)
stay single-prefix because upstream publishes them under `/api/**` behind
`dashboardGuard`, not under `/api/v1/**`.

The `/api/v1/*` routes are registered inside the same group as `/v1/*` in
`SetupServerRouter` — `RequireApiKey` → `RequireRateLimit` →
`guardrails.Inbound` — not in the dashboard group, so the alias inherits the
engine's key gate, per-key RPM/TPM/concurrency ceiling and inbound guardrail
policy.

The hand-written `GET /api/v1/models` and `GET /api/v1/models/*` lines are
gone: `mountEngineRoutes` produces them from `/models` and `/models/*`. The
older `/v1/models`, `/v1/audio/music` and `/v1/audio/sound-generation`
registrations stay as they were, and every existing bare-path registration is
unchanged, so no `/v1` behaviour moved.

#### Verification

- `TestAPIv1AliasCoversEveryEngineRoute` (`internal/handlers/router_api_v1_test.go`)
  walks the router `SetupServerRouter` builds with `chi.Walk`, and derives the
  expected set from `mountEngineRoutes` itself — a recording `routeRegistrar`
  captures every registration, so no hand-written route list can go stale. For
  each route it asserts the bare pattern and its `/api/v1` twin are both
  registered with the same method and the same handler (pointer identity). For
  every POST route it then probes both spellings with an unknown key and
  requires 401 from both — proof the alias sits behind the engine's API-key
  gate, not the dashboard's — while a made-up `/api/v1/...` path must still
  answer 404, so the probe is not vacuously satisfied.
- `TestV1APIv1AliasReachesTheSameEngineHandler`
  (`internal/integration/api_v1_alias_test.go`, build tag `integration`) boots
  the production stack and posts the same chat body to `/v1/chat/completions`
  and `/api/v1/chat/completions`: both answer 200, both reach the fake provider
  with model `deepseek-chat` (two upstream calls total), `GET /api/v1/models`
  answers 200, and a bogus key on the alias is refused 401 without touching the
  provider.

Verified: `go test ./internal/handlers/`,
`go test -tags=integration ./internal/integration/ -run V1`, and `go vet` (with
the `integration` tag) on both packages.