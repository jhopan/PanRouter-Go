// CodeBuddy Intl daily auto-ping: the daily check-in / activity keeper.
//
// Port of upstream src/shared/services/codebuddyAutoPing.js. One tiny chat
// request per connection per day keeps the account's daily sign-in / activity
// state alive. The slot is spread across the cycle so the accounts do not fire
// together, because the daily chat burst is what got them restricted
// (11140 "request illegal").
//
// The whole feature sits behind a master switch that has to be SET to "1", and
// it drives the shared Scheduler rather than a loop of its own: the cycle
// decides *whether* a connection is due, the Scheduler decides *when* it fires
// and guarantees the slot is not fired twice.
package schedule

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"panrouter/proxy/internal/envx"
	"panrouter/proxy/internal/log"
)

const (
	// CodeBuddyProvider is the provider the daily cycle serves. Upstream lists
	// the connections of exactly this one, active only
	// (getProviderConnections {provider: "codebuddy-intl", isActive: true}).
	CodeBuddyProvider = "codebuddy-intl"

	// CodeBuddyTickInterval matches upstream CODEBUDDY_AUTOPING_CONFIG
	// .tickIntervalMs (5 minutes). The slot has minute granularity, so a
	// one-minute tick would take the same decision five times over.
	CodeBuddyTickInterval = 5 * time.Minute

	// CodeBuddyCycleResetHourUTC is the hour the cycle resets, in UTC: upstream
	// resets at 07:00 WIB, and WIB is a fixed UTC+7 with no DST, so that
	// boundary is 00:00 UTC.
	CodeBuddyCycleResetHourUTC = 7

	// CodeBuddySlotStartMinute / CodeBuddySlotEndMinute are the slot window
	// inside a cycle: 1 minute after the reset (07:01 WIB) to 2 minutes before
	// the next reset (06:58 WIB).
	CodeBuddySlotStartMinute = 1
	CodeBuddySlotEndMinute   = 1438

	// CodeBuddyPingModel is the default ping model: the gateway's generic cheap
	// alias, available to every account tier. A concrete id such as
	// gpt-5.6-luna is gated per plan and answers 11158 ("model ... is not
	// allowed for user type personal_free_user") on a free account, which
	// failed the whole daily ping.
	CodeBuddyPingModel = "fast-model"

	// CodeBuddyPingMaxTokens is upstream's pingMaxTokens: the CodeBuddy
	// GPT-family endpoints reject a smaller max_tokens, and 16 is one word.
	CodeBuddyPingMaxTokens = 16

	// CodeBuddyFailureCooldown is upstream's failureCooldownMs (1 hour). A
	// connection whose ping was refused is left alone for an hour instead of
	// being retried on every tick, and it is retried after that — upstream does
	// not give up on the cycle because one attempt failed.
	CodeBuddyFailureCooldown = time.Hour

	// CodeBuddyTouchThrottle is upstream's touchThrottleMs (2 hours): the
	// smallest gap between two touches of one connection. It is restart-safe
	// because it is compared against the timestamp stored on the row, and
	// process-local for the same reason.
	CodeBuddyTouchThrottle = 2 * time.Hour

	// wibOffset is the fixed +07:00 every instant in this file is converted
	// with. Never time.Local: the box runs in whatever zone the operator set,
	// and the cycle must not move with it.
	wibOffset = 7 * time.Hour

	minutesPerHour = 60
	minutesPerDay  = 24 * minutesPerHour

	// codeBuddyStateTTL is how long one connection's state is kept. Everything
	// read from it is bounded by the cycle (one day) plus the cooldown, so a
	// day of slack keeps the maps bounded on a long-running gateway.
	codeBuddyStateTTL = 24 * time.Hour
)

// CodeBuddyPingModelFallbacks is the chain a ping walks after the preferred
// model, ported from upstream pingModelFallbacks. `default-model` and
// `balanced-model` are the gateway's other tier-agnostic aliases.
var CodeBuddyPingModelFallbacks = []string{"default-model", "balanced-model"}

// CodeBuddyAutoPingEnabled reports whether the daily check-in is switched on.
//
// Off unless CODEBUDDY_AUTOPING_ENABLED is exactly "1". The daily chat burst is
// what triggered the CodeBuddy Intl account restrictions (11140 "request
// illegal"), so the feature has to be switched on deliberately — an environment
// variable that has to be SET to "1", not one that has to be unset.
//
// Upstream wrote this as `!== "0"`, which defaults to ON: the operator was
// never asked, the scheduler started anyway, and it kept pinging accounts that
// were already restricted.
//
// The bare name wins because that is the name upstream reads and the name this
// repo's docs give (AGENTS.md §5, PORT_GUIDE.md §5). envx covers the prefixed
// spellings the same variable carries after this project's renames, so a .env
// written either way switches the feature on — and a spelling that is blank
// counts as unset, exactly as envx reads its own names.
func CodeBuddyAutoPingEnabled() bool {
	if value := strings.TrimSpace(os.Getenv("CODEBUDDY_AUTOPING_ENABLED")); value != "" {
		return value == "1"
	}
	return envx.Get("CODEBUDDY_AUTOPING_ENABLED") == "1"
}

// CodeBuddyCycleStart returns the instant the cycle containing now began: 07:00
// WIB, i.e. 00:00 UTC on the same UTC date.
func CodeBuddyCycleStart(now time.Time) time.Time {
	utc := now.UTC()
	return time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
}

// CodeBuddyDayKey is the cycle key of a timestamp: the UTC calendar date of the
// cycle it falls in.
//
// Upstream formats (now - resetHour*3600e3) in Asia/Jakarta. Jakarta is a fixed
// UTC+7 with no DST, so shifting the instant back by the reset hour and reading
// it at +07:00 is reading the instant in UTC — the cycle boundary is exactly UTC
// midnight, and 07:01 WIB today through 06:58 WIB tomorrow share one key.
//
// The arithmetic is a fixed offset, never time.Local: the cycle has to sit
// still no matter what zone the box is in, which is also what upstream
// achieves by naming the zone instead of using the host's.
func CodeBuddyDayKey(now time.Time) string {
	return now.UTC().Format("2006-01-02")
}

// CodeBuddyCycleMinute is how far into the cycle now is: 0 at 07:00 WIB, 1439
// one minute before the next reset.
//
// Upstream shifts to UTC+7 and counts
// (minuteOfDay - resetHour*60 + 1440) % 1440; the second modulo is Go's, whose
// % keeps the sign of the dividend and would otherwise answer a negative minute
// for the first seven hours of the UTC day.
func CodeBuddyCycleMinute(now time.Time) int {
	wib := now.UTC().Add(wibOffset)
	minuteOfDay := wib.Hour()*minutesPerHour + wib.Minute()
	return ((minuteOfDay-CodeBuddyCycleResetHourUTC*minutesPerHour)%minutesPerDay + minutesPerDay) % minutesPerDay
}

// CodeBuddySlotMinute is the deterministic minute-of-cycle one connection pings
// in: 1 (07:01 WIB) to 1438 (06:58 WIB the next day).
//
// The hash is upstream's: a rolling h*131 over "<connectionId>::<dayKey>",
// folded to 32 bits on every step, then taken modulo the span. It is stable for
// a given (connectionId, cycleKey) — no RNG at tick time — so every tick of one
// cycle, and every process restart, agrees on the same slot.
//
// The fold is Go's uint32 wraparound, which is upstream's `>>> 0`. Upstream
// hashes UTF-16 code units and this hashes bytes; a connection id is ASCII in
// both runtimes, so the two agree on every id this gateway stores.
func CodeBuddySlotMinute(connectionID, dayKey string) int {
	key := connectionID + "::" + dayKey
	var hash uint32
	for i := range len(key) {
		hash = hash*131 + uint32(key[i])
	}
	span := CodeBuddySlotEndMinute - CodeBuddySlotStartMinute
	if span < 1 {
		span = 1
	}
	return CodeBuddySlotStartMinute + int(hash%uint32(span))
}

// CodeBuddyPingedToday reports whether a connection was already pinged in this
// cycle, from the record upstream keeps on the row: the cycle key written with
// the last ping, or the cycle that ping's timestamp falls in (a row written by
// an older version carries only the timestamp).
func CodeBuddyPingedToday(conn CodeBuddyConnection, dayKey string) bool {
	if conn.LastAutoPingDayKey == dayKey {
		return true
	}
	return !conn.LastAutoPingAt.IsZero() && CodeBuddyDayKey(conn.LastAutoPingAt) == dayKey
}

// CodeBuddyPingModels is the candidate list one ping walks: the operator's
// override, else the built-in default, then the fallback chain, deduplicated
// and with blanks dropped.
func CodeBuddyPingModels(override string) []string {
	first := strings.TrimSpace(override)
	if first == "" {
		first = CodeBuddyPingModel
	}
	candidates := make([]string, 0, 1+len(CodeBuddyPingModelFallbacks))
	seen := make(map[string]bool, cap(candidates))
	for _, model := range append([]string{first}, CodeBuddyPingModelFallbacks...) {
		if model == "" || seen[model] {
			continue
		}
		seen[model] = true
		candidates = append(candidates, model)
	}
	return candidates
}

// CodeBuddyPingError is a refused ping attempt, carrying the two facts the
// fallback walk classifies: the upstream status and the upstream's own words.
type CodeBuddyPingError struct {
	Status int
	Text   string
}

// CodeBuddyMaxErrorText is how much of the upstream text a ping failure keeps,
// matching upstream's `lastText.slice(0, 160)`.
const CodeBuddyMaxErrorText = 160

func (e *CodeBuddyPingError) Error() string {
	text := e.Text
	if len(text) > CodeBuddyMaxErrorText {
		text = text[:CodeBuddyMaxErrorText]
	}
	if e.Status == 0 {
		return "codebuddy-intl ping failed: " + text
	}
	return fmt.Sprintf("codebuddy-intl ping failed (%d): %s", e.Status, text)
}

// CodeBuddyCreditsExhausted reports the answer upstream stops the model walk on:
// a 429. The account is out of credits, and no other model can help, so trying
// the next candidate only spends another request.
func CodeBuddyCreditsExhausted(err error) bool {
	var pingErr *CodeBuddyPingError
	return errors.As(err, &pingErr) && pingErr.Status == 429
}

// CodeBuddyModelGated reports whether a refusal is a per-model gating rejection,
// the only failure worth retrying on another model.
//
// Upstream's test is /11158|11101|not allowed for user type|model.*not allowed/i:
// 11158 is "model X is not allowed for user type personal_free_user", 11101 the
// plan-gating code the CodeBuddy gateway also answers for a model a tier may
// not use. The last arm is checked as two substrings rather than a regex,
// because the text between "model" and "not allowed" is not part of the
// decision.
func CodeBuddyModelGated(err error) bool {
	var pingErr *CodeBuddyPingError
	if !errors.As(err, &pingErr) {
		return false
	}
	text := strings.ToLower(pingErr.Text)
	if strings.Contains(text, "11158") ||
		strings.Contains(text, "11101") ||
		strings.Contains(text, "not allowed for user type") {
		return true
	}
	return strings.Contains(text, "model") && strings.Contains(text, "not allowed")
}

// CodeBuddySettings is the parsed `codebuddyIntlAutoPing` settings value.
//
// The per-connection map says WHICH connections ping, not WHETHER the feature
// runs: enabling it needs the master switch, and `enabled: false` beside a
// leftover per-connection true must not start it (upstream's regression test
// pins exactly that, because the check used to OR the connection map into the
// enabled flag).
type CodeBuddySettings struct {
	Enabled     bool
	Connections map[string]bool
	Model       string
}

// ParseCodeBuddySettings reads a stored settings value. Anything that is not an
// object yields the zero value, which is disabled: a payload the parser does not
// understand must not switch the feature on.
func ParseCodeBuddySettings(value any) CodeBuddySettings {
	out := CodeBuddySettings{Connections: map[string]bool{}}
	obj, ok := value.(map[string]any)
	if !ok {
		return out
	}
	if enabled, ok := obj["enabled"].(bool); ok {
		out.Enabled = enabled
	}
	out.Model, _ = obj["model"].(string)
	if conns, ok := obj["connections"].(map[string]any); ok {
		for id, raw := range conns {
			if on, ok := raw.(bool); ok && id != "" {
				out.Connections[id] = on
			}
		}
	}
	return out
}

// CodeBuddySettingsFromBlob reads `codebuddyIntlAutoPing` out of the settings
// blob the dashboard stores.
func CodeBuddySettingsFromBlob(raw map[string]any) CodeBuddySettings {
	if raw == nil {
		return CodeBuddySettings{Connections: map[string]bool{}}
	}
	return ParseCodeBuddySettings(raw["codebuddyIntlAutoPing"])
}

// CodeBuddyConnection is the slice of a provider connection the daily cycle
// reads: the id, the label the log line carries, the data blob (the credential
// refresh reads the token out of it) and the row's own record of the last
// attempt — upstream's lastAutoPingDayKey / lastAutoPingAt / lastAutoPingModel.
type CodeBuddyConnection struct {
	ID                 string
	DisplayName        string
	Data               string
	LastAutoPingDayKey string
	LastAutoPingAt     time.Time
}

// CodeBuddyStore reads the connections the cycle decides on and writes the
// attempt back, mirroring upstream's getProviderConnections /
// updateProviderConnection pair.
type CodeBuddyStore interface {
	// Connections returns the active CodeBuddy Intl connections.
	Connections() ([]CodeBuddyConnection, error)
	// RecordAttempt writes one successful ping onto the connection row.
	RecordAttempt(connectionID, dayKey string, at time.Time, model string) error
}

// CodeBuddySender sends one ping on one model.
//
// A failed attempt is reported as *CodeBuddyPingError when the sender knows the
// status and the upstream's words, which is what the fallback walk classifies.
// Any other error ends the walk, exactly as upstream's does when the body
// carries no gating signal.
type CodeBuddySender func(ctx context.Context, conn CodeBuddyConnection, model string) error

// CodeBuddyAutoPing runs the daily check-in for the connections the operator
// switched on.
//
// It owns the *decision* — which connection is due on this tick — and hands the
// firing to the shared Scheduler, so there is no second tick loop. Its state is
// process-local, one entry per connection, and pruned once it is older than the
// cycle it belongs to:
//
//   - pending  the connections this tick found due, with the model each one
//     should use (upstream passes the settings' model into the ping the same
//     way, from the settings it read at the top of the tick)
//   - state    the last successful ping (upstream's g.lastTouch), the cooldown
//     after a failed attempt (upstream's g.failureCache), and how many attempts
//     one connection has already failed in this cycle. The count is what lets
//     the slot key differ after a cooldown: upstream retries a connection it
//     failed, unlike the quota scheduler, which leaves a failed slot claimed.
type CodeBuddyAutoPing struct {
	// Interval is the tick interval; zero means CodeBuddyTickInterval.
	Interval time.Duration
	// Now is the clock; nil means time.Now. Injectable so a test can place a
	// tick at an exact minute of the cycle.
	Now func() time.Time
	// Enabled is the master switch; nil means CodeBuddyAutoPingEnabled.
	Enabled func() bool
	// Settings returns the current `codebuddyIntlAutoPing` configuration. It is
	// read on every tick rather than cached, so a model override saved in the
	// dashboard applies without a restart (upstream reads settings per tick).
	Settings func() CodeBuddySettings
	// Store reads the connection rows and records the attempts.
	Store CodeBuddyStore
	// Send performs one ping attempt on one model.
	Send CodeBuddySender

	mu       sync.Mutex
	pending  map[string]codeBuddyPending
	state    map[string]codeBuddyState
	schedule *Scheduler
	// immediate is upstream's g.immediateNextTick: the next tick fires every
	// switched-on connection whose slot has not arrived yet. It is set when the
	// scheduler starts and when the operator's settings change, which is the
	// moment upstream's configureCodebuddyAutoPing sets it.
	immediate bool
	// settingsKey identifies the settings the previous tick saw, so a saved
	// change (enabling the feature, adding a connection, changing the model) is
	// the configure signal upstream gets as a call.
	settingsKey string
}

type codeBuddyPending struct {
	conn    CodeBuddyConnection
	model   string
	dayKey  string
	attempt int
}

type codeBuddyState struct {
	touchedAt time.Time
	failedAt  time.Time
	dayKey    string
	attempts  int
}

// codeBuddyDecision is one connection's verdict for the tick.
type codeBuddyDecision struct {
	Due    bool
	Slot   int
	Reason string
}

// NewCodeBuddyAutoPing builds the daily cycle. Every collaborator can be
// replaced before the first tick (a test does exactly that); the values left
// nil are the production ones.
func NewCodeBuddyAutoPing() *CodeBuddyAutoPing {
	cycle := &CodeBuddyAutoPing{
		Interval: CodeBuddyTickInterval,
		Now:      time.Now,
		Enabled:  CodeBuddyAutoPingEnabled,
		pending:  map[string]codeBuddyPending{},
		state:    map[string]codeBuddyState{},
		// startCodebuddyAutoPing sets g.immediateNextTick before its first
		// tick, so a process that comes up inside a cycle does not wait for
		// every slot it has already passed.
		immediate: true,
	}
	cycle.schedule = NewScheduler(nil, nil)
	cycle.schedule.Now = cycle.now
	cycle.schedule.Due = cycle.due
	cycle.schedule.Ping = cycle.ping
	return cycle
}

// Tick runs one pass of the cycle and reports the ping errors the scheduler
// collected.
func (c *CodeBuddyAutoPing) Tick(ctx context.Context) []error {
	return c.schedule.Tick(ctx)
}

// Run ticks until ctx is cancelled. The first tick runs immediately, so a
// process that starts inside the cycle fires the slot that is already due.
func (c *CodeBuddyAutoPing) Run(ctx context.Context) {
	c.schedule.Interval = c.interval()
	c.schedule.Run(ctx)
}

// due reports the connections whose slot has arrived, and remembers each one's
// model for the ping that follows.
//
// It is the whole per-connection decision: the master switch, the settings
// switch, then for every switched-on connection — already pinged this cycle, in
// the failure cooldown, inside the touch throttle, and finally the slot minute
// against the elapsed cycle minute, which is upstream's order.
func (c *CodeBuddyAutoPing) due(now time.Time) []DueSlot {
	if !c.enabled() {
		return nil
	}
	settings := c.settings()
	if !settings.Enabled {
		return nil
	}
	if c.Store == nil {
		return nil
	}
	conns, err := c.Store.Connections()
	if err != nil {
		log.Warn("cb_ping", "connection list failed", "error", err)
		return nil
	}

	dayKey := CodeBuddyDayKey(now)
	cycleMinute := CodeBuddyCycleMinute(now)
	immediate := c.consumeImmediate(settings)

	c.mu.Lock()
	c.pending = make(map[string]codeBuddyPending, len(conns))
	c.pruneLocked(now)
	c.mu.Unlock()

	var due []DueSlot
	for _, conn := range conns {
		if !settings.Connections[conn.ID] {
			continue
		}
		decision := c.decide(conn, now, dayKey, cycleMinute, immediate)
		if !decision.Due {
			// A connection that is not due yet is silently left alone
			// (upstream `continue`s there). The other skips are logged at
			// debug: upstream logs them every tick, which is one line per
			// connection every five minutes for the rest of the day.
			if decision.Reason != "" {
				log.Debug("cb_ping", connLabel(conn)+": skip", "reason", decision.Reason)
			}
			continue
		}
		c.mu.Lock()
		attempt := c.state[conn.ID].attempts
		if c.state[conn.ID].dayKey != dayKey {
			attempt = 0
		}
		c.pending[conn.ID] = codeBuddyPending{conn: conn, model: settings.Model, dayKey: dayKey, attempt: attempt}
		c.mu.Unlock()

		due = append(due, DueSlot{
			Provider:     CodeBuddyProvider,
			ConnectionID: conn.ID,
			Slot: &Slot{
				// The key is the deterministic slot this connection owns in
				// this cycle, plus the attempt it is on. Two ticks in the same
				// minute produce the same key and the scheduler fires once;
				// after a cooldown the attempt makes the key new, which is what
				// lets the retry through.
				Key: dayKey + "#" + strconv.Itoa(decision.Slot) + "#" + strconv.Itoa(attempt),
				At:  CodeBuddyCycleStart(now).Add(time.Duration(decision.Slot) * time.Minute),
			},
		})
	}
	return due
}

// decide applies the per-connection gates for one tick. It takes the state lock
// itself, because it is called once per connection.
func (c *CodeBuddyAutoPing) decide(conn CodeBuddyConnection, now time.Time, dayKey string, cycleMinute int, immediate bool) codeBuddyDecision {
	if CodeBuddyPingedToday(conn, dayKey) {
		return codeBuddyDecision{Reason: "already pinged today"}
	}

	c.mu.Lock()
	state := c.state[conn.ID]
	c.mu.Unlock()

	if !state.failedAt.IsZero() && now.Sub(state.failedAt) < CodeBuddyFailureCooldown {
		return codeBuddyDecision{Reason: "in failure cooldown"}
	}

	// Upstream compares the throttle against the in-memory touch first and the
	// stored timestamp second (`g.lastTouch[conn.id] || conn.lastAutoPingAt`):
	// the stored one survives a restart, the in-memory one also covers a row
	// whose write did not land.
	lastTouch := conn.LastAutoPingAt
	if state.touchedAt.After(lastTouch) {
		lastTouch = state.touchedAt
	}
	if !lastTouch.IsZero() && now.Sub(lastTouch) < CodeBuddyTouchThrottle {
		return codeBuddyDecision{Reason: "inside the restart-safe touch throttle"}
	}

	slot := CodeBuddySlotMinute(conn.ID, dayKey)
	// A connection that has never been pinged has not had its check-in in any
	// cycle yet, so the first tick that sees it fires it instead of waiting for
	// a slot that was derived for a day it had not been added on. The immediate
	// arm is the same rule for a connection that *was* pinged in an earlier
	// cycle: upstream fires it on the first tick after the scheduler starts or
	// the settings are switched on, rather than making the operator wait for a
	// slot that may be hours away. Upstream spells both as one condition —
	// `!isImmediate && !isNew && minutesInCycle < slot`.
	//
	// The master switch is what keeps that burst deliberate: nothing here runs
	// unless CODEBUDDY_AUTOPING_ENABLED is "1".
	if cycleMinute < slot && !immediate && !codeBuddyNeverPinged(conn) {
		// Not due yet: upstream's silent continue.
		return codeBuddyDecision{}
	}
	return codeBuddyDecision{Due: true, Slot: slot}
}

// consumeImmediate reports whether this tick fires regardless of the slot time,
// and clears the flag.
//
// Upstream has one flag, set by startCodebuddyAutoPing and by
// configureCodebuddyAutoPing when the feature is switched on. This port has the
// start case (the constructor sets it) and derives the configure case from the
// settings: the dashboard saves them and the next tick is the configure call. A
// changed settings blob therefore buys exactly one immediate tick, which is what
// upstream does — a second tick without a change waits for the slots again.
func (c *CodeBuddyAutoPing) consumeImmediate(settings CodeBuddySettings) bool {
	key := settings.key()

	c.mu.Lock()
	defer c.mu.Unlock()
	// The first tick has no previous key to compare against: the constructor's
	// flag already covers that case, so an empty key is not a change.
	changed := c.settingsKey != "" && key != c.settingsKey
	c.settingsKey = key
	immediate := c.immediate
	c.immediate = false
	return immediate || changed
}

// key is a stable identity for a settings value: the switches and the model,
// plus every connection the map names. An empty value (a feature that was never
// configured) and a saved-off one are different keys, so saving "off" is a
// change too — the tick it buys finds nothing due and stops, which is upstream's
// stop path.
func (s CodeBuddySettings) key() string {
	ids := make([]string, 0, len(s.Connections))
	for id, on := range s.Connections {
		ids = append(ids, id+"="+strconv.FormatBool(on))
	}
	sort.Strings(ids)
	return strconv.FormatBool(s.Enabled) + "|" + s.Model + "|" + strings.Join(ids, ",")
}

// codeBuddyNeverPinged reports a connection with no attempt record at all —
// upstream's `isNew` (`!conn.lastAutoPingAt`, with the cycle key of a newer
// write counted as a record too).
func codeBuddyNeverPinged(conn CodeBuddyConnection) bool {
	return conn.LastAutoPingDayKey == "" && conn.LastAutoPingAt.IsZero()
}

// ping sends one connection's daily ping: the credential is refreshed, then the
// candidate models are walked, then the attempt is recorded on the row.
func (c *CodeBuddyAutoPing) ping(ctx context.Context, provider, connectionID string) error {
	pending, ok := c.takePending(connectionID)
	if !ok {
		// The due pass did not plan this connection (its slot was claimed by an
		// earlier tick, or a gate refused it): nothing to send.
		return nil
	}
	if provider != CodeBuddyProvider {
		return fmt.Errorf("codebuddy auto-ping: unexpected provider %q", provider)
	}
	if c.Send == nil {
		return fmt.Errorf("codebuddy auto-ping: no sender")
	}

	now := c.now()
	label := connLabel(pending.conn)

	var lastErr error
	for _, model := range CodeBuddyPingModels(pending.model) {
		err := c.Send(ctx, pending.conn, model)
		if err == nil {
			c.recordSuccess(pending, now, model, label)
			return nil
		}
		lastErr = err

		if CodeBuddyCreditsExhausted(err) {
			// Upstream stops the walk here instead of trying the next model:
			// no other model can help an account that is out of credits, and
			// each further attempt spends a request. It is reported as a skip
			// rather than thrown — the cooldown is what stays behind, and the
			// log line names the reason.
			c.recordFailure(pending, now)
			log.Warn("cb_ping", label+": skip", "reason", "upstream 429 credits exhausted", "error", err)
			return nil
		}
		if !CodeBuddyModelGated(err) {
			break
		}
	}

	c.recordFailure(pending, now)
	log.Warn("cb_ping", label+": ping failed", "error", lastErr)
	return lastErr
}

// recordSuccess closes the cycle for one connection: the attempt goes onto the
// row, and the failure state is dropped (upstream deletes its failureCache entry
// and writes lastAutoPingDayKey / lastAutoPingAt / lastAutoPingModel).
func (c *CodeBuddyAutoPing) recordSuccess(pending codeBuddyPending, now time.Time, model, label string) {
	c.mu.Lock()
	c.state[pending.conn.ID] = codeBuddyState{touchedAt: now}
	c.mu.Unlock()

	if c.Store != nil {
		if err := c.Store.RecordAttempt(pending.conn.ID, pending.dayKey, now, model); err != nil {
			log.Warn("cb_ping", label+": record attempt failed", "error", err)
		}
	}
	log.Info("cb_ping", label+": ping ok", "model", model)
}

// recordFailure starts the cooldown for one connection and counts the attempt,
// so the retry after the cooldown is allowed a fresh slot key.
func (c *CodeBuddyAutoPing) recordFailure(pending codeBuddyPending, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	state := c.state[pending.conn.ID]
	if state.dayKey != pending.dayKey {
		state.dayKey = pending.dayKey
		state.attempts = 0
	}
	state.failedAt = now
	state.attempts++
	c.state[pending.conn.ID] = state
}

// takePending hands the plan for one connection to the ping and drops it, so a
// tick cannot ping a connection it did not plan in the same pass.
func (c *CodeBuddyAutoPing) takePending(connectionID string) (codeBuddyPending, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	pending, ok := c.pending[connectionID]
	delete(c.pending, connectionID)
	return pending, ok
}

// pruneLocked drops state older than a cycle plus slack, so the maps stay
// bounded by the number of connections.
func (c *CodeBuddyAutoPing) pruneLocked(now time.Time) {
	for id, state := range c.state {
		stale := now.Sub(state.touchedAt) > codeBuddyStateTTL && now.Sub(state.failedAt) > codeBuddyStateTTL
		if stale {
			delete(c.state, id)
		}
	}
}

func (c *CodeBuddyAutoPing) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *CodeBuddyAutoPing) interval() time.Duration {
	if c.Interval > 0 {
		return c.Interval
	}
	return CodeBuddyTickInterval
}

func (c *CodeBuddyAutoPing) enabled() bool {
	if c.Enabled != nil {
		return c.Enabled()
	}
	return CodeBuddyAutoPingEnabled()
}

func (c *CodeBuddyAutoPing) settings() CodeBuddySettings {
	if c.Settings == nil {
		return CodeBuddySettings{Connections: map[string]bool{}}
	}
	return c.Settings()
}

// connLabel is the connection's display name, or the id when it has none
// (upstream logs `conn.displayName || conn.name || conn.id`).
func connLabel(conn CodeBuddyConnection) string {
	if strings.TrimSpace(conn.DisplayName) != "" {
		return conn.DisplayName
	}
	return conn.ID
}
