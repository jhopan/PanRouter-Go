package chat

import (
	"slices"
	"strings"

	json "encoding/json/v2"
	"panrouter/proxy/internal/db"
	"panrouter/proxy/internal/log"
	"panrouter/proxy/internal/providers"
)

// Capacity Adapter — global fallback pools of models per input-modality
// capability (vision / pdf / audioInput / videoInput).
//
// The pool models are prepended as extra candidates behind whatever models were
// already going to be tried (a combo's members, or a single target model).
// ReorderByCapabilities then floats a capable pool model to the front only when
// none of the original models can handle the request — so this never overrides
// a combo that already has a member covering the capability.
//
// Port of open-sse/services/capacityAdapter.js (upstream decolua/panrouter).

// adapterCapabilityKeys is the ordered capability set a pool can be declared for.
// The order is upstream's CAPABILITY_KEYS and is load-bearing: it is the
// priority order in which pool models are flattened.
var adapterCapabilityKeys = []string{"vision", "pdf", "audioInput", "videoInput"}

// defaultAdapterFallbackModel is the model an enabled-but-empty pool resolves to,
// so a toggle with no explicit models is never a no-op (upstream
// DEFAULT_FALLBACK_MODEL).
const defaultAdapterFallbackModel = "oc/mimo-v2.6-flash-free"

// upgradeLegacyAdapterModel normalizes the retired pool model id.
func upgradeLegacyAdapterModel(m string) string {
	if m == "oc/mimo-v2.5-free" {
		return defaultAdapterFallbackModel
	}
	return m
}

// adapterEntry is a normalized capacity-adapter pool.
//
// ComboName is the newer half of upstream's entry shape: instead of listing the
// models inline, a capability can point at a combo, which then holds the models,
// their order and the fallback/round-robin strategy. Both shapes are accepted at
// once — a stored row from before the combo reference existed still works, and a
// row that carries both prefers the combo, exactly like upstream's
// normalizeCapEntry.
type adapterEntry struct {
	Enabled    bool
	RoundRobin bool
	ComboName  string
	Models     []string
}

// normalizeCapEntry coerces a stored capacity-adapter entry into
// {enabled, roundRobin, comboName, models}. Backward compat: the legacy array
// form [{model, enabled}] is accepted and treated as enabled with fallback
// strategy, and `combo` is accepted as an alias for `comboName`
// (open-sse/services/capacityAdapter.js:24-38).
func normalizeCapEntry(raw any) adapterEntry {
	switch v := raw.(type) {
	case []any:
		models := make([]string, 0, len(v))
		for _, e := range v {
			s := ""
			switch item := e.(type) {
			case string:
				s = item
			case map[string]any:
				s, _ = item["model"].(string)
			}
			if s = upgradeLegacyAdapterModel(s); s != "" {
				models = append(models, s)
			}
		}
		return adapterEntry{Enabled: true, RoundRobin: false, Models: models}
	case map[string]any:
		enabled := true
		if b, ok := v["enabled"].(bool); ok {
			enabled = b
		}
		rr := false
		if b, ok := v["roundRobin"].(bool); ok {
			rr = b
		}
		// `combo` is the documented alias. Only a string is a name; anything
		// else is a malformed row, and guessing at it would point the adapter
		// at a combo the operator never chose.
		comboName, _ := v["comboName"].(string)
		if strings.TrimSpace(comboName) == "" {
			comboName, _ = v["combo"].(string)
		}
		var models []string
		if list, ok := v["models"].([]any); ok {
			for _, m := range list {
				s, _ := m.(string)
				if s = upgradeLegacyAdapterModel(s); s != "" {
					models = append(models, s)
				}
			}
		}
		return adapterEntry{
			Enabled:    enabled,
			RoundRobin: rr,
			ComboName:  strings.TrimSpace(comboName),
			Models:     models,
		}
	default:
		return adapterEntry{}
	}
}

// capacityAdapterPools returns the raw capacityAdapter map as stored, so the
// legacy array form survives the typed SettingsData parse. A settings row that
// omits the key (never saved) falls back to the type's declared defaults —
// vision and audioInput enabled-and-empty, which resolve to the default pool
// model below — so an image request is not silently forwarded to a text-only
// model because the row predates the toggle.
func (h *ChatHandler) capacityAdapterPools() map[string]any {
	if h.Repo == nil {
		return nil
	}
	settings, err := h.Repo.GetSettingsRaw()
	if err != nil || settings == nil {
		return nil
	}
	pools, _ := settings["capacityAdapter"].(map[string]any)
	return mergeAdapterDefaults(pools)
}

// mergeAdapterDefaults substitutes the type's declared default pools only when
// the stored settings row carries no capacityAdapter map at all (the common
// case for a row saved before the toggle existed). A present-but-partial map is
// returned unchanged: an absent capability key inside it means that pool is
// unconfigured, which the raw path treats as disabled — the behaviour the
// existing suite pins. This keeps a legacy image request from being forwarded
// to a text-only model without resurrecting a pool the operator left off.
func mergeAdapterDefaults(pools map[string]any) map[string]any {
	if len(pools) != 0 {
		return pools
	}
	out := make(map[string]any, len(adapterCapabilityKeys))
	for _, cap := range adapterCapabilityKeys {
		entry := db.DefaultSettings().CapacityAdapter[cap]
		out[cap] = map[string]any{
			"enabled":    entry.Enabled,
			"roundRobin": entry.RoundRobin,
			"comboName":  "",
			"models":     entry.Models,
		}
	}
	return out
}

// adapterConfig resolves one capability's full config. A combo reference is
// expanded to the combo's own models first, because the combo is where the
// operator put them — upstream's getCapacityAdapterConfig does the same and
// keeps the inline list only when the combo cannot be read. An enabled pool that
// still has no models falls back to defaultAdapterFallbackModel so the toggle is
// never a no-op; a DISABLED pool stays empty, which is what keeps a turned-off
// adapter from injecting anything.
func (h *ChatHandler) adapterConfig(cap string, pools map[string]any) adapterEntry {
	entry := normalizeCapEntry(pools[cap])
	if models := h.adapterComboModels(entry.ComboName); len(models) > 0 {
		entry.Models = models
	}
	if entry.Enabled && len(entry.Models) == 0 {
		entry.Models = []string{defaultAdapterFallbackModel}
	}
	return entry
}

// adapterComboModels expands a capability's combo reference into concrete
// leaves. A missing combo, an unrunnable one, or a handler without a repository
// yields nothing, and the caller keeps whatever the entry carried inline — the
// same "combo if it reads, otherwise the pool" precedence upstream applies.
func (h *ChatHandler) adapterComboModels(name string) []string {
	if name == "" || h.Repo == nil {
		return nil
	}
	combo, err := h.Repo.GetComboByName(name)
	if err != nil || combo == nil || combo.Models == "" {
		return nil
	}
	var leaves []string
	if err := json.Unmarshal([]byte(combo.Models), &leaves); err != nil || len(leaves) == 0 {
		return nil
	}
	flattened, err := h.flattenComboModels(leaves)
	if err != nil {
		return nil
	}
	models := make([]string, 0, len(flattened))
	for _, m := range flattened {
		if m = upgradeLegacyAdapterModel(m); m != "" {
			models = append(models, m)
		}
	}
	return models
}

// adapterModels flattens enabled models across all capability pools, in
// priority order, deduped.
func (h *ChatHandler) adapterModels(pools map[string]any) []string {
	var models []string
	for _, cap := range adapterCapabilityKeys {
		entry := h.adapterConfig(cap, pools)
		if !entry.Enabled {
			continue
		}
		for _, m := range entry.Models {
			if !slices.Contains(models, m) {
				models = append(models, m)
			}
		}
	}
	return models
}

// adapterStrategyFor returns the strategy of a single capability's pool.
func adapterStrategyFor(entry adapterEntry) string {
	if entry.Enabled && entry.RoundRobin {
		return "round-robin"
	}
	return "fallback"
}

// activeAdapterStrategy picks the strategy from the request's required
// capabilities: the first capability whose pool is enabled and can satisfy a
// hard requirement.
func (h *ChatHandler) activeAdapterStrategy(required map[string]bool, pools map[string]any) string {
	for _, cap := range adapterCapabilityKeys {
		if !required[cap] {
			continue
		}
		entry := h.adapterConfig(cap, pools)
		if !entry.Enabled || len(entry.Models) == 0 {
			continue
		}
		return adapterStrategyFor(entry)
	}
	return "fallback"
}

// modelSatisfiesHard reports whether a "provider/model" entry covers every
// required hard capability.
func modelSatisfiesHard(modelEntry string, hardRequired []string) bool {
	if !strings.Contains(modelEntry, "/") {
		// A combo name carries no capability of its own. Upstream's
		// modelSatisfies splits on "/" the same way, so a combo entry never
		// satisfies a hard cap and the pool is consulted instead.
		return false
	}
	provider, model, _ := strings.Cut(modelEntry, "/")
	caps := providers.GetCapabilitiesForModel(provider, model)
	for _, c := range hardRequired {
		if !capabilityFlag(caps, c) {
			return false
		}
	}
	return true
}

// capabilityFlag reads one capability off a Capabilities block by name.
func capabilityFlag(caps providers.Capabilities, cap string) bool {
	switch cap {
	case "vision":
		return caps.Vision
	case "pdf":
		return caps.PDF
	case "audioInput":
		return caps.AudioInput
	case "videoInput":
		return caps.VideoInput
	case "imageOutput":
		return caps.ImageOutput
	case "audioOutput":
		return caps.AudioOutput
	case "search":
		return caps.Search
	case "tools":
		return caps.Tools
	case "reasoning":
		return caps.Reasoning
	default:
		return false
	}
}

// AugmentModelsWithCapacityAdapter prepends capacity-adapter pool models when
// NONE of the original models can satisfy the request's required hard
// capabilities. The original list is returned untouched when it already covers
// them (ReorderByCapabilities handles that case), when no pool is enabled, or
// when no pool member is itself capable.
//
// A pool model the calling key may not dispatch is never injected. The gate
// that admitted the request only covers the model the CLIENT named, and this
// pool is invisible to it — without the filter below an operator restricting a
// key to one model would still have its traffic silently rerouted to a pool
// model it never allowed. keyID may be empty, which means "no allowlist".
func (h *ChatHandler) AugmentModelsWithCapacityAdapter(models []string, required map[string]bool, keyID string) ([]string, string) {
	return h.augmentModelsWithCapacityAdapter(models, required, keyID)
}

func (h *ChatHandler) augmentModelsWithCapacityAdapter(models []string, required map[string]bool, keyID string) ([]string, string) {
	hard := requiredHardCaps(required)
	if len(hard) == 0 || len(models) == 0 {
		return models, "fallback"
	}

	for _, m := range models {
		if modelSatisfiesHard(m, hard) {
			return models, "fallback"
		}
	}

	pools := h.capacityAdapterPools()
	pool := h.adapterModels(pools)
	patterns := h.allowedModelPatterns(keyID)
	injected := make([]string, 0, len(pool))
	for _, m := range pool {
		if slices.Contains(models, m) || !modelSatisfiesHard(m, hard) {
			continue
		}
		if !modelAllowed(patterns, h.accessCandidates(m)...) {
			log.Info("chat", "capacity adapter skipped, model not permitted for this api key", "model", m)
			continue
		}
		injected = append(injected, m)
	}
	if len(injected) == 0 {
		return models, "fallback"
	}

	log.Info("chat", "capacity adapter augmented", "caps", keysString(required), "pool", strings.Join(injected, ","))
	return append(injected, models...), h.activeAdapterStrategy(required, pools)
}

// requiredHardCaps filters the detected capabilities down to the input-modality
// ones that trigger the adapter pool.
func requiredHardCaps(required map[string]bool) []string {
	var hard []string
	for _, cap := range adapterCapabilityKeys {
		if required[cap] {
			hard = append(hard, cap)
		}
	}
	return hard
}
