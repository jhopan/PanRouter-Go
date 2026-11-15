// CodeBuddy conversation and tracing identity — port of the id builders in
// open-sse/executors/codebuddy-intl.js (uuidV1 / stableConversationId /
// perTurnId).
//
// The shape is part of the fingerprint. The official CLI's conversation id is a
// TIME-BASED uuid (version 1): it encodes when the conversation was opened and
// which machine opened it. A content-hash id (v5) or a random one (v4) carries
// neither, and the gateway is reading the pattern, not the value.
//
// The timestamp has to be pinned to the moment the conversation was first seen —
// not "now" on every request — or the id would change on every turn and stop
// being a conversation id at all.
package executor

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

const (
	// codebuddyUUIDv1Node is the constant node id the CLI's ids carry
	// (42:66:14:17:40:00), from the captured client.
	codebuddyUUIDv1Node = "426614174000"
	// codebuddyConversationMaxEntries bounds the pinned-timestamp table: a
	// long-lived gateway sees many conversations, and an unbounded map keyed by a
	// client-supplied value is a memory leak with a schedule.
	codebuddyConversationMaxEntries = 2000
)

// codebuddyConversationStarts pins the moment each conversation was first seen.
var (
	codebuddyConversationMu     sync.Mutex
	codebuddyConversationStarts = map[string]time.Time{}
)

// codebuddyUUIDv1 builds a version-1 (time-based) uuid for seed at at, in the
// layout the CLI emits: a 60-bit timestamp split across time_low/time_mid/
// time_high, a clock sequence derived from the seed's md5 (so the same
// conversation keeps the same id across retries and account failovers), and the
// constant node id above.
func codebuddyUUIDv1(seed string, at time.Time) string {
	// 100-ns intervals since 1582-10-15 (the uuid v1 epoch).
	ticks := uint64(at.UnixMilli())*10000 + 0x01b21dd213814000
	timeLow := uint32(ticks & 0xFFFFFFFF)
	timeMid := uint16((ticks >> 32) & 0xFFFF)
	timeHigh := uint16((ticks>>48)&0x0FFF) | 0x1000 // version 1
	seedHash := md5.Sum([]byte(seed))
	clockSeq := (uint16(seedHash[0])<<8 | uint16(seedHash[1]))&0x3FFF | 0x8000
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%s", timeLow, timeMid, timeHigh, clockSeq, codebuddyUUIDv1Node)
}

// codebuddyPerTurnID is the id for one turn: fresh every request, like the CLI's
// message id, and reused for the request id on the same turn.
func codebuddyPerTurnID() string {
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return codebuddyUUIDv1(fmt.Sprintf("turn-%d", time.Now().UnixNano()), time.Now())
	}
	return codebuddyUUIDv1(hex.EncodeToString(entropy[:]), time.Now())
}

// codebuddyStableConversationID returns the conversation id for key, pinning the
// timestamp the first time the key is seen so later turns of the same
// conversation re-send the same id.
func codebuddyStableConversationID(key string) string {
	codebuddyConversationMu.Lock()
	pinned, ok := codebuddyConversationStarts[key]
	if !ok {
		pinned = time.Now()
		if len(codebuddyConversationStarts) >= codebuddyConversationMaxEntries {
			// Best-effort eviction: the oldest pinned start goes, so the table
			// cannot grow without bound.
			var oldestKey string
			var oldest time.Time
			for candidate, started := range codebuddyConversationStarts {
				if oldestKey == "" || started.Before(oldest) {
					oldestKey, oldest = candidate, started
				}
			}
			delete(codebuddyConversationStarts, oldestKey)
		}
		codebuddyConversationStarts[key] = pinned
	}
	codebuddyConversationMu.Unlock()
	return codebuddyUUIDv1(key, pinned)
}

// codebuddyResetConversationStarts forgets every pinned conversation start
// (tests, and a settings change that reconnects the accounts).
func codebuddyResetConversationStarts() {
	codebuddyConversationMu.Lock()
	defer codebuddyConversationMu.Unlock()
	codebuddyConversationStarts = map[string]time.Time{}
}