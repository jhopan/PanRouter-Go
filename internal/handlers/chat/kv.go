// KV wiring shared by the per-provider caches that must survive a restart.
//
// Antigravity's exhausted buckets and eligibility flag, and Kiro's monthly cap,
// all persist into the repository's `kv` table under their own scope. The store
// is one process-wide dependency, so it is wired once — a second wiring function
// per provider would let one of them run unwired and silently stop persisting.
//
// A nil store is the fail-open path everywhere it is read: nothing is written or
// restored, and no request notices. Losing a persisted cap costs a re-discovery,
// never a refusal.
package chat

import "sync"

// KVStore is the key-value surface the provider caches persist through.
// *db.Repo satisfies it; tests inject a fake.
//
// The methods carry no context because the repository's KV API has none — a
// per-key upsert on local SQLite, not a caller-cancellable round trip.
type KVStore interface {
	GetKVScope(scope string) (map[string]string, error)
	SetKV(scope, key, value string) error
	DeleteKV(scope, key string) error
}

var (
	kvMu      sync.RWMutex
	kvBacking KVStore
)

// SetKVStore wires the store the provider caches persist through. Called once at
// startup; tests point it at a fake and reset it afterwards.
func SetKVStore(kv KVStore) {
	kvMu.Lock()
	defer kvMu.Unlock()
	kvBacking = kv
}

// kvStore reads the wired store, or nil when none was set.
func kvStore() KVStore {
	kvMu.RLock()
	defer kvMu.RUnlock()
	return kvBacking
}
