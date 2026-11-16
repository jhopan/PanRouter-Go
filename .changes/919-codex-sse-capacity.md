### Codex: a 200 that carries an error is now a rotation, not an empty answer

Port of `CodexExecutor._peekSseTransientError` in `open-sse/executors/codex.js`.

The ChatGPT Codex backend can answer HTTP 200 and put the failure in the event
stream: `Selected model is at capacity. Please try a different model.` Without
reading the first bytes, that reached the client as a successful turn that emitted
nothing — no rotation, no retry, and an operator looking at a healthy account whose
answers are empty. Two classes, handled the way the reference handles them:

- **capacity** (`selected model is at capacity`, `model_at_capacity`) → the model is
  out of capacity for this account, so the reply becomes a retryable 503 with the
  reference's wording and the router rotates;
- **overloaded** (`server_is_overloaded`, `service_unavailable_error`) → one more
  attempt on the same account before the rotation is blamed.

The peek stops as soon as the stream is answering (`response.output_text.delta` /
`function_call_arguments.delta`), so a healthy long answer is not buffered to the
256 KiB cap, and the bytes it did read are replayed through a `MultiReader` — the
client still receives the stream byte for byte. The lowercased matching buffer is
grown incrementally rather than rebuilt per read.

Verified: the capacity verdict, the snake-case marker, the overload class, an
unchanged replayed stream, the early stop on user output, the stream/status gate, and
the retryable failure shape. Full `go test` for the touched packages.

The other Codex surfaces (fast tier and `service_tier`, reasoning matrix and review
aliases, tool normalization, reset credits, image fetch/plan-gate/native generation,
STT, token-param allowlist, Responses-lite) were already at parity and are unchanged.