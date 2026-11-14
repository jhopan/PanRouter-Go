### CLI client adapters for the ten dashboard routes that had none

`/api/cli-tools/<client>-settings` existed in the reference for ten more clients
than the Go gateway served, so the CLI Tools cards for those clients answered
404. Each client now has an adapter in `internal/clientconfig/`, registered from
`clientconfig.IDs()`, so the existing route loop in `SetupServerRouter` serves
`GET` / `POST` / `DELETE` for all of them and a future adapter needs no
registration code.

Ported from `src/app/api/cli-tools/<client>-settings/route.js` (and the helpers
those routes import: `resolveApiKey.js`, `lib/grokBuildConfig.js`,
`shared/constants/coworkPlugins.js`).

#### Clients and what each writes

| Client | File (env override) | Written by `POST` |
|---|---|---|
| `codewhale` | `~/.codewhale/config.toml` | `[openai]` `base_url` (`+ /v1`), `api_key`, `model` (default `provider/model-id`) |
| `forge` | `~/.forge/config.toml` | same table, upstream's field order (`api_key`, `base_url`, `model`) |
| `deepseek-tui` | `~/.deepseek/config.toml` | `provider = "openai"` and `[providers.openai]` with `base_url`, `api_key` (default `sk_9router`), `model` |
| `grok-build` | `~/.grok/config.toml` | `[model.9router]` (`model`, `base_url`, `name`, `description`, `api_backend = "chat_completions"`, `api_key`, `context_window` when the gateway publishes one) and `[models] default = "9router"` |
| `jcode` | `~/.jcode/config.toml`, `$XDG_CONFIG_HOME/jcode/provider-9router.env` | `[providers.9router]` profile (`type`, `base_url`, `auth`, `api_key_env`, `env_file`, `default_model`, `requires_api_key = true`) and `JCODE_9ROUTER_API_KEY` in the dotenv file |
| `smelt` | `~/.smelt/config.json` | `baseUrl` (`+ /v1`), `apiKey`, `model` (request → existing → `provider/model-id`), `_managedBy = "PanRouter"` |
| `copilot` | `%APPDATA%\Code\User\chatLanguageModels.json`; `~/Library/Application Support/Code/User/…`; `~/.config/Code/User/…` | the `9Router` entry of the root array: `vendor = "azure"`, key (default `sk_9router`), one model per requested id with `url = <baseUrl>/chat/completions#models.ai.azure.com`, `maxInputTokens 128000`, `maxOutputTokens 16000` |
| `openclaw` | `~/.openclaw/openclaw.json` (+ `<agentDir>/models.json` per agent) | `models.providers["9router"]`, `agents.defaults.model.primary = 9router/<model>`, the `agents.defaults.models` allowlist, a stripped/re-pointed `agents.list`, and each agent's own `models.json` |
| `cowork` | `%LOCALAPPDATA%\Claude-3p` / `~/Library/Application Support/Claude-3p` / `~/.config/Claude-3p`, plus Claude's `claude_desktop_config.json` | `deploymentMode = "3p"`, `configLibrary/_meta.json`, the profile (`SECURITY_RELAX`, `inferenceProvider = "gateway"`, gateway URL/key, models, default remote MCP servers), and `operonSkipMcpApprovals` |
| `devin` | — (no config file) | nothing: upstream only detects the binary and reports its version; the adapter is a file-less no-op so the route resolves |

`DELETE` undoes each write: the provider table/section/keys are removed
(`codewhale` and `forge` delete their file when nothing else was in it), smelt
removes the file when only its keys were there, copilot filters the `9Router`
entry out of its array, jcode clears the env key with the profile, and cowork
resets the profile to `{}`; grok-build restores `models.default` to
`grok-build`.

#### Deliberate divergences

- **Managed-by comment headers** (`# CodeWhale config — managed by PanRouter`,
  `# Forge config — …`) and jcode's env-file header are not written: a key splice
  cannot insert a comment. Unknown keys elsewhere in the TOML clients
  (codewhale, forge, deepseek-tui, grok-build, jcode) are preserved, where
  upstream re-emits the whole document and drops them.
- **Key resolution.** Upstream's `resolveCliApiKey` falls back to the first
  active dashboard key when the request sends none. The adapters write exactly
  what the request carries (and never the `sk_PanRouter` placeholder), because
  `internal/clientconfig` has no database by design; the dashboard always sends
  the selected key.
- **cowork profile id.** Upstream generates a random UUID on first apply; the
  adapter uses a fixed opaque id so `Plan` and `Apply` name the same file and a
  second apply is byte-identical. An id already present in `_meta.json` is
  reused.
- **grok-build `context_window`** is written only when the gateway publishes the
  model's window (the same `/v1/models` catalog the other adapters use);
  upstream falls back to its bundled capability table.
- **devin** is served by the same three verbs as every other adapter even though
  upstream only has `GET`; it owns no file, so `POST`/`DELETE` are successful
  no-ops.

#### Verified

- `go test ./internal/clientconfig/ ./internal/handlers/media/` and
  `go vet` on both packages.
- Per client, `internal/handlers/media/cli_tool_clients_test.go` asserts: `GET`
  on a missing config answers 200 with the client's file list and an empty plan;
  `POST` writes the exact bytes of the golden fixture (temp-dir home) and a
  second `POST` reports `changed: false`; a config path made unreadable is
  reported as a 500 with an error message and is not overwritten.
- `internal/clientconfig/clients_test.go` pins each client's path (including
  jcode's `$XDG_CONFIG_HOME` and copilot's `%APPDATA%` overrides), apply
  idempotence across the registry, and the upstream placeholder defaults.
- `internal/handlers/cli_client_routes_test.go` walks `clientconfig.IDs()` on
  the real `SetupServerRouter` and asserts every id answers `GET`/`POST`/`DELETE`
  (not 404) while an unknown id still 404s.