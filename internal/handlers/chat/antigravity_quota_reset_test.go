package chat

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// weeklyBucketSummary is a retrieveUserQuotaSummary payload with one Gemini
// weekly bucket whose resetTime is the raw JSON literal under test.
func weeklyBucketSummary(resetTimeJSON string) []byte {
	return []byte(`{"groups":[{"displayName":"Gemini Models","buckets":[` +
		`{"bucketId":"gemini-weekly","displayName":"Weekly","window":"weekly",` +
		`"disabled":false,"remainingFraction":0.5,"resetTime":` + resetTimeJSON + `}]}]}`)
}

// TestParseWeeklyQuotaSummary_ResetInstantShapes ports parseResetTime
// (open-sse/services/usage/shared.js:15-42), the reader upstream feeds every
// Antigravity reset value through (google.js:79 family buckets, google.js:246
// per-model rows). It accepts an epoch in seconds, an epoch in milliseconds
// (as a JSON number or an all-digit string) and an RFC3339 instant; anything
// else must yield no reset rather than an invented one.
//
// Fails before the change: only time.RFC3339 was parsed, so every epoch shape
// left ResetAt zero (and a numeric JSON value failed the whole decode).
func TestParseWeeklyQuotaSummary_ResetInstantShapes(t *testing.T) {
	const epochSeconds = 1794000000
	wantEpoch := time.Unix(epochSeconds, 0).UTC()

	tests := []struct {
		name     string
		reset    string
		want     time.Time
		wantZero bool
	}{
		{name: "RFC3339", reset: `"2026-11-20T10:00:00Z"`, want: time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)},
		{name: "RFC3339 with fraction", reset: `"2026-11-20T10:00:00.500Z"`, want: time.Date(2026, 11, 20, 10, 0, 0, 500e6, time.UTC)},
		{name: "epoch seconds as number", reset: `1794000000`, want: wantEpoch},
		{name: "epoch milliseconds as number", reset: `1794000000000`, want: wantEpoch},
		{name: "epoch seconds as string", reset: `"1794000000"`, want: wantEpoch},
		{name: "epoch milliseconds as string", reset: `"1794000000000"`, want: wantEpoch},
		{name: "garbage string", reset: `"not-a-time"`, wantZero: true},
		{name: "prose", reset: `"soon"`, wantZero: true},
		{name: "null", reset: `null`, wantZero: true},
		{name: "empty string", reset: `""`, wantZero: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed := ParseWeeklyQuotaSummary(weeklyBucketSummary(tt.reset))
			bucket, ok := parsed["gemini_weekly"]
			if !ok {
				t.Fatalf("the bucket was dropped; a %s resetTime must not fail the decode: %v", tt.name, parsed)
			}
			if tt.wantZero {
				if !bucket.ResetAt.IsZero() {
					t.Errorf("ResetAt = %v, want no reset for %s", bucket.ResetAt, tt.reset)
				}
				return
			}
			if !bucket.ResetAt.Equal(tt.want) {
				t.Errorf("ResetAt = %v, want %v", bucket.ResetAt, tt.want)
			}
		})
	}
}

// TestRefreshAntigravityQuota_EpochResetTime pins the same acceptance set on the
// per-model rows: a numeric quotaInfo.resetTime must not fail the whole refresh
// (a string-typed field made the decode error out, so the account lost its
// entire quota reading).
func TestRefreshAntigravityQuota_EpochResetTime(t *testing.T) {
	ClearAntigravityQuotaCache()
	const epochSeconds = 1794000000

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":{"gemini-3.8-flash-high":{"isInternal":false,` +
			`"quotaInfo":{"remainingFraction":0.5,"resetTime":1794000000}}}}`))
	}))
	defer server.Close()

	oldBase := antigravityQuotaBaseURL
	antigravityQuotaBaseURL = server.URL
	defer func() { antigravityQuotaBaseURL = oldBase }()

	quotas, err := RefreshAntigravityQuota(context.Background(), server.Client(), "ag-epoch", "tok", "proj")
	if err != nil {
		t.Fatalf("RefreshAntigravityQuota: %v", err)
	}
	got, ok := quotas["gemini-3.8-flash-high"]
	if !ok {
		t.Fatalf("the numeric resetTime dropped the model row: %v", quotas)
	}
	if want := time.Unix(epochSeconds, 0).UTC(); !got.ResetAt.Equal(want) {
		t.Errorf("ResetAt = %v, want %v", got.ResetAt, want)
	}
}

// TestHandleAntigravityQuotaError_ParksOnTheBodyResetInstant pins the park
// order upstream uses: the 409/429 body names the bucket's exact end in
// details[].metadata.quotaResetTimeStamp, and chat.js only falls back to the
// live quota API when the body named nothing
// (open-sse/executors/antigravity.js:455-503 parseError;
// src/sse/handlers/chat.js:443 `... && !resetsAtMs`).
//
// Fails before the change: the handler always refreshed live quota, so it
// returned that reading's generic window instead of the body's instant, and it
// spent a quota call the body had already made unnecessary.
func TestHandleAntigravityQuotaError_ParksOnTheBodyResetInstant(t *testing.T) {
	for _, status := range []int{http.StatusConflict, http.StatusTooManyRequests} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			ClearAntigravityQuotaCache()

			// The live rows deliberately name a DIFFERENT (generic) reset, so a
			// handler that consults them cannot accidentally pass this test.
			var hits atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				hits.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"models":{"gemini-3.8-flash-high":{"isInternal":false,` +
					`"quotaInfo":{"remainingFraction":0,"resetTime":"` +
					time.Now().UTC().Add(2*time.Hour).Format(time.RFC3339) + `"}}}}`))
			}))
			defer server.Close()

			oldBase := antigravityQuotaBaseURL
			antigravityQuotaBaseURL = server.URL
			defer func() { antigravityQuotaBaseURL = oldBase }()

			const connID, model = "ag-body-instant", "gemini-3.8-flash-high"
			want := time.Now().UTC().Add(75 * time.Minute)
			got := HandleAntigravityQuotaError(AntigravityQuotaError{
				Ctx: context.Background(), Client: server.Client(), ConnectionID: connID,
				Status: status, Model: model, AccessToken: "tok", ProjectID: "proj",
				ErrorMessage: antigravityResetOnlyBody(status, want),
			})

			if got == nil {
				t.Fatal("no park deadline from a body that names one")
			}
			if diff := got.Sub(want); diff < -time.Second || diff > time.Second {
				t.Errorf("park until %v, want the body's %v (+/-1s)", got, want)
			}
			if n := hits.Load(); n != 0 {
				t.Errorf("the live quota API was called %d time(s); the body's instant must short-circuit it", n)
			}
			// The deadline has to reach the pre-filter as a block, not just the
			// return value: the account sits out the bucket's own window.
			if !IsAntigravityModelBlocked(connID, model) {
				t.Error("the model is not blocked after the park")
			}
			cached, ok := antigravityQuotaCacheSnapshot(connID)[model]
			if !ok || cached.RemainingPercentage != 0 {
				t.Fatalf("cache row = %+v, want a 0%% block", cached)
			}
			if diff := cached.ResetAt.Sub(want); diff < -time.Second || diff > time.Second {
				t.Errorf("cached resetAt = %v, want the body's %v", cached.ResetAt, want)
			}
		})
	}
}

// TestHandleAntigravityQuotaError_ParksOnTheDelayTheBodyNames keeps the park
// path off the cooldown's two-hour clamp: a body that states the wait as
// quotaResetDelay ("74h19m34s") names the bucket's own end, and upstream's
// parseError reads that same metadata field into resetsAtMs
// (open-sse/executors/antigravity.js:479-483). Parking two hours instead of
// three days is what puts the account back into rotation while it is still
// empty.
func TestHandleAntigravityQuotaError_ParksOnTheDelayTheBodyNames(t *testing.T) {
	ClearAntigravityQuotaCache()

	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"models":{}}`))
	}))
	defer server.Close()

	oldBase := antigravityQuotaBaseURL
	antigravityQuotaBaseURL = server.URL
	defer func() { antigravityQuotaBaseURL = oldBase }()

	const connID, model = "ag-delay-instant", "gemini-3.8-flash-high"
	body := `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo",` +
		`"reason":"QUOTA_EXHAUSTED","metadata":{"quotaResetDelay":"74h19m34s"}}]}}`

	got := HandleAntigravityQuotaError(AntigravityQuotaError{
		Ctx: context.Background(), Client: server.Client(), ConnectionID: connID,
		Status: http.StatusTooManyRequests, Model: model, AccessToken: "tok", ProjectID: "proj",
		ErrorMessage: body,
	})
	if got == nil {
		t.Fatal("no park deadline from a body that names the delay")
	}
	want := time.Now().UTC().Add(74*time.Hour + 19*time.Minute + 34*time.Second)
	if diff := got.Sub(want); diff < -time.Minute || diff > time.Minute {
		t.Errorf("park until %v, want the body's %v (not the two-hour cooldown clamp)", got, want)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("the live quota API was called %d time(s); the body's delay must short-circuit it", n)
	}
}

// TestHandleAntigravityQuotaError_NonQuotaStatusHasNoPark keeps the body reader
// scoped: a 500 that happens to carry a reset stamp is not a quota verdict.
func TestHandleAntigravityQuotaError_NonQuotaStatusHasNoPark(t *testing.T) {
	ClearAntigravityQuotaCache()
	if got := HandleAntigravityQuotaError(AntigravityQuotaError{
		Ctx: context.Background(), ConnectionID: "ag-500", Status: http.StatusInternalServerError,
		Model: "gemini-3.8-flash-high", ErrorMessage: antigravityResetOnlyBody(http.StatusInternalServerError, time.Now().Add(time.Hour)),
	}); got != nil {
		t.Errorf("HandleAntigravityQuotaError(500) = %v, want nil", got)
	}
}
