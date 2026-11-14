package proxy

import (
	"os"
	"strconv"
	"time"

	"panrouter/proxy/internal/envx"
	"panrouter/proxy/internal/providers"
)

// DefaultStallTimeout is the default inter-chunk budget: how long a stream that
// has already produced data may stay silent before it is considered dead.
// Matches Next.js STREAM_STALL_TIMEOUT_MS = 360000 (6 minutes).
const DefaultStallTimeout = 6 * time.Minute

// DefaultFirstChunkTimeout is the default budget for the FIRST chunk — the
// provider's prompt prefill. It is a separate budget because the two phases
// measure different things: on a 120k-token context a prefill of 27-30 seconds
// is normal and minutes happen, while the same silence once tokens are flowing
// means the stream is gone. Charging prefill against the inter-chunk budget is
// what aborted healthy MiniMax turns. Matches upstream
// STREAM_FIRST_CHUNK_TIMEOUT_MS = 200000 (3m20s).
const DefaultFirstChunkTimeout = 200 * time.Second

// Stall phases, named the way upstream names them so a log line reads the same
// on both sides.
const (
	stallPhaseFirstChunk      = "no first chunk"
	stallPhaseAfterFirstChunk = "stalled after first chunk"
)

// StallBudgets are the two budgets one stream watchdog runs on.
type StallBudgets struct {
	// FirstChunk caps the wait before any byte of the response body arrives.
	FirstChunk time.Duration
	// InterChunk caps the silence between bytes once the stream has started.
	InterChunk time.Duration
}

// DefaultStallBudgets resolves both budgets from the environment and falls back
// to the compiled defaults.
func DefaultStallBudgets() StallBudgets {
	return StallBudgets{
		FirstChunk: ResolveDefaultFirstChunkTimeout(),
		InterChunk: ResolveDefaultStallTimeout(),
	}
}

// ResolveStallBudgets applies a provider's declared budgets to the defaults.
// Every entry a registry leaves unset keeps the default for that phase, so a
// provider that declares only the inter-chunk timeout (qoder, the older
// entries) is not silently given the same value for prefill. A declared value
// wins over the environment, which is how upstream resolves it
// (`PROVIDERS[provider]?.stallTimeoutMs || STREAM_STALL_TIMEOUT_MS`).
func ResolveStallBudgets(cfg *providers.ProviderConfig) StallBudgets {
	budgets := DefaultStallBudgets()
	if cfg == nil {
		return budgets
	}
	if cfg.FirstChunkTimeoutMs > 0 {
		budgets.FirstChunk = time.Duration(cfg.FirstChunkTimeoutMs) * time.Millisecond
	}
	if cfg.StallTimeoutMs > 0 {
		budgets.InterChunk = time.Duration(cfg.StallTimeoutMs) * time.Millisecond
	}
	return budgets
}

// ResolveDefaultStallTimeout reads STREAM_STALL_TIMEOUT_MS and
// PANROUTER_STREAM_STALL_TIMEOUT_MS. A positive integer is milliseconds; an
// unset, non-numeric or non-positive value falls back to DefaultStallTimeout.
func ResolveDefaultStallTimeout() time.Duration {
	return envTimeout("STREAM_STALL_TIMEOUT_MS", DefaultStallTimeout)
}

// ResolveDefaultFirstChunkTimeout reads STREAM_FIRST_CHUNK_TIMEOUT_MS, with the
// same units and fallback rules as ResolveDefaultStallTimeout.
func ResolveDefaultFirstChunkTimeout() time.Duration {
	return envTimeout("STREAM_FIRST_CHUNK_TIMEOUT_MS", DefaultFirstChunkTimeout)
}

func envTimeout(name string, fallback time.Duration) time.Duration {
	for _, envVar := range append([]string{name}, envx.Names(name)...) {
		if val := os.Getenv(envVar); val != "" {
			if ms, err := strconv.Atoi(val); err == nil && ms > 0 {
				return time.Duration(ms) * time.Millisecond
			}
		}
	}
	return fallback
}
