package chat

import (
	json "encoding/json/v2"
	"math/rand/v2"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"panrouter/proxy/internal/constants"
	"panrouter/proxy/internal/errclass"
	"panrouter/proxy/internal/log"
	"panrouter/proxy/internal/providers"
	"panrouter/proxy/internal/translator"
)

const (
	minRetryJitterMs = 100
	maxRetryJitterMs = 400
)

// addRetryJitter adds a random jitter between minRetryJitterMs and maxRetryJitterMs
// to a positive duration to prevent thundering-herd collisions on retry.
func addRetryJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	delta := maxRetryJitterMs - minRetryJitterMs
	var jitter int64
	if delta > 0 {
		jitter = minRetryJitterMs + rand.Int64N(int64(delta))
	} else {
		jitter = minRetryJitterMs
	}
	return d + time.Duration(jitter)*time.Millisecond
}

// brief429RetryTolerance is the longest wait a rate-limited upstream may ask
// for and still be worth repeating on the same client turn. It is the router's
// own base backoff (providers.BackoffConfig.BaseMs): at or under it the account
// is very likely free again by the time the next attempt runs, so an ordinary
// burst is absorbed instead of surfacing a 429. Above it the quota window is
// spent, and re-sending to the same account only deepens Google's backoff
// instead of letting the next account serve the turn.
const brief429RetryTolerance = 2 * time.Second

// comboRetryWaitCap bounds how long a fully-failed combo pass will hold the
// request before retrying. Longer upstream Retry-After values are surfaced via
// the Retry-After header instead so the client decides. A 429 is held to the
// stricter brief429RetryTolerance: unlike a transient 5xx, a long Retry-After
// on a 429 names a quota window that waiting out here cannot shorten.
const comboRetryWaitCap = 8 * time.Second

// deepSeekWebMaxMuteCooldown is the ceiling for a DeepSeek web mute the site
// announced. Its mute envelope names a real release time, and the generic 2h
// cap would re-pick the muted account long before the mute ends — which is how
// a brief mute becomes a longer one.
const deepSeekWebMaxMuteCooldown = 7 * 24 * time.Hour

// quotaScope is the calendar cadence a provider's free tier refills on, ported
// from upstream QUOTA_SCOPES (open-sse/config/quotaWindows.js). The declared
// scope decides two things: the ceiling an announced reset is bounded by, and
// the calendar a refusal parks on when its body names no reset of its own.
type quotaScope int

const (
	// scopeMonthly is a calendar-month window (CodeBuddy's subscription cycle).
	scopeMonthly quotaScope = iota
	// scopeWeekly is a rolling weekly window: Token Harbor's free allowance
	// refills at the next weekly rollover (upstream
	// `tokenharbor: { scope: QUOTA_SCOPES.WEEKLY }`).
	scopeWeekly
	// scopeDaily is a calendar day: Agnes AI's free text quota refills at 00:00 UTC
	// (upstream QUOTA_SCOPES.DAILY with resetUtcHour 0).
	scopeDaily
	// scopeSession is a short rolling session window (upstream
	// QUOTA_SCOPES.SESSION, a 5h fallback when the provider names no reset).
	// MiniMax Code's M Plan and Codex/Claude's 5h windows use it. The ceiling is the
	// generous one upstream applies to every scope (40 days), which is what lets a
	// provider pin a flat 24h block without it being clamped to the generic two
	// hours.
	scopeSession
)

// quotaWindowScopes are the providers whose quota window this router knows.
// Both spellings of the CodeBuddy pair are listed because a connection row may
// store either; resolving the provider's alias covers the rest (`cbai` →
// codebuddy-intl, `th`/`thh` → tokenharbor).
var quotaWindowScopes = map[string]quotaScope{
	"codebuddy-intl": scopeMonthly,
	"codebuddy-cn":   scopeMonthly,
	"cbai":           scopeMonthly,
	"cbcn":           scopeMonthly,
	"tokenharbor":    scopeWeekly,
	// Agnes AI's free tier counts text requests per calendar day, and its reset is
	// the next 00:00 UTC.
	"agnes": scopeDaily,
	// Cloudflare Workers AI meters a free allocation per calendar day, and its
	// 429 states the daily cap rather than naming an instant. Upstream leaves the
	// provider out of quotaWindows.js on purpose and relies on the resolver's
	// `default: { scope: DAILY, resetUtcHour: 0 }`, so an undeclared provider parks
	// on the UTC day; declaring it here is the same answer without widening that
	// default to every other lane (a burned budget elsewhere may be a burst).
	"cloudflare-ai": scopeDaily,
	// MiniMax Code (both regions) reports a session-scoped plan window
	// (quotaWindows.js:46-47) and its exhaustion rule pins a flat 24h.
	"minimax-code":        scopeSession,
	"minimax-code-global": scopeSession,
	// Kiro's free tier refills on a calendar month and its usage API reports
	// that date as nextDateReset (upstream quotaWindows.js:34
	// `kiro: { scope: QUOTA_SCOPES.MONTHLY }`). Without the entry a 402 naming
	// no reset was clamped to the generic two-hour ceiling, so a monthly-capped
	// account returned to rotation the same afternoon.
	"kiro": scopeMonthly,
	// Antigravity's free tier refills WEEKLY, and its reading names the moment
	// (`gemini_weekly.resetAt`). Without the entry a spent quota was clamped by the
	// generic ceilings, and the classifier's flat hour was what the account waited.
	"antigravity": scopeWeekly,
	"ag":          scopeWeekly,
}

// scopeFor resolves a provider id (or one of its aliases) to the quota scope it
// declares, or ok=false for a provider with no declared window.
func scopeFor(provider string) (quotaScope, bool) {
	if scope, ok := quotaWindowScopes[provider]; ok {
		return scope, true
	}
	scope, ok := quotaWindowScopes[providers.ResolveAlias(provider)]
	return scope, ok
}

// maxCooldownFor returns the ceiling for an announced wait on one provider.
//
// Most providers state a wait in minutes, and the generic two-hour ceiling is
// generous for them. The exceptions are the providers that name the real
// release time:
//   - deepseek-web announces a mute end (up to a week).
//   - CodeBuddy Intl/CN run on MONTHLY quotas (upstream quotaWindows.js
//     registers both as QUOTA_SCOPES.MONTHLY), so a frequency limit can name a
//     reset days away. Clamping that to two hours is what puts an exhausted
//     account back into rotation while it is still out of quota.
//   - Token Harbor runs on a WEEKLY rolling allowance, so its ceiling is one
//     weekly window: upstream bounds the same reset by
//     quotaWindowFor(provider).maxCooldownMs, and a daily/monthly ceiling would
//     either park an empty account for weeks or hand it back mid-window.
func maxCooldownFor(provider string) time.Duration {
	if provider == "deepseek-web" {
		return deepSeekWebMaxMuteCooldown
	}
	if scope, ok := scopeFor(provider); ok {
		switch scope {
		case scopeMonthly, scopeSession, scopeDaily:
			// Upstream's default ceiling is 40 days for every scope; a session
			// window only changes the CALENDAR fallback, not the ceiling.
			return maxMonthlyResetCooldown
		default:
			return maxWeeklyResetCooldown
		}
	}
	return maxResetCooldown
}

// comboPassRetryWait returns how long to hold a fully-failed combo pass before
// repeating it, or 0 when the pass must not be repeated.
//
// earliest is the smallest Retry-After any candidate asked for; rateLimited
// reports whether a 429 was among the failures. When it was, only a wait
// inside brief429RetryTolerance earns another pass — a longer one means every
// candidate in the pool is out of quota, and retrying is what kept the same
// limited accounts absorbing the client's retries.
func comboPassRetryWait(earliest time.Duration, rateLimited bool) time.Duration {
	limit := comboRetryWaitCap
	if rateLimited {
		limit = brief429RetryTolerance
	}
	if earliest <= 0 || earliest > limit {
		return 0
	}
	return earliest
}

// tokenharborAllowanceMarker is the phrase Token Harbor's free-tier refusal
// carries ("You've used this period's free allowance"). Upstream keys both the
// quota classification (quotaWindows.js QUOTA_EXHAUSTED_SIGNALS) and the reset
// parse (auth.js tokenharborResetMs) on it, so a 429 without it is a plain
// burst throttle and keeps the ordinary backoff.
const tokenharborAllowanceMarker = "free allowance"

// tokenharborResetRe matches the reset sentence a Token Harbor free-allowance
// 429 states: "Your next rolling 7-day period starts on 2 Oct 2026 at 06:22
// UTC". Ported from upstream TH_RESET_RE (src/sse/services/auth.js:22).
var tokenharborResetRe = regexp.MustCompile(
	`(?i)(?:starts? on|next .{0,24}period starts? on)\s+(\d{1,2}\s+[A-Za-z]{3}\s+\d{4})\s+at\s+(\d{1,2}):(\d{2})\s*UTC`)

// tokenharborResetAt parses the exact instant out of a Token Harbor
// free-allowance 429 body — upstream tokenharborResetMs
// (src/sse/services/auth.js:20-60).
//
// It applies only to the tokenharbor lane (its `th`/`thh` aliases resolve
// through the shared alias table) and only to a 429, and it refuses a body
// whose wording does not match rather than guessing a date: the shape of the
// sentence is the only evidence that a date in the text is the reset. A named
// instant that is already past names no future window, so it is refused too
// (upstream returns null for it) and the caller's calendar fallback applies.
func tokenharborResetAt(statusCode int, provider string, body []byte, now time.Time) (time.Time, bool) {
	if !isTokenharborAllowance(statusCode, provider, body) {
		return time.Time{}, false
	}
	m := tokenharborResetRe.FindStringSubmatch(string(body))
	if m == nil {
		return time.Time{}, false
	}
	// The layout carries no zone, so Parse yields UTC whatever the host's zone
	// is: the same body gives the same instant on every gateway, and Go's month
	// match ignores case the way the upstream regex does.
	reset, err := time.Parse("2 Jan 2006 15:04", m[1]+" "+m[2]+":"+m[3])
	if err != nil {
		return time.Time{}, false
	}
	if !reset.After(now) {
		return time.Time{}, false
	}
	return reset, true
}

// isTokenharborAllowance reports whether a failure is Token Harbor's
// free-allowance refusal at all: the tokenharbor lane (by id or alias), a 429,
// and the allowance wording in the body.
func isTokenharborAllowance(statusCode int, provider string, body []byte) bool {
	if statusCode != http.StatusTooManyRequests {
		return false
	}
	if providers.ResolveAlias(provider) != "tokenharbor" {
		return false
	}
	return strings.Contains(strings.ToLower(string(body)), tokenharborAllowanceMarker)
}

// tokenharborParkDuration returns how long a Token Harbor free-allowance 429
// parks the account, or ok=false for every other provider, status and body.
//
// Order of truth mirrors upstream: the instant the body states wins over the
// fixed quota cooldown the classifier hands back (auth.js:439-441 builds
// cooldownMs from the parsed reset before checkFallbackError's rule table is
// consulted), and it is bounded by the provider's own window. When the body
// names no parseable reset — the message shape changed, or the instant it named
// has passed — the provider's calendar takes over, exactly as upstream's
// quotaWindow.js resolver falls back to calendarResetMs(scope) for a
// WEEKLY-scoped provider.
func tokenharborParkDuration(statusCode int, provider string, ue *upstreamError, now time.Time) (time.Duration, bool) {
	var body []byte
	if ue != nil {
		body = ue.Body
	}
	if !isTokenharborAllowance(statusCode, provider, body) {
		return 0, false
	}
	if reset, ok := tokenharborResetAt(statusCode, provider, body, now); ok {
		return clampCooldown(reset.Sub(now), maxWeeklyResetCooldown), true
	}
	return clampCooldown(nextWeeklyRollover(now).Sub(now), maxWeeklyResetCooldown), true
}

// nextUTCMidnight is the calendar fallback for a DAILY quota: the first instant of
// the next UTC day (upstream calendarResetMs with resetUtcHour 0).
func nextUTCMidnight(now time.Time) time.Time {
	utc := now.UTC()
	return time.Date(utc.Year(), utc.Month(), utc.Day()+1, 0, 0, 0, 0, time.UTC)
}

// nextWeeklyRollover returns the coming weekly quota boundary: the next Monday
// at 00:00 UTC, or the Monday after when now IS a Monday. It is upstream
// calendarResetMs(QUOTA_SCOPES.WEEKLY) with weeklyResetWeekday = 1.
func nextWeeklyRollover(now time.Time) time.Time {
	utc := now.UTC()
	day := time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
	delta := (int(time.Monday) - int(utc.Weekday()) + 7) % 7
	if delta == 0 {
		delta = 7
	}
	return day.AddDate(0, 0, delta)
}

// retryableCooldownSec returns how long a failed account must stay out of
// rotation, in seconds, given the classifier's own backoff as the baseline.
//
// A 429 names the window the account is out for, in either direction: honour a
// long one and the next request no longer re-picks an exhausted account (the
// old code locked it for the classifier's 2s base, so the same limited account
// came back two seconds later and absorbed another failing request), and
// honour a short one and the bounded second pass below actually finds the
// account again instead of waiting out a lock it set itself. Every other
// status keeps the quota-reset reading it has always had. maxResetCooldown
// stops a hostile or nonsensical duration from parking an account forever.
//
// A Token Harbor free-allowance 429 overrides both readings: see
// tokenharborParkDuration.
func retryableCooldownSec(statusCode int, base time.Duration, ue *upstreamError, provider string) int {
	return retryableCooldownSecAt(statusCode, base, ue, provider, time.Now())
}

// retryableCooldownSecAt is retryableCooldownSec with the clock injected, so a
// test can pin the instant a park ends without waiting out the calendar.
func retryableCooldownSecAt(statusCode int, base time.Duration, ue *upstreamError, provider string, now time.Time) int {
	cooldown := base
	if statusCode == http.StatusTooManyRequests {
		if wait, ok := retryAfterWait(ue); ok {
			cooldown = wait
		}
	} else if dur, ok := extractResetDuration(ue.Body); ok {
		cooldown = dur
	}
	// The provider's own instant beats the fixed quota cooldown the classifier
	// returns for a free-allowance 429: parking for that flat hour hands the
	// account back days before the window the body named.
	if park, ok := tokenharborParkDuration(statusCode, provider, ue, now); ok {
		cooldown = park
	}
	return ceilSeconds(min(cooldown, maxCooldownFor(provider)))
}

// cooldownSecFor turns one upstream failure into the number of seconds its
// account must stay out of rotation. It exists so every fallback loop (the
// OpenAI walk, the combo pass) reads the same provider windows: a provider whose
// quota refills on a calendar can name a reset the rule table cannot compute.
//
// Kiro is the case that needs it. Its monthly cap says
// `MONTHLY_REQUEST_COUNT` and names no reset, so the window comes from the usage
// reading (nextDateReset) or the calendar — and the cap is recorded here, because
// from the next request on the account is skipped before it goes upstream. That
// is the difference between one 402 per account per month and one per model per
// retry (upstream auth.js `kiroMonthly` + markKiroCapped).
func (h *ChatHandler) cooldownSecFor(connID, provider string, ue *upstreamError, errorText string, cls providers.ErrorClassification) int {
	cooldownSec := retryableCooldownSec(ue.StatusCode, time.Duration(cls.CooldownMs)*time.Millisecond, ue, provider)
	// A spent DAILY allowance refills at the provider's calendar boundary, not after
	// a fixed hour: Agnes AI's free text quota resets at 00:00 UTC, and parking it
	// for the generic quota window would hand the account back mid-day to be refused
	// again (upstream resolveQuotaResetAt's calendar fallback, resetUtcHour 0).
	if cls.Quota && !KiroMonthlyVerdict(provider, ue.StatusCode, errorText) {
		if scope, ok := scopeFor(provider); ok && scope == scopeDaily {
			if wait := ceilSeconds(min(time.Until(nextUTCMidnight(time.Now().UTC())), maxCooldownFor(provider))); wait > 0 {
				return wait
			}
		}
	}
	// A spent Google/Antigravity quota names its window weekly. The reading the
	// tracker already holds is the reference's `usageKey: gemini_weekly`; the
	// calendar fallback is the next Monday 00:00 UTC. Without this the account was
	// parked for the classifier's flat hour (or, worse, the 2s burst base) and
	// re-dialled all week.
	if providers.ResolveAlias(provider) == "antigravity" && isQuotaExhaustionVerdict(ue.StatusCode, provider, errorText, ue, cls) {
		now := time.Now().UTC()
		// A verdict that names no reset instant is a detail-free refusal, and this
		// lane answers one for a request shape it dislikes exactly as it does for a
		// spent bucket ("Resource has been exhausted (e.g. check quota)." — measured
		// live against a bucket still reading 99% remaining). The account's own
		// reading is what separates the two: while the bucket has quota, hold the
		// ordinary cooldown and let the next account serve, instead of parking a
		// healthy account for the whole week.
		// shortcut: a connection with no reading yet still parks for the window,
		// because nothing has told us the bucket is alive.
		if _, namesReset := extractResetDuration(ue.Body); !namesReset {
			if remaining, ok := antigravityBucketRemaining(connID, AntigravityQuotaUsageKey, now); ok && remaining > 0 {
				return cooldownSec
			}
		}
		until := antigravityQuotaResetFor(connID, now)
		if secs := ceilSeconds(min(until.Sub(now), maxCooldownFor(provider))); secs > 0 {
			return secs
		}
		return cooldownSec
	}
	if !KiroMonthlyVerdict(provider, ue.StatusCode, errorText) {
		return cooldownSec
	}
	now := time.Now().UTC()
	until := KiroMonthlyReset(connID, now)
	if secs := ceilSeconds(min(until.Sub(now), maxCooldownFor(provider))); secs > 0 {
		cooldownSec = secs
	}
	MarkKiroCapped(connID, &until)
	return cooldownSec
}

// isQuotaExhaustionVerdict reports whether a failure the classifier already
// called retryable is a spent-budget verdict rather than a throttle. It decides
// which park the account gets: a spent budget is recorded with the quota window
// itself — upstream writes quotaExhaustedUntil from exactly this judgement,
// `quotaExhausted = isQuotaExhaustedError(errorText) || !!thResetAtMs`
// (src/sse/services/auth.js:422) — while a throttle only cools the account down.
//
// Four carriers answer it, because a provider may carry only one of them:
//
//   - the rule table's own spent-budget rows (providers.ErrorClassification.Quota:
//     "insufficient balance", CodeBuddy's "frequency limit", Token Harbor's
//     "free allowance"), which are what the cooldown was built from;
//   - the shared quota wording list (providers.LooksLikeQuotaExhausted, the port
//     of upstream's QUOTA_EXHAUSTED_PATTERNS): it names spent budgets the
//     cooldown table spells differently — "monthly quota", "plan limit", "out
//     of credits" — so a provider wording the rule table has not been taught
//     still parks as a quota rather than as a blip;
//   - the Token Harbor allowance sentence. That 429 body is prose, not JSON, so
//     no error text reaches the rule table at all (extractErrorText returns "");
//     the park resolver that already reads the sentence is what identifies it;
//   - the errclass taxonomy, which names the verdict a cooldown table states as
//     a bare status: a 402 payment-required is a spent balance, not a
//     request-scoped refusal.
//
// A plain 429 matching none of them stays a throttle, which is the case
// upstream's own suite pins ("generic rate limits keep the short backoff path").
func isQuotaExhaustionVerdict(statusCode int, provider, errorText string, ue *upstreamError, cls providers.ErrorClassification) bool {
	if cls.Quota || providers.LooksLikeQuotaExhausted(errorText) {
		return true
	}
	var body []byte
	if ue != nil {
		body = ue.Body
	}
	if isTokenharborAllowance(statusCode, provider, body) {
		return true
	}
	return errclass.ClassifyDetail("", statusCode, "", errorText).Kind == errclass.KindQuotaExhausted
}

// parkQuotaExhausted records the whole-account quota park for a failure when —
// and only when — the verdict is a spent budget, and reports whether it wrote
// one. until is the deadline the cooldown computation produced, handed in
// rather than recomputed so the park and the account cooldown name the same
// instant (upstream builds both from one reset timestamp).
//
// A failure that is not a quota verdict is left to the caller's ordinary
// account lock: a throttle says nothing about the account's balance, and
// persisting one as a spent budget would tell the operator to top up an account
// that only has to wait.
func (h *ChatHandler) parkQuotaExhausted(connID string, ue *upstreamError, provider, errorText string, cls providers.ErrorClassification, until time.Time) bool {
	if ue == nil {
		return false
	}
	if !isQuotaExhaustionVerdict(ue.StatusCode, provider, errorText, ue, cls) {
		return false
	}
	if err := h.Repo.LockConnectionQuotaExhausted(connID, until, cls.NewBackoffLevel, ue.StatusCode); err != nil {
		log.Warn("chat", "quota park failed", "conn", connID, "provider", provider, "error", err)
	}
	return true
}

// passRetry is the rate-limit bookkeeping for one combo pass. The OpenAI and
// Claude combo loops need the same three answers — how long to hold before
// repeating the pass, what to tell the client, and whether a 429 was among the
// failures — and keeping them together stops the two endpoints from drifting
// apart on a contract only one of them would enforce.
type passRetry struct {
	earliest    time.Duration
	rateLimited bool
}

// note folds an upstream failure into the pass-wide wait.
func (p *passRetry) note(ue *upstreamError) {
	if ue.StatusCode == http.StatusTooManyRequests {
		p.rateLimited = true
	}
	if wait, ok := retryAfterWait(ue); ok && (p.earliest == 0 || wait < p.earliest) {
		p.earliest = wait
	}
}

// reset starts a fresh pass: the previous one's exclusions and waits do not
// carry over.
func (p *passRetry) reset() { *p = passRetry{} }

// wait returns how long to hold before repeating the pass (with bounded random jitter),
// or 0 when the pass must not be repeated.
func (p *passRetry) wait() time.Duration {
	w := comboPassRetryWait(p.earliest, p.rateLimited)
	if w <= 0 {
		return 0
	}
	return addRetryJitter(w)
}

// writeError answers the client with lastErr, publishing the earliest
// Retry-After the pass collected three ways: the Retry-After header, the
// human-readable suffix on the error message, and the reset_at / retry_after
// pair in the JSON error object. The body fields exist because a client that
// never surfaces headers — a browser SDK, a log-only integration — has no
// other way to learn when the combo becomes usable again.
//
// Without a usable reset the upstream body is forwarded untouched: an invented
// reset instant would send clients into a retry loop against a quota with no
// known end.
func (p *passRetry) writeError(cw *committedResponseWriter, lastErr *upstreamError) {
	// A client that asked for a stream must be answered with one. The body is
	// the same JSON either way; what changes is the envelope. Writing the bare
	// document under a text/event-stream Content-Type leaves the client with no
	// frame separator it can split on, and a strict SSE reader reports the
	// first backslash it meets as `Unrecognized token '\'` — the status, type
	// and code never reach it.
	if cw.stream {
		cw.Header().Set(constants.HeaderContentType, constants.ContentTypeEventStream)
		if cw.responsesClient {
			var errBody struct {
				Error map[string]any `json:"error"`
			}
			if err := json.Unmarshal(lastErr.Body, &errBody); err == nil && errBody.Error != nil {
				if code, _ := errBody.Error["code"].(string); code == "context_length_exceeded" {
					cw.WriteHeader(http.StatusOK)
					_, _ = cw.Write([]byte(translator.FormatResponsesFailedSSE(errBody.Error)))
					if f, ok := cw.ResponseWriter.(http.Flusher); ok {
						f.Flush()
					}
					return
				}
			}
		}
		cw.WriteHeader(lastErr.StatusCode)
		frame := append([]byte("data: "), lastErr.Body...)
		frame = append(frame, '\n', '\n')
		_, _ = cw.Write(frame)
		_, _ = cw.Write([]byte("data: [DONE]\n\n"))
		if f, ok := cw.ResponseWriter.(http.Flusher); ok {
			f.Flush()
		}
		return
	}
	// Set before any branch so the pass-through path below is a JSON body
	// too. The upstream content type is not trusted here: a 429 served as
	// text/html is what makes an SDK report a parse crash instead of a quota.
	cw.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
	if p.earliest > 0 {
		sec := max(ceilSeconds(p.earliest), 1)
		cw.Header().Set("Retry-After", strconv.Itoa(sec))
		retryAt := time.Now().UTC().Add(p.earliest)
		retryHuman := formatRetryAfter(retryAt.Format(time.RFC3339))
		var errBody map[string]any
		if err := json.Unmarshal(lastErr.Body, &errBody); err == nil {
			if errObj, ok := errBody["error"].(map[string]any); ok {
				if msg, _ := errObj["message"].(string); msg != "" {
					errObj["message"] = msg + " (" + retryHuman + ")"
					// The instant has to travel as data, not only as prose:
					// only a human reads "reset after 2m 30s", and a client
					// that never surfaces the header has nothing else left.
					errObj["reset_at"] = retryAt.Format(time.RFC3339)
					errObj["retry_after"] = sec
					updated, _ := json.Marshal(errBody)
					cw.WriteHeader(lastErr.StatusCode)
					cw.Write(updated)
					return
				}
			}
		}
	}
	cw.WriteHeader(lastErr.StatusCode)
	cw.Write(lastErr.Body)
}

// retryAfterWait reports how long the upstream asked the client to wait before
// re-sending. It reads both carriers a rate limit uses — the Retry-After
// response header and the google.rpc.RetryInfo payload in the JSON body — plus
// the quota-reset forms extractRetryAfter already knows. The longest value wins,
// because a wait is only over when every source's window has passed.
func retryAfterWait(ue *upstreamError) (time.Duration, bool) {
	if ue == nil {
		return 0, false
	}
	var longest time.Duration
	found := false
	note := func(d time.Duration, ok bool) {
		if ok && d > longest {
			longest, found = d, true
		}
	}
	note(parseRetryAfterHeader(ue.Header.Get("Retry-After")))
	note(parseRateLimitResetHeaders(ue.Header))
	note(quotaResetTimestamp(ue.Body))
	note(retryInfoDelay(ue.Body))
	// extractRetryAfter already resolves quotaResetDelay, "Resets in X" and the
	// retryAfter/retry_after/resetsAt/resets_at fields; re-reading its string
	// keeps one owner of the field names.
	note(parseRetryAfterValue(extractRetryAfter(ue.Body)))
	return longest, found
}

// parseRateLimitResetHeaders reads the two carriers Google and OpenAI-style
// gateways put beside Retry-After, which the executor's parseRetryHeaders also
// reads: `x-ratelimit-reset-after` is a delta in seconds and
// `x-ratelimit-reset` is an epoch in seconds. Without them a response that
// announced its window only in these headers left the account on the burst
// backoff.
func parseRateLimitResetHeaders(h http.Header) (time.Duration, bool) {
	if h == nil {
		return 0, false
	}
	if v := strings.TrimSpace(h.Get("x-ratelimit-reset-after")); v != "" {
		if secs, err := strconv.ParseFloat(v, 64); err == nil && secs > 0 {
			return time.Duration(secs * float64(time.Second)), true
		}
	}
	if v := strings.TrimSpace(h.Get("x-ratelimit-reset")); v != "" {
		if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
			if wait := time.Until(time.Unix(secs, 0)); wait > 0 {
				return wait, true
			}
		}
	}
	return 0, false
}

// rateLimitDetail is one google.rpc detail of a rate-limit body; only the
// ErrorInfo metadata bag is read.
type rateLimitDetail struct {
	Metadata map[string]string `json:"metadata"`
}

// rateLimitDetails returns the detail list of a 409/429 body, reading both the
// wrapped shape Google sends (`error.details`) and the unwrapped one some relays
// forward instead.
func rateLimitDetails(body []byte) []rateLimitDetail {
	if len(body) == 0 {
		return nil
	}
	var envelope struct {
		Details []rateLimitDetail `json:"details"`
		Error   struct {
			Details []rateLimitDetail `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil
	}
	if len(envelope.Error.Details) > 0 {
		return envelope.Error.Details
	}
	return envelope.Details
}

// quotaResetInstant returns the exact instant a 409/429 body names, in the
// order upstream's Antigravity parseError reads the carriers
// (open-sse/executors/antigravity.js:455-503): the ErrorInfo
// `quotaResetTimeStamp`, then the RetryInfo `retryDelay`, then the
// `quotaResetDelay` delay prose.
//
// The instant is the bucket's own end, not a retry window, so nothing is
// clamped here. retryAfterWait's duration is what the ACCOUNT cooldown uses, and
// that path keeps its ceilings; a model-level park that clamped a multi-day
// exhaustion to the generic two-hour window would put the account back into
// rotation while it is still empty — the next request is a guaranteed-failing
// round trip.
//
// Callers pass the body of a 409/429. Other statuses carry no quota reset.
func quotaResetInstant(body []byte, now time.Time) (time.Time, bool) {
	if wait, ok := quotaResetTimestamp(body); ok {
		return now.Add(wait), true
	}
	if dur, ok := retryInfoDelay(body); ok {
		return now.Add(dur), true
	}
	if dur, ok := quotaResetDelay(body); ok {
		return now.Add(dur), true
	}
	return time.Time{}, false
}

// quotaResetDelay reads the ErrorInfo `quotaResetDelay` ("74h19m34.031069133s")
// without the two-hour clamp extractResetDuration applies, so the park path can
// last as long as the bucket it names.
func quotaResetDelay(body []byte) (time.Duration, bool) {
	for _, d := range rateLimitDetails(body) {
		if dur, ok := parseDurationString(d.Metadata["quotaResetDelay"]); ok {
			return dur, true
		}
	}
	return 0, false
}

// quotaResetTimestamp reads the precise reset Google puts on a 409/429 body:
// `details[].metadata.quotaResetTimeStamp`, an ISO-8601 instant. Antigravity's
// generation endpoint answers RESOURCE_EXHAUSTED with that field — usually
// beside `quotaResetDelay` and a google.rpc.RetryInfo `retryDelay` — and the
// executor's parseError takes the instant over the live quota API, which has no
// per-model row for a free-tier account.
//
// The other two carriers were already read (extractResetDuration reads
// quotaResetDelay, retryInfoDelay reads the RetryInfo detail), so a body that
// named ONLY the instant left the account with no cooldown at all:
// retryableCooldownSec fell back to the classifier's two-second base and the
// same exhausted account absorbed the next request.
//
// Google nests the details under `error.details`; some relays surface them
// unwrapped at the top level, and the executor tolerates both. A stamp already
// in the past names no future window, so it is skipped rather than clamped to
// the five-second floor — upstream skips it too and falls through to the
// message patterns.
func quotaResetTimestamp(body []byte) (time.Duration, bool) {
	details := rateLimitDetails(body)
	if len(details) == 0 {
		return 0, false
	}
	for _, d := range details {
		raw := strings.TrimSpace(d.Metadata["quotaResetTimeStamp"])
		if raw == "" {
			continue
		}
		reset, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			continue
		}
		if wait := time.Until(reset); wait > 0 {
			return wait, true
		}
	}
	return 0, false
}

// retryInfoDelay reads the google.rpc.RetryInfo detail a rate-limited Gemini
// request carries. retryDelay sits on the detail itself, not inside ErrorInfo
// metadata, so extractResetDuration (which only looks for quotaResetDelay there)
// never sees it — and Google's most common carrier of the wait was therefore
// invisible to the whole cooldown/failover path.
func retryInfoDelay(body []byte) (time.Duration, bool) {
	if len(body) == 0 {
		return 0, false
	}
	var rpcErr struct {
		Error struct {
			Details []struct {
				Type        string `json:"@type"`
				RetryDelay  string `json:"retryDelay"`
				MetadataMap struct {
					RetryDelay string `json:"retryDelay"`
				} `json:"metadata"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &rpcErr); err != nil {
		return 0, false
	}
	for _, d := range rpcErr.Error.Details {
		if !strings.Contains(d.Type, "RetryInfo") && d.RetryDelay == "" && d.MetadataMap.RetryDelay == "" {
			continue
		}
		if dur, ok := parseDurationString(d.RetryDelay); ok {
			return dur, true
		}
		if dur, ok := parseDurationString(d.MetadataMap.RetryDelay); ok {
			return dur, true
		}
	}
	return 0, false
}

// parseRetryAfterHeader reads an RFC 9110 Retry-After value: delta-seconds or
// an HTTP-date. Anything else — an empty header, a bare word, a date already in
// the past — carries no usable wait.
func parseRetryAfterHeader(v string) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d, true
		}
	}
	return 0, false
}

// parseRetryAfterValue reads a Retry-After carried in a JSON body, which
// upstreams spell as an RFC3339 timestamp, a Go duration ("120s") or bare
// seconds ("120").
func parseRetryAfterValue(v string) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		if d := time.Until(t); d > 0 {
			return d, true
		}
		return 0, false
	}
	return parseDurationString(v)
}

func parseDurationString(v string) (time.Duration, bool) {
	v = strings.TrimRight(strings.TrimSpace(v), ".")
	if v == "" {
		return 0, false
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d, d > 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		return time.Duration(secs) * time.Second, secs > 0
	}
	return 0, false
}

func ceilSeconds(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	return int((d + time.Second - 1) / time.Second)
}
