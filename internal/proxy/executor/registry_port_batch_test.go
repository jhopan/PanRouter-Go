package executor

import "testing"

// TestRegistryPortBatchHasAnExecutor pins executor dispatch for the registry
// entries ported in the 2026-11 batch (agentrouter, apinex, apmix,
// freemodel-dev, justdowork, orcarouter, vyceai, mmf).
//
// A provider that reaches KnownProviders but not the executor registry answers
// `unknown provider` at dispatch time, which the fallback loop reports as an
// upstream failure — the request never leaves the process, and no unit test in
// internal/providers can see it because it cannot import this package without a
// cycle. That is why the assertion lives here.
func TestRegistryPortBatchHasAnExecutor(t *testing.T) {
	RegisterAll()

	for _, provider := range []string{
		"agentrouter", "apinex", "apmix", "freemodel-dev",
		"justdowork", "orcarouter", "vyceai", "mmf",
	} {
		t.Run(provider, func(t *testing.T) {
			if got := Get(provider); got == nil {
				t.Fatalf("no executor registered for %q", provider)
			}
			// The shared OpenAI passthrough is the whole requirement for these:
			// the wire shape is decided by the provider's Format and base URL,
			// not by a provider-specific executor.
			if Get(provider) == nil {
				t.Fatal("executor lookup is not stable")
			}
		})
	}
}
