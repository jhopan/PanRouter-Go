package pricing

// Rate is one model's rate table in the shape the dashboard's pricing view
// reads. The JSON keys are upstream's (open-sse/providers/pricing.js): rates are
// $/1M tokens, named bare — `input`, not `input_per_1m`.
//
// Upstream's source rows are partial: a model that has no published cache rate
// simply omits `cached`/`cache_creation` from its object literal. The Go table
// cannot tell "explicitly zero" from "not written", so every rate is emitted.
// A client reading `cached: 0` and a client reading an absent `cached` both
// price the same request, and inventing an omission table would be a second
// copy of the rates that can drift from the first.
type Rate struct {
	Input         float64 `json:"input"`
	Output        float64 `json:"output"`
	Cached        float64 `json:"cached"`
	Reasoning     float64 `json:"reasoning"`
	CacheCreation float64 `json:"cache_creation"`
}

// DefaultPricing returns the provider-specific rate overrides keyed
// provider → model → rate, which is what upstream GET /api/pricing answers.
//
// The canonical, provider-agnostic MODEL_PRICING table is NOT part of that
// payload: upstream's getPricing() merges PROVIDER_PRICING with the user's
// stored overrides, and falls back to MODEL_PRICING only when resolving a
// single model. Reproducing the same split here keeps the response a client can
// compare against upstream — the dashboard's pricing editor lists exactly these
// provider rows.
//
// The returned map is a copy: a caller that edits a rate cannot reach into the
// process-wide table.
func DefaultPricing() map[string]map[string]Rate {
	out := make(map[string]map[string]Rate, len(providerPricing))
	for provider, models := range providerPricing {
		rates := make(map[string]Rate, len(models))
		for model, p := range models {
			rates[model] = Rate{
				Input:         p.InputPer1M,
				Output:        p.OutputPer1M,
				Cached:        p.CachedPer1M,
				Reasoning:     p.ReasoningPer1M,
				CacheCreation: p.CacheCreationPer1M,
			}
		}
		out[provider] = rates
	}
	return out
}
