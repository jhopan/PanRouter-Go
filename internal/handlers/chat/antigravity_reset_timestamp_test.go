package chat

import (
	"net/http"
	"strconv"
	"testing"
	"time"
)

// antigravityResetOnlyBody is the detail-only 429 Antigravity's generation
// endpoint answers with: the precise reset lives in a google.rpc ErrorInfo
// detail as an ISO instant (quotaResetTimeStamp), and there is NO "Resets in …"
// prose and no RetryInfo delay beside it. Upstream's executor parses exactly
// this shape (open-sse/executors/antigravity.js parseError); the detail-free
// variant is what its own comments describe Google answering with.
func antigravityResetOnlyBody(status int, resetAt time.Time) string {
	return `{"error":{"code":` + strconv.Itoa(status) + `,"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"QUOTA_EXHAUSTED","domain":"cloudcode-pa.googleapis.com","metadata":{"model":"gemini-3.8-flash-tiered","quotaResetTimeStamp":"` +
		resetAt.UTC().Format(time.RFC3339) + `"}}]}}`
}

// antigravityExhaustedBody is the full captured 429: the same ErrorInfo detail
// plus the human-readable quotaResetDelay and the "Resets in …" sentence inside
// the message.
func antigravityExhaustedBody(resetAt time.Time, delay string) string {
	return `{"error":{"code":429,"message":"Individual quota reached. Please upgrade your subscription to increase your limits. Resets in ` +
		delay + `.","status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"QUOTA_EXHAUSTED","domain":"cloudcode-pa.googleapis.com","metadata":{"model":"gemini-3.8-flash-tiered","quotaResetDelay":"` +
		delay + `","quotaResetTimeStamp":"` + resetAt.UTC().Format(time.RFC3339) + `"}}]}}`
}

// TestQuotaResetTimestamp_ISOInstant is the carrier the executor's parseError
// reads and this router did not: details[].metadata.quotaResetTimeStamp.
//
// Without it a body whose only reset instruction is the ISO instant produced no
// cooldown at all — retryableCooldownSec fell back to the classifier's
// two-second base and the next request re-picked the exhausted account, which
// is the retry that deepens Google's backoff.
func TestQuotaResetTimestamp_ISOInstant(t *testing.T) {
	const want = 90 * time.Minute
	body := []byte(antigravityResetOnlyBody(http.StatusTooManyRequests, time.Now().Add(want)))

	got, ok := quotaResetTimestamp(body)
	if !ok {
		t.Fatalf("no reset read out of %s", body)
	}
	if got < want-time.Minute || got > want+time.Minute {
		t.Errorf("quotaResetTimestamp = %v, want ~%v", got, want)
	}

	ue := &upstreamError{StatusCode: http.StatusTooManyRequests, Body: body}
	wait, ok := retryAfterWait(ue)
	if !ok {
		t.Fatal("retryAfterWait found no wait in a body that names the reset instant")
	}
	if wait < want-time.Minute || wait > want+time.Minute {
		t.Errorf("retryAfterWait = %v, want ~%v", wait, want)
	}

	// The account cooldown honours it instead of the two-second burst base.
	if secs := retryableCooldownSec(http.StatusTooManyRequests, 2*time.Second, ue, "antigravity"); secs < 3600 {
		t.Errorf("cooldown = %ds, want the announced window (~5400s) rather than the burst base", secs)
	}
}

// TestQuotaResetTimestamp_UnwrappedAndNested covers the two envelopes the
// executor tolerates: Google's own `error.details` and a relay that surfaces
// the detail list unwrapped at the top level.
func TestQuotaResetTimestamp_UnwrappedAndNested(t *testing.T) {
	reset := time.Now().Add(45 * time.Minute).UTC().Format(time.RFC3339)
	tests := []struct {
		name string
		body string
	}{
		{
			name: "nested under error",
			body: `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","details":[{"metadata":{"quotaResetTimeStamp":"` + reset + `"}}]}}`,
		},
		{
			name: "unwrapped at the top level",
			body: `{"code":429,"details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","metadata":{"quotaResetTimeStamp":"` + reset + `"}}]}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := quotaResetTimestamp([]byte(tt.body))
			if !ok {
				t.Fatalf("no reset read out of %s", tt.body)
			}
			if got < 44*time.Minute || got > 45*time.Minute {
				t.Errorf("quotaResetTimestamp = %v, want ~45m", got)
			}
		})
	}
}

// TestQuotaResetTimestamp_PastInstantIsNotAWait: a stamp in the past names no
// future window, so it must not become a five-second floor either. Upstream
// skips it and falls through to the message patterns; a body that carries a
// past stamp plus a live "Resets in" must answer with the live one.
func TestQuotaResetTimestamp_PastInstantIsNotAWait(t *testing.T) {
	past := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	body := []byte(`{"error":{"code":429,"message":"Resource exhausted. Resets in 4m.","status":"RESOURCE_EXHAUSTED","details":[{"metadata":{"quotaResetTimeStamp":"` + past + `"}}]}}`)

	if got, ok := quotaResetTimestamp(body); ok {
		t.Errorf("quotaResetTimestamp = %v, want no wait from a past instant", got)
	}
	got, ok := retryAfterWait(&upstreamError{StatusCode: http.StatusTooManyRequests, Body: body})
	if !ok {
		t.Fatal("the message pattern was not reached")
	}
	if got < 4*time.Minute-time.Second || got > 4*time.Minute+time.Second {
		t.Errorf("retryAfterWait = %v, want the 4m the message names", got)
	}
}

// TestQuotaResetTimestamp_LongestCarrierWins: the instant and the RetryInfo
// delay often disagree (the instant is the real window, the delay a rounded
// echo). A wait is only over when every source's window has passed, so the
// longer one is the answer.
func TestQuotaResetTimestamp_LongestCarrierWins(t *testing.T) {
	body := []byte(`{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","metadata":{"quotaResetTimeStamp":"` +
		time.Now().Add(3*time.Hour).UTC().Format(time.RFC3339) +
		`"}},{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"120s"}]}}`)
	ue := &upstreamError{StatusCode: http.StatusTooManyRequests, Body: body}

	got, ok := retryAfterWait(ue)
	if !ok {
		t.Fatal("no wait found")
	}
	if got < 2*time.Hour+59*time.Minute {
		t.Errorf("retryAfterWait = %v, want the 3h instant rather than the 120s echo", got)
	}
	// The announced window is still clamped by the provider ceiling so a
	// nonsensical body cannot park an account forever.
	if secs := retryableCooldownSec(http.StatusTooManyRequests, 2*time.Second, ue, "antigravity"); secs != int(maxResetCooldown/time.Second) {
		t.Errorf("cooldown = %ds, want the %v ceiling", secs, maxResetCooldown)
	}
}

// TestQuotaResetTimestamp_ConflictStatus: Antigravity signals pool exhaustion
// with 409 as well as 429, and the executor parses the reset out of both. The
// carrier reader is status-agnostic, so a 409 body must answer the same way.
func TestQuotaResetTimestamp_ConflictStatus(t *testing.T) {
	body := []byte(antigravityResetOnlyBody(http.StatusConflict, time.Now().Add(75*time.Minute)))
	got, ok := retryAfterWait(&upstreamError{StatusCode: http.StatusConflict, Body: body})
	if !ok {
		t.Fatal("no wait found in a 409 that names the reset instant")
	}
	if got < 74*time.Minute || got > 75*time.Minute {
		t.Errorf("retryAfterWait = %v, want ~75m", got)
	}
}

// TestRateLimitResetHeaders: the executor's parseRetryHeaders reads
// x-ratelimit-reset-after (delta seconds) and x-ratelimit-reset (epoch seconds)
// beside Retry-After. A response naming its window nowhere else left the account
// on the burst backoff.
func TestRateLimitResetHeaders(t *testing.T) {
	tests := []struct {
		name   string
		header http.Header
		want   time.Duration
		wantOk bool
	}{
		{name: "delta seconds", header: http.Header{"X-Ratelimit-Reset-After": {"45"}}, want: 45 * time.Second, wantOk: true},
		{name: "epoch seconds", header: http.Header{"X-Ratelimit-Reset": {strconv.FormatInt(time.Now().Add(2 * time.Minute).Unix(), 10)}}, want: 2 * time.Minute, wantOk: true},
		{name: "epoch already past", header: http.Header{"X-Ratelimit-Reset": {"1"}}, wantOk: false},
		{name: "nonsense", header: http.Header{"X-Ratelimit-Reset-After": {"soon"}}, wantOk: false},
		{name: "absent", header: http.Header{}, wantOk: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseRateLimitResetHeaders(tt.header)
			if ok != tt.wantOk {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOk)
			}
			if !ok {
				return
			}
			if diff := got - tt.want; diff > 2*time.Second || diff < -2*time.Second {
				t.Errorf("wait = %v, want ~%v", got, tt.want)
			}
		})
	}

	// The header alone is enough for the whole cooldown path.
	ue := &upstreamError{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"X-Ratelimit-Reset-After": {"120"}},
	}
	if secs := retryableCooldownSec(http.StatusTooManyRequests, 2*time.Second, ue, "antigravity"); secs != 120 {
		t.Errorf("cooldown = %ds, want the 120s the header announced", secs)
	}
}

// TestAntigravityExhaustedBodyReadsEveryCarrier pins the captured full body:
// message prose, quotaResetDelay and the ISO instant all name the same window,
// and the router must agree with all three (the ISO instant is the one the
// executor prefers, so a disagreement must not shorten the wait).
func TestAntigravityExhaustedBodyReadsEveryCarrier(t *testing.T) {
	const want = 90 * time.Minute
	body := []byte(antigravityExhaustedBody(time.Now().Add(want), "1h30m0s"))

	got, ok := retryAfterWait(&upstreamError{StatusCode: http.StatusTooManyRequests, Body: body})
	if !ok {
		t.Fatal("no wait found in the captured body")
	}
	if got < want-time.Minute || got > want+time.Minute {
		t.Errorf("retryAfterWait = %v, want ~%v", got, want)
	}
}
