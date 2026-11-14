### feat(cli-tools): cowork MCP registry and tools probe

Ports the last two network-backed CLI-tools routes, which had no Go
counterpart:

| Route | Upstream source |
|---|---|
| `GET /api/cli-tools/cowork-mcp-registry` | `src/app/api/cli-tools/cowork-mcp-registry/route.js` |
| `POST /api/cli-tools/cowork-mcp-tools` | `src/app/api/cli-tools/cowork-mcp-tools/route.js` |

Both mount in the dashboard auth group beside `/api/cli-tools/cowork-settings`
(upstream lists `/api/cli-tools` in `PROTECTED_API_PATHS`), so the SPA reaches
them with the session cookie.

#### Registry

`GET /api/cli-tools/cowork-mcp-registry` pages Anthropic's public catalogue
(`limit=500&visibility=commercial,gsuite,gsuite-google`, max 20 pages, cursor
passed through) and answers the upstream envelope:
`{cached, servers:[{name, slug, title, description, url, transport, oauth,
toolNames, toolCount, iconUrl}], total}`. `?refresh=1` bypasses the cache.

`isDirectConnect` is ported verbatim: only `https` remotes, and never
`mcp.claude.com`, `api.anthropic.com/mcp`, templated URLs, or entries whose
`requiredFields` demand a tenant. Entries are deduped by URL, first wins, as
upstream does after paging.

The cache is one bounded slot rather than a map: upstream's 1-hour TTL, a
2000-server cap while paging, and an 8 MiB cap on the serialized entry. A
catalogue past the byte cap is answered but not retained, so the cache can never
grow without bound. The slot's mutex also makes the read single-flight, so a
burst against a cold cache performs one fetch.

A non-2xx page stops paging and returns what was already read — upstream's
`if (!r.ok) break`, not an error. A transport or JSON failure is upstream's 500
`{error, servers:[], total:0}`, and nothing is cached, so the next request
retries.

#### Tools probe

`POST /api/cli-tools/cowork-mcp-tools` speaks the MCP streamable-HTTP
handshake: `initialize`, `notifications/initialized`, then `tools/list`, with
the `mcp-session-id` header propagated between them and an 8-second budget for
all three. Both JSON and `text/event-stream` answers are parsed; the SSE branch
reads the frame with `id: 2`. Results are `{tools:[{name, description}]}`,
`{requiresAuth:true, tools:[]}` on 401/403, and `{error, tools:[]}` for
transport failures or `init <status>`, all with HTTP 200 as upstream answers.
A missing `url` is `400 {error:"url required"}`.

#### SSRF

The probe target is caller-supplied, so it runs through the same gate the
provider-node validation uses: `handlerutil.AssertPublicURL` for remote peers,
with upstream's loopback exemption so a self-hosted MCP server on `127.0.0.1`
stays reachable. Cloud metadata is refused for every caller, including
loopback, matching the provider-node hardening — the exception exists for local
providers, not for instance credentials. Refusals are upstream's
`400 {error:"URL not allowed"}`.

Both routes dial through `proxy.NewSafeTransport`, so even a passed gate cannot
reach a private/reserved address at the socket (and a redirect target is
re-checked at dial time). The fetch is a package-level seam
(`coworkMCPDo`), which is how the tests count calls and answer with fake MCP
payloads without a network.

#### Tests

`TestCoworkMCPRegistry_UpstreamShapeAndFilters`,
`TestCoworkMCPRegistry_CacheTTLAndRefresh` (a second call inside the TTL does
not fetch, a stale entry refetches exactly once, `refresh=1` bypasses),
`TestCoworkMCPRegistry_PagesAndDedupes`,
`TestCoworkMCPRegistry_FetchErrorIsNotCached`,
`TestCoworkMCPTools_ProbeMapsFakeMCPResponse`,
`TestCoworkMCPTools_ParsesSSE`, `TestCoworkMCPTools_RequiresAuthOnInit`,
`TestCoworkMCPTools_RefusesUnreachableServer`, `TestCoworkMCPTools_InitFailure`,
`TestCoworkMCPTools_RefusesPrivateAddress`, `TestCoworkMCPTools_RequiresURL`.