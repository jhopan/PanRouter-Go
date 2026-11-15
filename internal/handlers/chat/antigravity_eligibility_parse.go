// Antigravity eligibility parsing — the two upstream gates that decide whether
// Google considers an account eligible to serve Antigravity at all:
//
//  1. loadCodeAssist's `ineligibleTiers` (upstream parseAntigravityEligibility)
//  2. the quota endpoint's 403 `error.details` (upstream
//     parseAntigravityQuotaEligibility)
//
// Each returns nil when the payload says nothing about eligibility, so a caller
// cannot mistake "no fields" for "eligible".
package chat

import (
	"strings"

	json "panrouter/proxy/internal/fastjson"
	"panrouter/proxy/internal/providers"
)

// ParseAntigravityEligibility reads the eligibility gate out of a
// loadCodeAssist response.
func ParseAntigravityEligibility(payload map[string]any) *AntigravityEligibility {
	if payload == nil {
		return nil
	}
	tiers := antigravityMembers(payload["ineligibleTiers"])
	if len(tiers) == 0 {
		// No ineligible tiers → the account is fine, unless it also has no
		// allowed tier (an account with neither is not usable).
		if len(antigravityMembers(payload["allowedTiers"])) > 0 {
			return &AntigravityEligibility{Eligible: true}
		}
		return nil
	}

	// Prefer the tier that names a validation reason; the first tier is not
	// necessarily the blocking one.
	tier := tiers[0]
	for _, t := range tiers {
		if antigravityStr(t["reasonCode"]) != "" || antigravityStr(t["validationUrl"]) != "" || antigravityStr(t["reasonMessage"]) != "" {
			tier = t
			break
		}
	}
	reason := antigravityStr(tier["reasonMessage"])
	if reason == "" {
		reason = antigravityStr(tier["validationErrorMessage"])
	}
	return &AntigravityEligibility{
		ReasonCode:    antigravityStr(tier["reasonCode"]),
		ReasonMessage: reason,
		ValidationURL: antigravityStr(tier["validationUrl"]),
		TierID:        antigravityStr(tier["tierId"]),
	}
}

// ParseAntigravityQuotaEligibility reads the gate out of a quota endpoint 403:
// {"error":{"code":403,"status":"PERMISSION_DENIED","details":[{"reason":…,
// "metadata":{"validation_url":…}},{"links":[{"url":…}]}]}}. The ErrorInfo
// detail carries the reason and the metadata, the Help detail only the link —
// either alone is enough to act on. Returns nil for a plain 403.
func ParseAntigravityQuotaEligibility(payload map[string]any) *AntigravityEligibility {
	if payload == nil {
		return nil
	}
	errObj, _ := payload["error"].(map[string]any)
	if errObj == nil {
		return nil
	}
	details := antigravityMembers(errObj["details"])
	var info map[string]any
	for _, d := range details {
		meta, _ := d["metadata"].(map[string]any)
		if antigravityStr(d["reason"]) != "" || antigravityStr(meta["validation_url"]) != "" {
			info = d
			break
		}
	}
	helpURL := ""
	for _, d := range details {
		for _, l := range antigravityMembers(d["links"]) {
			if url := antigravityStr(l["url"]); strings.Contains(url, "accounts.google.com") {
				helpURL = url
				break
			}
		}
		if helpURL != "" {
			break
		}
	}
	if info == nil && helpURL == "" {
		return nil
	}

	meta, _ := info["metadata"].(map[string]any)
	validationURL := antigravityStr(meta["validation_url"])
	if validationURL == "" {
		validationURL = helpURL
	}
	reason := antigravityStr(meta["validation_error_message"])
	if reason == "" {
		reason = antigravityStr(errObj["message"])
	}
	return &AntigravityEligibility{
		ReasonCode:    antigravityStr(info["reason"]),
		ReasonMessage: reason,
		ValidationURL: validationURL,
	}
}

// AntigravityVerdictFromErrorBody builds the verdict for a body that refused an
// account. The quota-403 details decide when they are there; the appeal link the
// ban detector validates fills in the verification URL when the body carries it
// somewhere else, so the operator has the one link Google wants opened. Returns
// nil when the body establishes nothing.
func AntigravityVerdictFromErrorBody(body []byte) *AntigravityEligibility {
	if len(body) == 0 {
		return nil
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}
	verdict := ParseAntigravityQuotaEligibility(payload)
	if verdict == nil {
		return nil
	}
	if verdict.ValidationURL == "" {
		if appeal, ok := providers.ExtractAppealURL(body); ok {
			verdict.ValidationURL = appeal
		}
	}
	return verdict
}

// antigravityMembers reads a JSON array of objects out of a decoded payload.
func antigravityMembers(v any) []map[string]any {
	list, _ := v.([]any)
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// antigravityStr reads a string field, treating anything else as absent.
func antigravityStr(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}
