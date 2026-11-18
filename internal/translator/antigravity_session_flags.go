package translator

import (
	"strings"
	"sync"
	"time"
)

// The `used_*` labels a shipped agent turn carries are CONVERSATION state, not a
// description of the current model. Captured from the live client on 2026-11-18
// (one cascade, seven turns): the first turn ran claude-sonnet-4-6 and carried
// used_claude=true / used_claude_conservative=true / used_non_gemini_model=true;
// every later turn — including three that ran a Gemini model — carried exactly
// the same three values.
//
// This port derived them from the model of the request, so a Claude turn claimed
// used_claude_conservative=false (a combination the client never emits) and a
// Gemini turn in a Claude conversation claimed all three false. The flags are
// what tells the backend which weekly bucket a trajectory belongs to
// (gemini-weekly vs 3p-weekly in retrieveUserQuotaSummary), so they are worth
// reproducing rather than approximating.
const (
	agSessionFlagTTL      = 6 * time.Hour
	agSessionFlagMaxSlots = 4096
)

type agSessionFlags struct {
	usedClaude    bool
	usedNonGemini bool
	expires       time.Time
}

var (
	agSessionFlagMu   sync.Mutex
	agSessionFlagByID = make(map[string]agSessionFlags)
)

// antigravityObserveModelFamily records the model family this conversation has
// used and returns the sticky flags its labels bag carries.
//
// The state is bounded twice: a slot expires after agSessionFlagTTL, and the map
// never exceeds agSessionFlagMaxSlots. A conversation arriving while the map is
// full and has nothing expired falls back to the per-request flags rather than
// growing — the labels degrade, the request does not.
func antigravityObserveModelFamily(conversationID, model string) agSessionFlags {
	lower := strings.ToLower(model)
	seen := agSessionFlags{
		usedClaude:    strings.HasPrefix(lower, "claude"),
		usedNonGemini: !strings.HasPrefix(lower, "gemini"),
	}
	if conversationID == "" {
		return seen
	}

	now := time.Now()
	agSessionFlagMu.Lock()
	defer agSessionFlagMu.Unlock()

	flags, ok := agSessionFlagByID[conversationID]
	if ok && flags.expires.After(now) {
		seen.usedClaude = seen.usedClaude || flags.usedClaude
		seen.usedNonGemini = seen.usedNonGemini || flags.usedNonGemini
	}

	entry := agSessionFlags{
		usedClaude:    seen.usedClaude,
		usedNonGemini: seen.usedNonGemini,
		expires:       now.Add(agSessionFlagTTL),
	}
	if !ok && len(agSessionFlagByID) >= agSessionFlagMaxSlots {
		agSessionFlagEvictExpired(now)
	}
	if _, present := agSessionFlagByID[conversationID]; present || len(agSessionFlagByID) < agSessionFlagMaxSlots {
		agSessionFlagByID[conversationID] = entry
	}
	return seen
}

// agSessionFlagEvictExpired drops every lapsed slot. Callers hold the lock.
func agSessionFlagEvictExpired(now time.Time) {
	for id, flags := range agSessionFlagByID {
		if !flags.expires.After(now) {
			delete(agSessionFlagByID, id)
		}
	}
}

// ResetAntigravitySessionLabels clears the per-conversation family state. It
// exists for tests, which must not observe a conversation another test opened —
// the same reason ClearAntigravityQuotaCache exists on the quota cache.
func ResetAntigravitySessionLabels() {
	agSessionFlagMu.Lock()
	defer agSessionFlagMu.Unlock()
	agSessionFlagByID = make(map[string]agSessionFlags)
}
