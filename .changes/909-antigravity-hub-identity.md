### fix(antigravity): the lane sends the captured Hub identity, not the old CLI guess

Asked directly — "so this uses HUB, right?" — the answer was **no**: every
Antigravity call sent

```
antigravity/cli/1.5.1 (aidev_client; os_type=windows; arch=amd64; cl=1014269460; auth_method=consumer)
```

and a connection whose `clientProfile` was not `cli` sent the even older
`antigravity/2.24.0`. PanRouter, the behaviour reference, sends one value:

```
antigravity/hub/2.32.1 (aidev_client; os_type=<host>; arch=<host>; cl=1021296516)
```

`open-sse/providers/shared.js:100-137` records how it was obtained: the shipped
Hub client (`language_server.exe`) was run with the argument set the Hub launcher
passes — `--subclient_type hub --override_ide_version 2.32.1` — against a local
listener, and that is the wire value. The comment states what the capture
settles: `hub` is the *subclient type*, the version is the Hub app's own (not the
IDE release and not its VS Code base), and the tail is a client descriptor rather
than the `darwin/arm64` suffix an earlier guess had invented.

Five differences, all of them a fingerprint: subclient (`cli`→`hub`), version
(1.5.1→2.32.1), the `cl` build label (1014269460→1021296516), a pinned
`os_type=windows; arch=amd64` instead of the machine's own, and a trailing
`; auth_method=consumer` upstream never sends. The repository's own note claimed
the non-`cli` flavour is answered with 404 on the Argon lanes — a claim about the
client that produced it (the standalone `agy` 1.5.1), which the Hub capture
supersedes for this lane.

- `providers.AntigravityUserAgent()` is now the one place the identity is built:
  the hub descriptor, with `os_type`/`arch` derived from `runtime` so a Linux box
  does not claim to be Windows. `ANTIGRAVITY_IDENTITY=cli` restores the previous
  string, which is what makes an A/B test on a live account possible without a
  rebuild.
- `AntigravityUserAgentFor` returns that value for every profile: a profile that
  still changed the header would present an identity no client has. The profile
  stays because other behaviour is keyed on it.
- `X-Client-Version` follows the Hub version, so the header and the User-Agent
  describe the same client.
- The two tests that pinned the old split (`TestAntigravityUserAgentFor`,
  `TestAntigravityProjectUserAgent`) were rewritten to the new contract rather
  than re-pinned: one identity for every profile, the captured shape asserted
  field by field, and the override asserted to change it.

Verified: `go build ./...`, `go test ./internal/providers/ ./internal/handlers/chat/
./internal/handlers/dashboard/ ./internal/handlers/media/ ./internal/proxy/`.