// Antigravity in-place retry — port of computeRetryDelay in
// open-sse/executors/antigravity.js.
//
// Google's agent lane answers 429/5xx and a family of 400s whose body says
// "high traffic" or "agent terminated due to error". Upstream retries the SAME
// call in place while the wait is short, instead of parking the account: on a
// single-account deployment the alternative is failing the client's turn for a
// fault that clears in a couple of seconds.
//
// The budgets are the reference's: a provider-named wait is honoured only up to
// MAX_RETRY_AFTER_MS, anything longer is a veto (the caller rotates instead), and a
// transient fault without a named wait backs off exponentially within
// 10s (429) / 15s (5xx).
package proxy

import (
	"net/http"
	json "panrouter/proxy/internal/fastjson"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// antigravityMaxRetryAfter caps a provider-named wait. Longer means the lane is
	// out for a while and the account should rotate instead of holding the turn.
	antigravityMaxRetryAfter = 10 * time.Second
	// antigravityTransientRetryMax caps the exponential backoff for a 5xx.
	antigravityTransientRetryMax = 15 * time.Second
	// antigravityMaxTransientRetries bounds how many times one call is repeated in
	// place. Each attempt costs a full request to a lane that bans accounts over
	// repeated request patterns, so the count stays small.
	antigravityMaxTransientRetries = 2
)

// antigravityTransientPatterns is the reference's ANTIGRAVITY_TRANSIENT_ERROR_PATTERNS.
var antigravityTransientPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)high\s+traffic`),
	regexp.MustCompile(`(?i)agent\s+(execution\s+)?terminated\s+due\s+to\s+error`),
	regexp.MustCompile(`(?i)capacity`),
	regexp.MustCompile(`(?i)temporarily\s+unavailable`),
	regexp.MustCompile(`(?i)timeout`),
	regexp.MustCompile(`(?i)stream\s+(ended|closed|terminated|interrupted)`),
	regexp.MustCompile(`(?i)empty\s+response`),
}

// antigravityResetsInPattern is parseRetryFromErrorMessage's shape: "resets in
// 1h2m3s", with every part optional.
var antigravityResetsInPattern = regexp.MustCompile(`(?i)resets?\s+(?:in|after)\s+((?:\d+(?:\.\d+)?h)?)((?:\d+(?:\.\d+)?m)?)((?:\d+(?:\.\d+)?s)?)`)

// antigravityStripStreamOptions removes stream_options from a non-streaming
// request. An OpenAI client may send it even when it asked for no stream, and
// Google generateContent rejects that combination before processing the request
// (open-sse/executors/antigravity.js transformRequest: `if (stream !== true)
// delete body.stream_options`). A streaming request keeps it.
func antigravityStripStreamOptions(body []byte) []byte {
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return body
	}
	if _, present := parsed["stream_options"]; !present {
		return body
	}
	delete(parsed, "stream_options")
	encoded, err := json.Marshal(parsed)
	if err != nil {
		return body
	}
	return encoded
}

// antigravityRetryableStatus reports whether the status is worth inspecting for a
// retry at all. The body is read only for these, so a normal answer pays nothing.
func antigravityRetryableStatus(status int) bool {
	return status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
}

// antigravityIsTransient reports whether the fault clears on its own. A 429 is
// always transient; so are the 5xx family and the body patterns above.
func antigravityIsTransient(status int, message string) bool {
	if status == http.StatusTooManyRequests {
		return true
	}
	switch status {
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	for _, pattern := range antigravityTransientPatterns {
		if pattern.MatchString(message) {
			return true
		}
	}
	return false
}

// antigravityRetryDelay returns how long to wait before repeating the call, or 0
// when it must not be repeated (the caller rotates or surfaces the error).
//
// attempt is 1-based, matching the reference: the first repeat doubles the 1s base.
func antigravityRetryDelay(status int, header http.Header, body string, attempt int) time.Duration {
	if wait, ok := antigravityHeaderRetryDelay(header); ok {
		if wait <= antigravityMaxRetryAfter {
			return wait
		}
		return 0
	}
	if wait, ok := antigravityMessageRetryDelay(body); ok {
		if wait <= antigravityMaxRetryAfter {
			return wait
		}
		return 0
	}
	if !antigravityIsTransient(status, body) {
		return 0
	}
	cap := antigravityTransientRetryMax
	if status == http.StatusTooManyRequests {
		cap = antigravityMaxRetryAfter
	}
	delay := time.Duration(1000*(1<<attempt)) * time.Millisecond
	if delay > cap {
		delay = cap
	}
	return delay
}

// antigravityHeaderRetryDelay reads what the response headers name: Retry-After
// (seconds or an HTTP date), then the rate-limit reset headers.
func antigravityHeaderRetryDelay(header http.Header) (time.Duration, bool) {
	if header == nil {
		return 0, false
	}
	if raw := strings.TrimSpace(header.Get("Retry-After")); raw != "" {
		if seconds, err := strconv.Atoi(raw); err == nil && seconds > 0 {
			return time.Duration(seconds) * time.Second, true
		}
		if when, err := http.ParseTime(raw); err == nil {
			if wait := time.Until(when); wait > 0 {
				return wait, true
			}
		}
	}
	if raw := strings.TrimSpace(header.Get("X-Ratelimit-Reset-After")); raw != "" {
		if seconds, err := strconv.Atoi(raw); err == nil && seconds > 0 {
			return time.Duration(seconds) * time.Second, true
		}
	}
	if raw := strings.TrimSpace(header.Get("X-Ratelimit-Reset")); raw != "" {
		if stamp, err := strconv.ParseInt(raw, 10, 64); err == nil {
			if wait := time.Until(time.Unix(stamp, 0)); wait > 0 {
				return wait, true
			}
		}
	}
	return 0, false
}

// antigravityMessageRetryDelay reads "resets in 1h2m3s" out of an error body.
func antigravityMessageRetryDelay(body string) (time.Duration, bool) {
	match := antigravityResetsInPattern.FindStringSubmatch(body)
	if match == nil {
		return 0, false
	}
	total := time.Duration(0)
	parts := []struct {
		value   string
		perUnit time.Duration
	}{
		{match[1], time.Hour},
		{match[2], time.Minute},
		{match[3], time.Second},
	}
	for _, part := range parts {
		if part.value == "" {
			continue
		}
		number, err := strconv.ParseFloat(strings.TrimSuffix(part.value, string(part.value[len(part.value)-1])), 64)
		if err != nil {
			continue
		}
		total += time.Duration(number * float64(part.perUnit))
	}
	if total <= 0 {
		return 0, false
	}
	return total, true
}
