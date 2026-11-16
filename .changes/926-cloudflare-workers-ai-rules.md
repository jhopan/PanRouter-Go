### fix(providers): Cloudflare Workers AI content shape + the 1015 IP-block rule

The `cf` / `cloudflare-ai` provider was already ported end to end — registry
entry with `hasProviderSpecificData`, 24 models (`@cf/meta/llama-3.2-1b-instruct`
included, plus the image lane and `@cf/cloudflare/clef-flash`), `{accountId}`
substitution in the transport URL with the `CLOUDFLARE_ACCOUNT_ID` fallback, the
validate probe, the image handler and limits via the models.dev catalogue
(`cloudflare-ai` → `cloudflare-workers-ai`). Two provider-specific **rules** were
missing, and both break that model on a normal client:

- **Content parts.** Workers AI validates `messages[].content` against a `oneOf`
  root schema that accepts a plain string and rejects the OpenAI content-part
  array (upstream paramSupport STRIP_RULES, #1926), so any client that always
  sends parts — a translated Claude body, an editor with an image attached, a
  replay — got a bare 400 from Cloudflare naming nothing. `ForwardOpenAI` now
  folds the array into a string for this provider, byte-identical to upstream's
  `.map(b => b?.type === "text" ? b.text : "").join("")`: text blocks concatenate
  in order, every other block contributes nothing, and a body whose messages are
  already strings is returned untouched. Resolved through `ResolveAlias`, so `cf`
  and `cloudflare-ai` share the rule and no other lane is affected.
- **Error 1015.** "You are being rate limited" is Cloudflare blocking the EGRESS
  IP, not the account being throttled (upstream errorConfig.js:93-100). The
  generic `rate limit` row read it as a burst and backed off 2s→5min, so the same
  blocked address was re-dialled every few seconds for the whole block. It now
  parks the connection for an hour, which is what lets the rotation fall to
  another account. The HTML-body rule (managed challenge, cold start) is ported
  with it and stays a short cooldown; both sit above the generic rate-limit row,
  as upstream orders them.

Verified: `go test ./internal/providers/... ./internal/proxy/...` green, with new
tests for the classification (1h for a 1015 body, short cooldown for a plain HTML
body, and a generic "rate limit" still taking the backoff ladder) and for the
flatten rule, including a `ForwardOpenAI` harness that asserts what a fake
upstream actually receives: `say hi` as a string for both `cf` and
`cloudflare-ai`, and the untouched three-part array for `openai`.