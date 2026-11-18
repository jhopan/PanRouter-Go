### fix(antigravity): park for the week only on the account's own reading

v1.0.3 shipped a guard that skipped the weekly park when a live reading showed
quota left, but it read the **provider-wide** `gemini_weekly` bucket for every
model. Measured a few hours later, on a Claude turn:

```
connection locked conn=b2d9ef4b provider=antigravity model=claude-sonnet-4-6
  status=429 cooldown_s=393774 until=2026-11-23T00:00:00Z
```

That is 4.5 days taken out of rotation by a body that names no reset, no model and
no error code — for a model the account answered normally in its own client
moments later. Two defects, both in the park decision:

- **The reading was the wrong one.** A Claude turn is answered by
  `claude_gpt_weekly`; `gemini_weekly` says nothing about it.
- **A missing reading was treated as evidence.** The guard only skipped the park
  when a reading existed *and* showed quota; with no reading it fell through to
  the calendar and parked anyway. The quota refresh runs *on error*, after the
  decision, so "no reading" is the normal state at the moment it is made.

The park is now written only when a live reading exists and its **lowest bucket**
is spent. No model is needed for that judgement, and passing one was rejected on
purpose: the decision takes the whole account out of rotation, so it must rest on
the whole account's reading. With no live reading, or a lapsed one, the ordinary
cooldown stands and the account keeps rotating.

Unchanged: a body that names a reset instant still parks to that instant, a spent
bucket still parks to its window, and `503 MODEL_CAPACITY_EXHAUSTED` remains a
transient throttle.

Pinned by `TestCooldownSecFor_AntigravityWithoutAReadingHoldsTheOrdinaryCooldown`,
`TestCooldownSecFor_AntigravityStaleReadingIsNotAPark`, and the three existing
cases (spent bucket parks, healthy bucket rotates, named reset parks). The
handler-level rotation test still passes.

`go test ./internal/handlers/chat` → ok.