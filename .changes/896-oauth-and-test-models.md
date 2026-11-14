### feat(oauth): codex access-token import, Xiaomi MiMo import/login, per-provider model tests

Three dashboard flows had no server surface in this port: pasting a ChatGPT
access token, importing a Xiaomi MiMo key (or discovering one in MiMo Desktop),
and asking "does every model of this provider actually answer?". Each answered
404. This lands all three, plus the account-session login walk the MiMo modal
polls.

**`POST /api/oauth/codex/import-token`** stores a token created at
chatgpt.com/settings as an `access_token` connection, with no refresh flow. The
token's claims are decoded for the account email, ChatGPT account id, plan and
expiry — from the namespaced `https://api.openai.com/{auth,profile}` blocks
first, then the shared Codex extractor — and the answer names the row it wrote
(`workspace`/`plan` come from those claims). Like upstream, a non-JWT token is
still importable: it simply carries no metadata, so the connection is named
`ChatGPT Access Token` rather than guessing.

**Xiaomi MiMo import.** `POST /api/oauth/xiaomi-mimo/api-key` validates a pasted
`sk-` key against the cluster's `/models` endpoint, records the model count, and
normalises the base URL. Validation is a soft gate on purpose: a blocked egress
is not evidence about the credential, so an unreachable endpoint stores the key
marked `untested` instead of refusing it. Dedup follows upstream — same uid,
same key, or the same Desktop identity within the same region updates the
existing row (`updated: true`) rather than starting a second account, and a
session-only import rides `authType: oauth` with the passToken in
`providerSpecificData` so multi-account rotation has something to rotate.
`GET|POST /api/oauth/xiaomi-mimo/auto-import` reads MiMo Desktop's own
`auth.json` (plus the Desktop cookie DB for the session identity, copied before
reading because Desktop holds it exclusively), and answers `found: false` with
the paths it checked when nothing is installed.

**MiMo account-session login.** `POST .../login/start` walks the first two hops
of Desktop's login surface (`me` → SSO → login page) server-side and hands the
browser the same-origin page path plus the `9r_mimo_login` cookie. The session,
including the accumulated cookie jar, travels inside that httpOnly cookie —
start and status are separate requests that share no memory. Non-CN clusters may
need an overseas exit, so `MIMO_LOGIN_PROXY` wins, else the first local HTTP
proxy that answers a CONNECT to the account host is used; CN never proxies.
`GET .../login/status` reports `pending` (re-arming the cookie on every poll, so
a modal that sits on the form longer than the 15-minute TTL still recovers) and
`done` once a passToken is in the jar, then clears the cookie so the identity is
one-shot. A missing, stale or state-mismatched session is a 404 `expired`.

**`POST /api/providers/{id}/test-models`** resolves the connection's provider,
reads its catalogue, and probes every model — each through the endpoint its
service kind serves, via the same probe the dashboard's Test button uses. That
matters: the probe dispatches into the real chat/media handlers, so a model the
button calls dead is a model real traffic fails, and every call is
`handlerutil.WithProbeContext`-marked so nothing lands in the usage ledger or
moves a cooldown. Compatible providers with no static catalogue ask the existing
`/api/providers/{id}/models` discovery in-process, so one node's model list
cannot drift between the two routes. Probes run one at a time and each model is
probed exactly once, even if the registry ever lists an id twice; a handler built
without a probe wired refuses rather than reporting verdicts nothing measured.

**Verified:** `go test ./internal/handlers/oauth/ ./internal/handlers/dashboard/`
and `go vet` over the oauth, dashboard, media and handlerutil packages. The new
tests cover a token import with decoded claims (asserting the stored
`accessToken`, `providerSpecificData.chatgptAccountId`/`chatgptPlanType`/`jwtExp`,
`testStatus` and derived name), the `400 Access token is required` refusal for a
missing or non-string token, the login start hop walk and its two 502s, status
`pending` → `done` from an injected poller with the cookie re-armed and cleared,
the api-key lane's validation/soft-fail/dedup paths, and a provider model test
that asserts one probe per model with per-model pass/fail verdicts.