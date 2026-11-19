### fix(antigravity): request Hub/CLI's required `aicode` OAuth scope

A direct read-only comparison of the failing VPS token against a live official
Hub/CLI token found the first credential-level difference that survives every
wire/profile experiment:

| scope | official Hub/CLI token | VPS connection token |
|---|---|---|
| `cloud-platform` | yes | yes |
| `cclog` | yes | yes |
| `experimentsandconfigs` | yes | yes |
| **`aicode`** | **yes** | **no** |

The missing scope exactly explains this v1.0.6 log line:

```text
GET https://aicode.googleapis.com/v1alpha:fetchConfig
→ 403 PERMISSION_DENIED: Request had insufficient authentication scopes.
```

That authenticated config carries `experimentConfig.experimentFlags`, including
the account-specific `generic-system-prompt` decision. Without it every VPS
account silently falls back to the generic identity branch, even after the
request method was corrected from POST (404) to GET.

The Antigravity OAuth authorization request now includes:

```text
https://www.googleapis.com/auth/aicode
```

alongside the existing official scopes. `TestHandleAntigravityAuthorize` was red
until the scope appeared in the generated Google authorization URL, then green.

**Existing connections cannot gain a new OAuth scope through token refresh.**
Each Antigravity account must log in again after this release; Google then issues
a new refresh token carrying `aicode`, `fetchConfig` can read the real identity
branch, and the Hub wire is chosen per account rather than guessed.

Verification:

```text
go test ./internal/handlers/oauth ./internal/providers ./internal/translator \
  ./internal/proxy ./internal/handlers/chat \
  -run 'Antigravity|RefreshAntigravityExperiments|HandleAntigravityAuthorize' → ok
```
