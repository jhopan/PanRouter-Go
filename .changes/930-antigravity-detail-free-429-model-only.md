### fix(antigravity): detail-free 429 is a model refusal, never account quota

The VPS reproduced this body on 192-message / 13-tool agent turns while the same
account/model answered `200` to the dashboard's minimal two-message test:

```json
{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"RESOURCE_EXHAUSTED"}
```

It carries no reset instant, quota bucket, RetryInfo, model-capacity detail or
other authority that the account spent its weekly allowance. The global
classifier matched `RESOURCE_EXHAUSTED` as quota, wrote `rateLimitedUntil` and
`quotaExhaustedUntil` for one hour, and then every later request failed locally
as `502 skip unhealthy`.

For provider `antigravity` only, this exact detail-free shape now means a
request/model fingerprint refusal:

- first backoff is two seconds;
- only the requested model gets a short model lock;
- no `rateLimitedUntil` or `quotaExhaustedUntil` is written;
- stale cached `0%` quota cannot upgrade it to a weekly lock;
- account rotation continues to a connection that may accept the payload.

A real quota response remains quota when it includes a reset, RetryInfo, quota
bucket/detail, `individual quota`, or other authoritative provider signal.
`503 MODEL_CAPACITY_EXHAUSTED` remains the existing short model-capacity backoff.

Regression proof was red against the old code: it found an account cooldown and
a six-day cache-derived lock after the exact body. It now passes:

- `TestClassifyErrorForProvider_AntigravityDetailFree429IsNotQuota`
- `TestHandleAccountFallback_AntigravityDetailFreeRefusalOnlyLocksModel`
- `TestCooldownSecFor_AntigravityDetailFreeRefusalIgnoresExhaustedCache`

Focused provider/chat suites pass.