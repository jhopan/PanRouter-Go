---
type: fix
title: A bare quota refusal goes straight to the relocation instead of waiting out the backoff
---
### Problem

After the fallback was fixed, Hermes still failed — but with a different symptom:
`POST /chat/completions status=499`, the caller hanging up at 5.6s. The log shows
why:

```text
22:18:44 retrying the same call ... wait=2s attempt=1
22:18:47 retrying the same call ... wait=4s attempt=2
22:18:48 fallback client canceled request ...
```

The bare `RESOURCE_EXHAUSTED` is the request-shape verdict, not a transient
fault: repeating the same bytes after 2s and 4s cannot clear it. It only spent
the caller's patience, so the relocation fallback that does clear it never got a
turn.

### Solution

`internal/proxy/gemini.go`: when the retry loop reads a bare quota refusal, it
hands the body back immediately and breaks, so the relocation runs at once.
`antigravityBareQuotaRefusal` now shares its body test with the loop
(`bareQuotaRefusalBody`), so the case costs no second read.

### Verification

```text
before (95 KB persona, through the gateway):
  retrying attempt=1 (2s) -> retrying attempt=2 (4s) -> 499 at 5.6s
after:
  systemInstruction moved into contents[0] cleared the refusal -> 200 in 5.5s

go test ./...   73 packages ok
go vet ./...    ok
```

The regression test now refuses once and expects the relocation as the very
next request, and asserts the whole call finishes under 2s — it passed in 0.08s
where the backoff version took 6.09s.