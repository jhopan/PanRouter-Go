// Kiro upload parsing — parity with upstream open-sse/services/usage/kiro.js
// (parseKiroQuotaData) and tests/unit/kiro-reset-visibility.test.js.
//
// Two contracts the tracker depends on:
//   - the monthly window is ALWAYS a row. Without it the card goes blank exactly
//     when the account is out of quota, instead of saying "exhausted, resets
//     <date>".
//   - limitReached is reported, so the row can be labelled exhausted rather than
//     left as an unknown reading that looks like plenty of quota left.
package dashboard

import (
	"testing"
)

func TestParseKiroQuotaDataSynthesizesMonthlyRow(t *testing.T) {
	// No breakdown at all: the account is simply out. The synthesized row is what
	// stops the dashboard rendering an empty table.
	res := parseKiroQuotaData(map[string]any{
		"nextDateReset": "2026-12-01T00:00:00.000Z",
	})
	monthly, ok := res.quotas["monthly"].(map[string]any)
	if !ok {
		t.Fatalf("monthly row missing from %v", res.quotas)
	}
	if used, _ := monthly["used"].(float64); used != 0 {
		t.Errorf("monthly used = %v, want 0 when no bucket reported exhaustion", monthly["used"])
	}
	if remaining, _ := monthly["remaining"].(float64); remaining != 1 {
		t.Errorf("monthly remaining = %v, want 1", monthly["remaining"])
	}
	if res.extra["limitReached"] != false {
		t.Errorf("limitReached = %v, want false", res.extra["limitReached"])
	}
	if got, _ := res.extra["resetAt"].(string); got != "2026-12-01T00:00:00Z" {
		t.Errorf("resetAt = %q, want the account-level nextDateReset", got)
	}
}

func TestParseKiroQuotaDataReportsAnExhaustedBucket(t *testing.T) {
	res := parseKiroQuotaData(map[string]any{
		"usageBreakdownList": []any{
			map[string]any{
				"resourceType":              "AGENTIC_REQUEST",
				"currentUsageWithPrecision": 40.0,
				"usageLimitWithPrecision":   40.0,
			},
			map[string]any{
				"resourceType":              "CHAT",
				"currentUsageWithPrecision": 3.0,
				"usageLimitWithPrecision":   100.0,
			},
		},
		"nextDateReset": "2026-12-01T00:00:00.000Z",
	})

	if res.extra["limitReached"] != true {
		t.Errorf("limitReached = %v, want true when one bucket is used up", res.extra["limitReached"])
	}
	agentic, ok := res.quotas["agentic_request"].(map[string]any)
	if !ok {
		t.Fatalf("agentic_request row missing from %v", res.quotas)
	}
	if used, _ := agentic["used"].(float64); used != 40 {
		t.Errorf("agentic used = %v, want 40", agentic["used"])
	}
	if remaining, _ := agentic["remaining"].(float64); remaining != 0 {
		t.Errorf("agentic remaining = %v, want 0", agentic["remaining"])
	}
	// A breakdown of agentic_request/chat buckets still leaves no bucket named
	// "monthly", so the window row is synthesized on top — with the exhausted flag
	// carried into it, which is what the tracker labels as the account's cap.
	monthly, hasMonthly := res.quotas["monthly"].(map[string]any)
	if !hasMonthly {
		t.Fatalf("monthly row missing from %v", res.quotas)
	}
	if used, _ := monthly["used"].(float64); used != 1 {
		t.Errorf("monthly used = %v, want 1 for an exhausted account", monthly["used"])
	}
	if remaining, _ := monthly["remaining"].(float64); remaining != 0 {
		t.Errorf("monthly remaining = %v, want 0", monthly["remaining"])
	}
}

func TestParseKiroQuotaDataFreeTrialRow(t *testing.T) {
	res := parseKiroQuotaData(map[string]any{
		"usageBreakdownList": []any{
			map[string]any{
				"resourceType":              "AGENTIC_REQUEST",
				"currentUsageWithPrecision": 1.0,
				"usageLimitWithPrecision":   50.0,
				"freeTrialInfo": map[string]any{
					"currentUsageWithPrecision": 2.0,
					"usageLimitWithPrecision":   10.0,
					"freeTrialExpiry":           "2026-11-20T00:00:00.000Z",
				},
			},
		},
	})
	trial, ok := res.quotas["agentic_request_freetrial"].(map[string]any)
	if !ok {
		t.Fatalf("free-trial row missing from %v", res.quotas)
	}
	if remaining, _ := trial["remaining"].(float64); remaining != 8 {
		t.Errorf("free-trial remaining = %v, want 8", trial["remaining"])
	}
	if got, _ := trial["resetAt"].(string); got != "2026-11-20T00:00:00Z" {
		t.Errorf("free-trial resetAt = %q, want freeTrialExpiry", got)
	}
}
