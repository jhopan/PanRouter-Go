### feat(zed): serve the import and auto-import lanes

Ports the two Zed OAuth lanes the gateway was missing, from upstream
`src/app/api/oauth/zed/{import,auto-import}/route.js` plus
`src/lib/oauth/utils/zedCredentials.js` and `open-sse/shared/zedAuth.js`.

- `POST /api/oauth/zed/import` (`HandleZedImport`) validates the session against
  `cloud.zed.dev/client/users/me` with Zed's own `Authorization: <userId> <accessToken>`
  and `x-zed-system-id` headers, then stores the connection. A missing field is
  `400` (`Access token is required` / `User id is required`), a refused token is
  `401` carrying the upstream message, `systemId` defaults to a minted UUID, and
  the answer keeps upstream's nested shape:
  `{"success":true,"connection":{"id","provider","email","displayName"}}`.
  The row records `authMethod: "imported"`, the user id, system id and the
  resolved organization id, matching `mapTokens` in the ported provider.
- `GET /api/oauth/zed/auto-import` (`HandleZedAutoImport`) reads the signed-in
  Zed IDE session off this machine. It writes nothing, exactly like upstream: the
  page takes the returned session and calls the import lane. `found:false` keeps
  upstream's `error` + `credentialsUrl` shape with `200`, including upstream's
  default wording `Zed IDE credentials not found` when the reader gives no reason.
  POST is registered on the same handler as well, because the dashboard posts.
- The machine read is a `ZedCredentialReader` seam on `OAuthHandler`, so the
  route can be exercised without a keyring or a Zed install. The default reader
  resolves the credential URL from Zed's `settings.json` (`credentials_url` →
  `server_url` → `https://zed.dev`), reads the secret out of the platform store,
  and reads `system_id` from Zed's local `kv_store` (`0-global`/`0-stable`/
  `0-preview`), minting one when the IDE never stored it.
- The platform secret read lives in `internal/browsercookies` as
  `ReadSecrets(SecretQuery)`, beside the keyring code that already serves browser
  cookie keys: the secret service over D-Bus on Linux, `security
  find-internet-password` on macOS, and `CredReadW` out of Windows Credential
  Manager (no PowerShell, no shell). `ErrNoSecretStore` separates "no such item"
  from "no store to ask". Zed keys its item by `url=<credentials URL>` with the
  label `zed-github-account`; the item is preferred by label and accepted without
  it when its account and secret are present, as upstream does.
- The Zed userinfo lookup became one helper shared by the import and exchange
  lanes (`fetchZedUser`, typed `zedUser`), and the connection row write became
  the shared `upsertOAuthConnection`; the exchange lane's best-effort behaviour
  is unchanged — a failed profile lookup still does not refuse a decrypted login.
- `/api/oauth/zed/auto-import` joins `IsAlwaysProtectedPath` beside the Cursor and
  Kiro auto-imports, because it reads a machine-local credential and verifies
  nothing itself.

Verification: `go test ./internal/handlers/oauth/ ./internal/proxy/oauth/ ./internal/browsercookies/ ./internal/middleware/`
passes, `go vet ./internal/handlers/oauth/ ./internal/browsercookies/` is clean.
Coverage added in `internal/handlers/oauth/zed_import_test.go`:
`TestHandleZedImport_storesValidatedSession` (valid payload → stored row, nested
response, numeric user id, org resolution), `TestHandleZedImport_refusedTokenIsUnauthorized`
(401, no row), `TestHandleZedImport_requiresFields` (both 400s),
`TestHandleZedAutoImport_reportsAMachineWithoutZed` (reader's own reason and
upstream's default, `credentialsUrl` null, nothing written),
`TestHandleZedAutoImport_reportsTheFoundSession`,
`TestHandleZedAutoImport_readerFailureIsServerError`, `TestPickZedSecret`, and
`TestZedUserIDString`. No test touches the real filesystem, keyring, or network:
the import lane points at an `httptest` stub through `OAuthHandler.ZedCloudBase`,
and auto-import runs on an injected reader.