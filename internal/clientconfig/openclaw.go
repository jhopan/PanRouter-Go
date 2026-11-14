package clientconfig

import "strings"

// OpenClaw: ~/.openclaw/openclaw.json plus a models.json under each agent's own
// agentDir.
//
// src/app/api/cli-tools/openclaw-settings/route.js registers our provider under
// models.providers["9router"], selects 9router/<model> as the default in
// agents.defaults.model.primary, allowlists the model ids in
// agents.defaults.models, strips any old 9router model from agents.list, and
// writes the provider block into every agent directory it has.
//
// The settings object is the user's: only the three subtrees the route owns are
// replaced, and every other key is left to the splice. agents.list is an array,
// which a key path cannot address, so it is rewritten whole when (and only
// when) one of its entries still names us.

const (
	openClawProviderKey  = "9router"
	openClawFallbackKey  = "your_api_key"
	openClawAPIBackend   = "openai-completions"
	openClawModelPrefix  = "9router/"
	openClawAgentFile    = "models.json"
	openClawAgentPurpose = "agent-models"
)

type openClawAdapter struct{ baseAdapter }

func newOpenClawAdapter() Adapter {
	return openClawAdapter{baseAdapter{id: "openclaw", paths: openClawPaths}}
}

func (a openClawAdapter) Plan(opts Options) ([]FilePlan, error) {
	paths, err := a.Paths(opts)
	if err != nil {
		return nil, err
	}
	settingsFile := paths.Files[0]
	old, err := readClientFile(settingsFile)
	if err != nil {
		return nil, err
	}
	settings, err := parseOpenClawSettings(old)
	if err != nil {
		return nil, err
	}
	models := cliModels(opts)
	if len(models) == 0 {
		// The route requires both fields: it selects 9router/<model> and
		// allowlists it, so there is nothing sensible to write without one.
		return nil, errModelRequired("openclaw", settingsFile)
	}
	key := cliAPIKey(opts.APIKey)
	if key == "" {
		key = openClawFallbackKey
	}

	providerJSON, err := openClawProviderJSON(withV1(opts.BaseURL), key, models)
	if err != nil {
		return nil, err
	}
	allowJSON, err := openClawAllowlistJSON(openClawAllowlist(settings, models))
	if err != nil {
		return nil, err
	}
	edits := []Edit{
		{
			KeyPath:         []string{"models", "providers", openClawProviderKey},
			ValueJSON:       providerJSON,
			CreateIfMissing: true,
		},
		{
			KeyPath:         []string{"agents", "defaults", "model", "primary"},
			Value:           openClawModelPrefix + models[0],
			CreateIfMissing: true,
		},
		{
			KeyPath:         []string{"agents", "defaults", "models"},
			ValueJSON:       allowJSON,
			CreateIfMissing: true,
		},
	}
	plans := []FilePlan{{File: settingsFile, Edits: edits}}

	agents := openClawRewriteAgents(settings)
	if agents != nil {
		// Our default is agents.defaults.model.primary, so an agent that used
		// to name a 9router model drops the field and inherits it. Rewriting the
		// array only when one of its entries still names us keeps a file we do
		// not own byte-identical.
		if agents.changed {
			listJSON, err := agents.json()
			if err != nil {
				return nil, err
			}
			plans[0].Edits = append(plans[0].Edits, Edit{
				KeyPath:         []string{"agents", "list"},
				ValueJSON:       listJSON,
				CreateIfMissing: true,
			})
		}
		agentPlans, err := openClawAgentPlans(agents.dirs, withV1(opts.BaseURL), key, models[0])
		if err != nil {
			// A per-agent models.json this package cannot read is reported
			// rather than silently left stale: the settings file already names
			// the provider, so a half-applied client is worse than a refusal.
			return nil, err
		}
		plans = append(plans, agentPlans...)
	}
	return plans, nil
}

func (a openClawAdapter) Apply(opts Options) (Result, error) {
	plans, err := a.Plan(opts)
	if err != nil {
		return Result{}, err
	}
	return applyPlans(plans, nil)
}

func (a openClawAdapter) Reset(opts Options) (Result, error) {
	paths, err := a.Paths(opts)
	if err != nil {
		return Result{}, err
	}
	settingsFile := paths.Files[0]
	old, err := readClientFile(settingsFile)
	if err != nil {
		return Result{}, err
	}
	settings, err := parseOpenClawSettings(old)
	if err != nil {
		return Result{}, err
	}
	if len(old) == 0 {
		return Result{}, nil
	}
	edits := []Edit{{KeyPath: []string{"models", "providers", openClawProviderKey}, Delete: true}}
	// agents.defaults.models is the route's allowlist: our entries go, the
	// user's stay. Upstream drops the map entirely once it is empty.
	allow := openClawMap(settings, "agents", "defaults", "models")
	if openClawHasOurModels(allow) {
		kept := openClawWithoutOurModels(allow)
		if len(kept) == 0 {
			edits = append(edits, Edit{KeyPath: []string{"agents", "defaults", "models"}, Delete: true})
		} else {
			keptJSON, err := openClawAllowlistJSON(kept)
			if err != nil {
				return Result{}, err
			}
			edits = append(edits, Edit{
				KeyPath:         []string{"agents", "defaults", "models"},
				ValueJSON:       keptJSON,
				CreateIfMissing: true,
			})
		}
	}
	if primary := openClawPrimary(settings); strings.HasPrefix(primary, openClawModelPrefix) {
		edits = append(edits, Edit{KeyPath: []string{"agents", "defaults", "model", "primary"}, Delete: true})
	}
	if agents := openClawRewriteAgents(settings); agents != nil && agents.changed {
		listJSON, err := agents.json()
		if err != nil {
			return Result{}, err
		}
		edits = append(edits, Edit{
			KeyPath:         []string{"agents", "list"},
			ValueJSON:       listJSON,
			CreateIfMissing: true,
		})
	}
	return applyPlans([]FilePlan{{File: settingsFile, Edits: edits}}, nil)
}

// openClawSettings is the decoded settings document. Only the keys the route
// owns are typed; everything else stays as decoded, so a rewrite of the whole
// list cannot drop a field this package does not know about.
type openClawSettings = map[string]any

func parseOpenClawSettings(b []byte) (openClawSettings, error) {
	out := openClawSettings{}
	if len(b) == 0 {
		return out, nil
	}
	if err := parseClientJSON(b, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = openClawSettings{}
	}
	return out, nil
}

// openClawMap walks nested maps, returning an empty map when any step is
// missing or holds something else.
func openClawMap(root map[string]any, path ...string) map[string]any {
	cur := root
	for _, key := range path {
		next, ok := cur[key].(map[string]any)
		if !ok {
			return map[string]any{}
		}
		cur = next
	}
	return cur
}

func openClawPrimary(settings openClawSettings) string {
	v, _ := openClawMap(settings, "agents", "defaults", "model")["primary"].(string)
	return v
}
