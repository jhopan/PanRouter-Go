// Kiro session replay — port of open-sse/utils/kiroSessionReplay.js.
//
// Kiro's cache is keyed on the conversation prefix, so a session that re-sends a
// slightly different first user turn every request pays full prefill every time.
// The volatile part is the current-time context the translator injects; the stable
// part is the system material (thinking prefix, agentic protocol).
//
// The fix is to freeze the first user message for a session — including the time
// it was built with — replay that exact message on every later turn, and inject
// the fresh time context only into the CURRENT turn. The prefix stays byte-identical
// across turns, so the upstream cache hits.
//
// The store is bounded: one entry per (connection, conversation), oldest evicted,
// and an entry that has not been used for a TTL is dropped, so a long-running
// gateway cannot accumulate sessions without limit.
package translator

import (
	"sync"
	"time"
)

const (
	// kiroSessionStartMaxEntries is the ceiling on replayed sessions. Beyond it the
	// oldest entry is evicted — an unbounded map keyed by a client-supplied value
	// is a memory leak with a schedule.
	kiroSessionStartMaxEntries = 5000
	// kiroSessionStartTTL drops a session nobody has continued for a day.
	kiroSessionStartTTL = 24 * time.Hour
)

// kiroSessionStart is the frozen opening turn of one session.
type kiroSessionStart struct {
	message     map[string]any
	modelID     string
	systemPart  string
	lastUsed    time.Time
}

var (
	kiroSessionMu     sync.Mutex
	kiroSessionStarts = map[string]kiroSessionStart{}
)

// ClearKiroSessionReplayStore forgets every frozen session (tests, and a settings
// change that reconnects the accounts).
func ClearKiroSessionReplayStore() {
	kiroSessionMu.Lock()
	defer kiroSessionMu.Unlock()
	kiroSessionStarts = map[string]kiroSessionStart{}
}

// kiroSessionKey names one session.
func kiroSessionKey(connectionID, conversationID string) string {
	return connectionID + ":" + conversationID
}

// kiroApplySessionReplay freezes the opening turn of a session and replays it on
// later turns.
//
// systemPart is the stable system material, timePart the volatile context. On a
// fresh session both ride on the first user message; on a replayed one only the
// current message gets the new time, so the frozen prefix stays identical.
func kiroApplySessionReplay(conversationID, connectionID, modelID, systemPart, timePart string, history []map[string]any, current map[string]any) (outHistory []map[string]any, outCurrent map[string]any) {
	baseHistory := kiroCloneTurns(history)
	baseCurrent := kiroCloneMap(current)
	contentPrefix := kiroJoinNonEmpty(systemPart, timePart)

	key := kiroSessionKey(connectionID, conversationID)
	now := time.Now().UTC()

	kiroSessionMu.Lock()
	existing, ok := kiroSessionStarts[key]
	if ok && (conversationID == "" || existing.modelID != modelID || existing.systemPart != systemPart) {
		ok = false
	}
	if ok && now.Sub(existing.lastUsed) > kiroSessionStartTTL {
		delete(kiroSessionStarts, key)
		ok = false
	}
	if ok {
		existing.lastUsed = now
		kiroSessionStarts[key] = existing
	}
	kiroSessionMu.Unlock()

	if ok {
		start := kiroCloneMap(existing.message)
		kiroEnsureUserModelID(start, modelID)
		firstUser := kiroFirstUserIndex(baseHistory)
		if kiroCanReplaceSessionStart(baseHistory, firstUser) {
			baseHistory[firstUser] = start
		} else {
			baseHistory = append([]map[string]any{start}, baseHistory...)
			if len(baseHistory) == 1 {
				baseHistory = append(baseHistory, map[string]any{
					"assistantResponseMessage": map[string]any{"content": kiroAssistantPlaceholder},
				})
			}
		}
		return kiroEnsureHistoryModelIDs(baseHistory, modelID), kiroPrefixUserMessage(baseCurrent, timePart, modelID)
	}

	firstUser := kiroFirstUserIndex(baseHistory)
	var start map[string]any
	nextCurrent := kiroEnsureUserModelID(baseCurrent, modelID)
	switch {
	case kiroCanReplaceSessionStart(baseHistory, firstUser):
		start = kiroPrefixUserMessage(baseHistory[firstUser], contentPrefix, modelID)
		baseHistory[firstUser] = kiroCloneMap(start)
		nextCurrent = kiroPrefixUserMessage(baseCurrent, timePart, modelID)
	case firstUser >= 0:
		// The opening turn already carries tool results, so it cannot hold the
		// system material: a synthetic user turn goes in front of it.
		start = kiroPrefixUserMessage(map[string]any{
			"userInputMessage": map[string]any{"content": "", "modelId": modelID},
		}, contentPrefix, modelID)
		baseHistory = append([]map[string]any{kiroCloneMap(start)}, baseHistory...)
		nextCurrent = kiroPrefixUserMessage(baseCurrent, timePart, modelID)
	default:
		start = kiroPrefixUserMessage(baseCurrent, contentPrefix, modelID)
		nextCurrent = kiroCloneMap(start)
	}

	if conversationID != "" {
		kiroRememberSessionStart(key, kiroSessionStart{
			message:    kiroCloneMap(start),
			modelID:    modelID,
			systemPart: systemPart,
			lastUsed:   now,
		})
	}
	return kiroEnsureHistoryModelIDs(baseHistory, modelID), nextCurrent
}

// kiroRememberSessionStart stores one entry, evicting the oldest when full.
func kiroRememberSessionStart(key string, entry kiroSessionStart) {
	kiroSessionMu.Lock()
	defer kiroSessionMu.Unlock()
	if len(kiroSessionStarts) >= kiroSessionStartMaxEntries {
		var oldestKey string
		var oldest time.Time
		for candidate, value := range kiroSessionStarts {
			if oldestKey == "" || value.lastUsed.Before(oldest) {
				oldestKey, oldest = candidate, value.lastUsed
			}
		}
		delete(kiroSessionStarts, oldestKey)
	}
	kiroSessionStarts[key] = entry
}

// kiroCloneTurns deep-copies a history slice.
func kiroCloneTurns(turns []map[string]any) []map[string]any {
	out := make([]map[string]any, len(turns))
	for i, turn := range turns {
		out[i] = kiroCloneMap(turn)
	}
	return out
}

// kiroFirstUserIndex finds the opening user turn, or -1.
func kiroFirstUserIndex(history []map[string]any) int {
	for i, turn := range history {
		if _, ok := turn["userInputMessage"]; ok {
			return i
		}
	}
	return -1
}

// kiroCanReplaceSessionStart reports whether the frozen message can take the
// opening turn's place without losing tool results.
func kiroCanReplaceSessionStart(history []map[string]any, index int) bool {
	if index != 0 {
		return false
	}
	user, _ := history[index]["userInputMessage"].(map[string]any)
	return len(kiroToolResults(user)) == 0
}

// kiroEnsureUserModelID fills modelId on a turn that lacks it.
func kiroEnsureUserModelID(turn map[string]any, modelID string) map[string]any {
	if user, ok := turn["userInputMessage"].(map[string]any); ok {
		if s, _ := user["modelId"].(string); s == "" && modelID != "" {
			user["modelId"] = modelID
		}
	}
	return turn
}

// kiroEnsureHistoryModelIDs does the same for every turn in a history slice.
func kiroEnsureHistoryModelIDs(history []map[string]any, modelID string) []map[string]any {
	for _, turn := range history {
		kiroEnsureUserModelID(turn, modelID)
	}
	return history
}

// kiroPrefixUserMessage copies a turn and prepends the content prefix.
func kiroPrefixUserMessage(turn map[string]any, prefix, modelID string) map[string]any {
	out := kiroCloneMap(turn)
	if out == nil {
		out = map[string]any{}
	}
	user, _ := out["userInputMessage"].(map[string]any)
	if user == nil {
		user = map[string]any{"content": ""}
		out["userInputMessage"] = user
	}
	kiroEnsureUserModelID(out, modelID)
	if prefix == "" {
		return out
	}
	existing, _ := user["content"].(string)
	if existing == "" {
		user["content"] = prefix
		return out
	}
	user["content"] = prefix + "\n\n" + existing
	return out
}

// kiroJoinNonEmpty joins the parts that are present with a blank line.
func kiroJoinNonEmpty(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			out = append(out, part)
		}
	}
	return joinKiroParts(out)
}

// joinKiroParts is strings.Join with the Kiro separator, kept local so the
// session file has no import cycle with the translator's own helpers.
func joinKiroParts(parts []string) string {
	joined := ""
	for i, part := range parts {
		if i > 0 {
			joined += "\n\n"
		}
		joined += part
	}
	return joined
}