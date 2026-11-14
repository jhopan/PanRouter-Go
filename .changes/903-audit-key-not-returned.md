### fix(security): never return `audit_chain_key` from the settings endpoints

The key is easy to miss in a review because its name does not look like a
credential: `audit_chain_key` is the HMAC key every audit-chain row is signed
with, and deriving it away from the database is the property that stops someone
who can only write rows from forging a chain. `GET /api/settings` (and the echo
`PUT`/`PATCH` answers with) returned the whole stored settings blob, so any
authenticated dashboard session — and anything that logs a response body — got the
key in plaintext. Upstream does not return it either.

- `secretSettingKeys` now carries `audit_chain_key` beside `password` and
  `oidcClientSecret`, which covers the read path and the write echo, since both go
  through `sanitizeSettings`.
- The key stays stored and the chain keeps signing with it; only the response is
  stripped.

Pinned by `TestSettingsNeverReturnTheAuditChainKey`: it seeds a known key, asserts
neither GET nor PUT contains the field or its material, and asserts the 32-byte
key is still readable from the repository afterwards.

Verified live on the rebuilt binary: `GET`/`PUT /api/settings` answer 200 with no
`audit_chain_key`, while `hasPassword` still reports correctly.