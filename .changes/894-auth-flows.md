### SSO login flows and password reset

The dashboard could test an OIDC issuer and a SAML configuration, and it served
SAML SP metadata, but the browser flows the login page links to were never
registered. `LoginView.svelte` sends the browser to `/api/auth/oidc/start` (and
`/api/auth/saml/start`), so "Sign in with SSO" answered 404 on every install and
SSO was unusable end to end. `POST /api/auth/reset-password` was in the same
state: the handler existed, nothing mounted it.

Ported from `src/app/api/auth/{oidc,saml,reset-password}/**/route.js` and
`src/lib/auth/{oidc,saml}.js` of the Node reference.

#### Routes

| Route | Auth group | Upstream source |
|---|---|---|
| `GET /api/auth/oidc/start` | public (`PUBLIC_API_PATHS` prefix `/api/auth/oidc`) | `src/app/api/auth/oidc/start/route.js` |
| `GET /api/auth/oidc/callback` | public (same prefix) | `src/app/api/auth/oidc/callback/route.js` |
| `GET /api/auth/saml/start` | public (`PUBLIC_API_PATHS` prefix `/api/auth/saml`) | `src/app/api/auth/saml/start/route.js` |
| `POST /api/auth/saml/acs` | public (same prefix) | `src/app/api/auth/saml/acs/route.js` |
| `POST /api/auth/reset-password` | dashboard + local-only (`LOCAL_ONLY_PATHS`, not public) | `src/app/api/auth/reset-password/route.js` |

The four flow routes are mounted in `SetupServerRouter`'s public block: the
login page links straight to them and the caller has no session yet. Each
handler answers a refused login with the redirect upstream sends —
`/login?error=<reason>` — never a JSON body. `reset-password` stays inside
`SetupDashboardRoutes`, matching upstream's guard, which requires a session (or
the CLI token) *and* a loopback peer for that path; the handler repeats the
loopback / same-origin gate so a direct mount is safe too.

#### OIDC

Authorization Code + PKCE.

- `/start` gates on `getOidcRuntimeConfig`: `authMode` must be `oidc` or
  `both`, and issuer URL, client id and client secret must all be set. Anything
  else redirects `error=oidc_not_configured`, exactly as upstream's `null`
  config does.
- The authorization URL carries `response_type=code`, the stored `client_id`,
  `redirect_uri=<origin>/api/auth/oidc/callback`, `scope` (stored, default
  `openid profile email`), random `state`/`nonce`, and `code_challenge` +
  `code_challenge_method=S256`.
- `state`, `nonce` and the PKCE verifier ride in `oidc_state`,
  `oidc_nonce`, `oidc_code_verifier` cookies: httpOnly, `SameSite=Lax`, path
  `/`, `Max-Age=600`, `Secure` when the request is over TLS / `X-Forwarded-Proto:
  https` / `AUTH_COOKIE_SECURE`.
- `/callback` refuses, in upstream's order: a provider `?error=` (forwarded),
  missing `code`/`state` (`oidc_missing_code`), a missing or mismatched state
  cookie (`oidc_invalid_state`), unconfigured OIDC, a failed discovery, a
  failed token exchange, a response without `id_token`. Flow cookies are cleared
  on every outcome.
- The id_token is verified against the discovery JWKS (RS256 only; a key
  rotation forces one refresh), then `iss`, `aud`, `exp`, `nbf` and the flow's
  `nonce`. A token signed with another algorithm is refused rather than read.
- On success the dashboard session cookie is issued with upstream's claims
  (`oidc: true`, `oidcSub`, `oidcEmail` from `email`, `oidcName` from
  `preferred_username | email | name | given_name | sub`, falling back to
  `"OIDC user"`), and the browser lands on `/dashboard`.

#### SAML 2.0 (SP-initiated, POST assertion)

- `/start` requires `samlEntryPoint` and `samlCert` (`isSamlConfigured`), builds
  an `AuthnRequest` with `NameIDPolicy` email format, deflates it with raw
  DEFLATE, base64-encodes it, and redirects to
  `<entryPoint>?SAMLRequest=...`. The request ID is stored in `saml_state`
  (10 minutes, httpOnly, `SameSite=Lax`).
- `/acs` consumes the `application/x-www-form-urlencoded` `SAMLResponse` field,
  deletes `saml_state` on every attempt, and refuses with upstream's redirect
  reasons: `saml_missing_response`, `saml_not_configured`, and the validation
  error for a malformed assertion.
- The signature is verified with `goxmldsig` against the configured
  certificate before any claim is read, and claims come from the validated
  element. XML Signature Wrapping is the failure this ordering exists to
  prevent: a signed assertion plus a second unsigned one must not authenticate
  as the unsigned identity.
- `InResponseTo` must name the request this server started (when a state cookie
  is present), `Conditions` must cover now, and the `AudienceRestriction` must
  include this ACS URL — a missing audience fails closed, as SAML Core
  §2.5.1.4 requires.
- Failures feed `internal/auth`'s login limiter (upstream
  `loginLimiter.recordFail`), so repeated garbage posts escalate to
  `Too many failed attempts. Try again in Ns.`; a verified login clears the
  bucket. Success issues the session (`saml: true`, `samlEmail`, `samlName`)
  and redirects to `/dashboard`.

#### `POST /api/auth/reset-password`

Byte-for-byte upstream: the route takes **no body** and clears the stored
password hash, falling back to `INITIAL_PASSWORD` / `123456`, answering
`{"success":true}` (500 `{"error":...}` on a store failure). It never echoes the
password literal.

The network gates are upstream's too, enforced in the handler: loopback peer
only (client-controlled `Host` is ignored), same-origin / CSRF check against
`Origin` and `Referer`, and a per-peer rate limit (3 per 10 minutes). A caller
from another host gets 403 `{"error":"Password reset is only available from
localhost"}`.

Nothing on this route asks for the current password: a reset exists for the
operator who no longer has it. The `Current password required` /
`Invalid current password` pair belongs to the settings password-change flow
(`PATCH /api/settings`) and is tested there.

#### Deliberate divergences

- **Origin pinning.** `publicOrigin` uses `BASE_URL` / `NEXT_PUBLIC_BASE_URL`
  when set, otherwise the request's own scheme and `Host`; it never trusts
  `X-Forwarded-Host` / `X-Forwarded-Proto`. SAML additionally honours upstream's
  `settings.baseUrl`. Both values this feeds (the OIDC `redirect_uri` and the
  SAML ACS URL) decide where an authorization code is delivered, so a forged
  header would be code interception. Without a configured base URL and without
  TLS the flows refuse to start (`oidc_base_url_required` /
  `saml_base_url_required`) instead of building a plain-http `redirect_uri`; an
  operator behind a TLS-terminating proxy must set one.
- **Reason strings.** Where upstream forwards an exception's `message` into
  `error=`, the Go flow uses a stable reason (`oidc_discovery_failed`,
  `saml_invalid_response`, ...). Status and destination match; the text is not
  an upstream contract.
- **SAML signature scope.** Upstream sets `wantAssertionsSigned: true`, so only
  an assertion carrying its own `<Signature>` is accepted. This port validates
  whatever element the document signs (the assertion or the enclosing
  `<Response>`) and then reads the claims from that validated element, so a
  response signature — which covers the assertion — is accepted too. Both shapes
  are the IdP's own signature over the identity; the wrapping defence is the
  ordering, not the element the signature sits on.
- **SAML request signing.** Upstream's `generateAuthorizeRequestAsync(false,
  false)` signs neither the request nor the redirect, so neither does this
  port. The response side always requires a signature this gateway can verify.

#### Verified

- `go build ./internal/handlers/sso/ ./internal/auth/` and
  `go vet ./internal/handlers/sso/ ./internal/auth/` clean.
- `go test ./internal/handlers/sso/` — OIDC start (unconfigured redirect, SSO
  disabled / missing secret redirect, origin refusal, authorization-URL query:
  client id, redirect URI, response type, scope, PKCE S256, state/nonce/verifier
  cookies bound to the URL), OIDC callback (bad state, provider error,
  missing code), SAML start (unconfigured, origin refusal), SAML ACS (missing
  field, invalid base64, verified assertion opens a session, repeated failures
  lock out), assertion verification (valid, foreign signer, expired, wrong
  audience, missing audience, wrong/missing `InResponseTo`, unsigned,
  signature wrapping), certificate shapes, redirect-binding deflate.
- `go test ./internal/auth/` — session claims round-trip including the new
  `oidcSub`, cookie, lockout and client-IP behaviour.
- `go test ./internal/handlers/sso/ ./internal/auth/ ./internal/handlers/dashboard/`
  and `go test ./internal/handlers/` — full package runs green on the touched
  packages.
- `go test ./internal/handlers/ -run TestSetupServerRouter_AuthFlowsAreLive` —
  boots the real router: the four flow routes answer anonymously with
  `/login?error=...`, `reset-password` answers the dashboard gate's 401
  anonymously, clears the verifier for a loopback session, and refuses the same
  session from a remote peer.
- `go test ./internal/handlers/dashboard/ -run TestHandleResetPassword` — bare
  POST clears the verifier (state asserted, not bytes), the old password no
  longer logs in, bodies are ignored, and non-loopback / cross-site callers get
  403.
- `go test ./internal/handlers/dashboard/ -run TestChangeDashboardPassword` —
  the settings password change requires the current password, answers
  upstream's `Invalid current password`, leaves the verifier untouched on a
  mismatch, and replaces it on success.