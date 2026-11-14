package clientconfig

import (
	"path/filepath"
	"strings"
)

// The settings subtrees openclaw.json owns: the provider block, the model
// allowlist, and the per-agent models.json files. Split from openclaw.go, which
// decides whether and when to write them.

// openClawHasOurModels reports whether the allowlist already names a 9router/*
// entry.
func openClawHasOurModels(allow map[string]any) bool {
	for key := range allow {
		if strings.HasPrefix(key, openClawModelPrefix) {
			return true
		}
	}
	return false
}

func openClawWithoutOurModels(allow map[string]any) map[string]any {
	out := make(map[string]any, len(allow))
	for key, value := range allow {
		if strings.HasPrefix(key, openClawModelPrefix) {
			continue
		}
		out[key] = value
	}
	return out
}

// openClawAllowlist is the old allowlist minus our entries, plus one entry per
// model the request names. An existing blank entry for the same id is reused so
// the value a user put there is not replaced by an empty object.
func openClawAllowlist(settings openClawSettings, models []string) map[string]any {
	kept := openClawWithoutOurModels(openClawMap(settings, "agents", "defaults", "models"))
	for _, m := range models {
		kept[openClawModelPrefix+m] = map[string]any{}
	}
	return kept
}

func openClawAllowlistJSON(allow map[string]any) (string, error) {
	return jsonInline(allow)
}

// openClawProviderJSON renders models.providers["9router"].
func openClawProviderJSON(baseURL, key string, models []string) (string, error) {
	entries := make([]map[string]any, 0, len(models))
	for _, m := range models {
		entries = append(entries, map[string]any{"id": m, "name": lastModelSegment(m)})
	}
	return jsonInline(map[string]any{
		"baseUrl": baseURL,
		"apiKey":  key,
		"api":     openClawAPIBackend,
		"models":  entries,
	})
}

// openClawAgents is the rewritten agents.list plus the directories whose
// models.json has to be written.
type openClawAgents struct {
	list    []any
	dirs    []string
	changed bool
}

// json renders the list for a ValueJSON edit.
func (a openClawAgents) json() (string, error) { return jsonInline(a.list) }

// openClawRewriteAgents strips our model from every entry and collects the
// agentDir values that need a models.json. A settings file with no list is not
// an error: the route only enriches a list that exists.
func openClawRewriteAgents(settings openClawSettings) *openClawAgents {
	agents := openClawMap(settings, "agents")
	raw, ok := agents["list"].([]any)
	if !ok {
		return nil
	}
	out := make([]any, 0, len(raw))
	dirs := make([]string, 0, len(raw))
	changed := false
	for _, item := range raw {
		entry, ok := item.(map[string]any)
		if !ok {
			out = append(out, item)
			continue
		}
		dir, _ := entry["agentDir"].(string)
		if dir != "" {
			dirs = append(dirs, dir)
		}
		model, _ := entry["model"].(string)
		if !strings.HasPrefix(model, openClawModelPrefix) {
			out = append(out, entry)
			continue
		}
		// Upstream removes the model field so the agent inherits
		// agents.defaults.model.primary, which this adapter sets to ours.
		next := make(map[string]any, len(entry))
		for k, v := range entry {
			if k == "model" {
				continue
			}
			next[k] = v
		}
		out = append(out, next)
		changed = true
	}
	return &openClawAgents{list: out, dirs: dirs, changed: changed}
}

// openClawAgentPlans writes models.providers["9router"] into each agent's own
// models.json, which is the file the client reads per agent.
func openClawAgentPlans(dirs []string, baseURL, key, model string) ([]FilePlan, error) {
	plans := make([]FilePlan, 0, len(dirs))
	for _, dir := range dirs {
		file := TargetFile{
			Path:    filepath.Join(dir, openClawAgentFile),
			Dir:     dir,
			Format:  FormatJSONC,
			Purpose: openClawAgentPurpose,
		}
		doc, err := openClawAgentDocument(file, baseURL, key, model)
		if err != nil {
			return nil, err
		}
		plans = append(plans, FilePlan{File: file, Document: doc})
	}
	return plans, nil
}

func openClawAgentDocument(file TargetFile, baseURL, key, model string) ([]byte, error) {
	old, err := readClientFile(file)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := parseClientJSON(old, &doc); err != nil {
		return nil, err
	}
	if doc == nil {
		doc = map[string]any{}
	}
	providers, ok := doc["providers"].(map[string]any)
	if !ok {
		providers = map[string]any{}
	}
	providers[openClawProviderKey] = map[string]any{
		"baseUrl": baseURL,
		"apiKey":  key,
		"api":     openClawAPIBackend,
		"models":  []any{map[string]any{"id": model, "name": lastModelSegment(model)}},
	}
	doc["providers"] = providers
	return jsonDocument(doc)
}
