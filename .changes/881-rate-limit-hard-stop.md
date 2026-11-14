### Fixed

- **A CodeBuddy Intl rate limit no longer walks the account pool.** A 429 from
  `codebuddy-intl` (6004 `超出频率限制`) now gets zero retries, locks the
  account until the provider's own reset plus a ten-minute margin, and stops
  the rotation: the caller receives the provider's error instead of the next
  account's attempt. Appending the account to the exclusion list and continuing
  produced one request per account in the same second — the automation
  signature the provider answers with `11140 request illegal` and 30-day bans
  (mirrors `src/sse/handlers/chat.js` `CODEBUDDY_STOP_ON_LIMIT` and
  `open-sse/config/errorConfig.js:130-151`). Every other provider keeps its
  existing pool walk.

- **The reset timestamp is read with the offset printed in the body.** A body
  saying `your usage will reset at 2026-11-02 10:05:00 UTC+8` parses to
  `2026-11-02T02:05:00Z` (+10m margin → `02:15Z`, i.e. `10:15 WITA`) no matter
  which zone the server runs in. It was previously built from the server's
  local zone, so on the WIB (UTC+7) deployment the lock landed an hour off the
  real reset and the premature retry was what the provider read as automation
  (mirrors `open-sse/executors/codebuddy-intl.js:26-33,376-391`). A body with
  no offset keeps upstream's UTC+8 fallback; `UTC±N` and `UTC±hh:mm` are both
  honoured, and a malformed offset is rejected rather than clamped.

### Verification

- `go test ./internal/handlers/chat/ -run 'RateLimit|Rotation|Reset'`: the 429
  hard-stop test asserts exactly one upstream attempt across two seeded
  `codebuddy-intl` accounts (and that a `deepseek`-style provider still rotates
  to the second), and the reset tests assert the absolute lock instant for a
  fixed 2026-11-02 body while `time.Local` is WIB, WITA and UTC.
- `go test ./internal/db/ -run Quota` and `go vet` on both touched packages.