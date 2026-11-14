package clientconfig

import (
	"fmt"
	"strings"
)

// DeepSeek TUI and Grok Build: TOML clients whose upstream route owns a whole
// provider/model section rather than a nested map.
//
// src/app/api/cli-tools/deepseek-tui-settings/route.js rebuilds
// ~/.deepseek/config.toml from a template on every POST and writes
// provider = "deepseek" on DELETE.
//
// src/lib/grokBuildConfig.js writes [model.9router] plus models.default for
// ~/.grok/config.toml and remembers the previous default in a comment marker so
// DELETE can restore it. The adapter reproduces the sections and the default;
// the marker is a comment, which a key splice cannot write, so Reset falls back
// to upstream's own fallback value (grok-build) when the current default is
// ours.

// deepSeekFallbackKey is what deepseek-tui's route writes when the caller sends
// no key.
const deepSeekFallbackKey = "sk_9router"

// grokMainModelSlot is the model table grok-build selects (GROK_MAIN_MODEL_SLOT).
const grokMainModelSlot = "9router"

// grokBuiltinDefault is grok-build's own default when no previous value was
// remembered (GROK_BUILTIN_DEFAULT).
const grokBuiltinDefault = "grok-build"

// grokSubagentTypes are the per-type overrides upstream also manages
// (GROK_SUBAGENT_TYPES). Connect writes none of them, but Reset clears the slots
// an earlier upstream apply may have left so `9router-*` never survives.
var grokSubagentTypes = []string{"general-purpose", "explore", "plan"}

type deepSeekTUIAdapter struct{ baseAdapter }

func newDeepSeekTUIAdapter() Adapter {
	return deepSeekTUIAdapter{baseAdapter{id: "deepseek-tui", paths: deepSeekTUIPaths}}
}

func (a deepSeekTUIAdapter) Plan(opts Options) ([]FilePlan, error) {
	paths, err := a.Paths(opts)
	if err != nil {
		return nil, err
	}
	key := cliAPIKey(opts.APIKey)
	if key == "" {
		key = deepSeekFallbackKey
	}
	edits := []Edit{
		{KeyPath: []string{"provider"}, Value: "openai", CreateIfMissing: true},
		{KeyPath: []string{"providers", "openai", "base_url"}, Value: withV1(opts.BaseURL), CreateIfMissing: true},
		{KeyPath: []string{"providers", "openai", "api_key"}, Value: key, CreateIfMissing: true},
	}
	// Upstream refuses a POST without a model, so the section it writes always
	// carries one. A caller that sends none keeps whatever model the table
	// already names instead of being given an empty string.
	if model := strings.TrimSpace(opts.Model); model != "" {
		edits = append(edits, Edit{KeyPath: []string{"providers", "openai", "model"}, Value: model, CreateIfMissing: true})
	}
	return []FilePlan{{File: paths.Files[0], Edits: edits}}, nil
}

func (a deepSeekTUIAdapter) Apply(opts Options) (Result, error) {
	plans, err := a.Plan(opts)
	if err != nil {
		return Result{}, err
	}
	return applyPlans(plans, nil)
}

func (a deepSeekTUIAdapter) Reset(opts Options) (Result, error) {
	paths, err := a.Paths(opts)
	if err != nil {
		return Result{}, err
	}
	edits := []Edit{{KeyPath: []string{"provider"}, Value: "deepseek", CreateIfMissing: true}}
	edits = append(edits, deleteEdits(
		[]string{"providers", "openai", "base_url"},
		[]string{"providers", "openai", "api_key"},
		[]string{"providers", "openai", "model"},
	)...)
	// Upstream writes DEFAULT_CONFIG, which keeps the file. The empty
	// [providers.openai] header the deletes leave is dropped by the TOML writer.
	return applyPlans([]FilePlan{{File: paths.Files[0], Edits: edits}}, nil)
}

// grokBuildAdapter owns ~/.grok/config.toml.
type grokBuildAdapter struct{ baseAdapter }

func newGrokBuildAdapter() Adapter {
	return grokBuildAdapter{baseAdapter{id: "grok-build", paths: grokBuildPaths}}
}

func (a grokBuildAdapter) Plan(opts Options) ([]FilePlan, error) {
	paths, err := a.Paths(opts)
	if err != nil {
		return nil, err
	}
	model := strings.TrimSpace(opts.Model)
	if model == "" {
		// A grok-build POST without a model would write a section the client
		// cannot resolve; upstream refuses that request outright, and the
		// dashboard always sends one.
		return nil, errModelRequired("grok-build", paths.Files[0])
	}
	key := cliAPIKey(opts.APIKey)
	if key == "" {
		key = deepSeekFallbackKey
	}
	section := []string{"model", grokMainModelSlot}
	// The field order is buildModelSection's, because that is what a client
	// reading the section for the first time parses.
	edits := []Edit{
		{KeyPath: appendPath(section, "model"), Value: model, CreateIfMissing: true},
		{KeyPath: appendPath(section, "base_url"), Value: withV1(opts.BaseURL), CreateIfMissing: true},
		{KeyPath: appendPath(section, "name"), Value: "9Router", CreateIfMissing: true},
		{KeyPath: appendPath(section, "description"), Value: "Routed via 9Router gateway", CreateIfMissing: true},
		{KeyPath: appendPath(section, "api_backend"), Value: "chat_completions", CreateIfMissing: true},
	}
	// api_key is optional upstream: buildModelSection omits the line when the
	// route was given no key. This adapter always resolved one (the fallback),
	// so the line is always written, like a dashboard POST with a key.
	edits = append(edits, Edit{KeyPath: appendPath(section, "api_key"), Value: key, CreateIfMissing: true})
	if limits, ok := opts.ModelLimits[model]; ok && limits.ContextWindow > 0 {
		edits = append(edits, Edit{
			KeyPath:         appendPath(section, "context_window"),
			ValueJSON:       numberLiteral(limits.ContextWindow),
			CreateIfMissing: true,
		})
	}
	edits = append(edits, Edit{KeyPath: []string{"models", "default"}, Value: grokMainModelSlot, CreateIfMissing: true})
	return []FilePlan{{File: paths.Files[0], Edits: edits}}, nil
}

func (a grokBuildAdapter) Apply(opts Options) (Result, error) {
	plans, err := a.Plan(opts)
	if err != nil {
		return Result{}, err
	}
	return applyPlans(plans, nil)
}

func (a grokBuildAdapter) Reset(opts Options) (Result, error) {
	paths, err := a.Paths(opts)
	if err != nil {
		return Result{}, err
	}
	slots := []string{grokMainModelSlot}
	for _, t := range grokSubagentTypes {
		slots = append(slots, grokMainModelSlot+"-"+t)
	}
	var edits []Edit
	for _, slot := range slots {
		section := []string{"model", slot}
		edits = append(edits, deleteEdits(
			appendPath(section, "model"),
			appendPath(section, "base_url"),
			appendPath(section, "name"),
			appendPath(section, "description"),
			appendPath(section, "api_backend"),
			appendPath(section, "api_key"),
			appendPath(section, "context_window"),
		)...)
	}
	// The per-type mapping has to go after its model section, so a client
	// reading the file mid-reset never sees a mapping to a missing slot.
	for _, t := range grokSubagentTypes {
		edits = append(edits, Edit{KeyPath: []string{"subagents", "models", t}, Delete: true})
	}
	// models.default is the user's key once it no longer names us; restore
	// upstream's fallback only when it still does.
	if grokDefaultIsOurs(paths.Files[0]) {
		edits = append(edits, Edit{KeyPath: []string{"models", "default"}, Value: grokBuiltinDefault, CreateIfMissing: true})
	}
	return applyPlans([]FilePlan{{File: paths.Files[0], Edits: edits}}, nil)
}

// grokDefaultIsOurs reports whether models.default still names our slot. An
// unreadable or missing file is not ours to rewrite.
func grokDefaultIsOurs(file TargetFile) bool {
	b, err := readClientFile(file)
	if err != nil || len(b) == 0 {
		return false
	}
	doc, err := scanTOML(parseableTOML(b), b)
	if err != nil {
		return false
	}
	for _, kv := range doc.kvs {
		if samePath(kv.fullPath, []string{"models", "default"}) {
			return strings.Trim(string(b[kv.valStart:kv.valEnd]), `"'`) == grokMainModelSlot
		}
	}
	return false
}

// errModelRequired is the refusal for a client whose upstream route requires a
// model, because its config names the selected model in the section it writes.
func errModelRequired(id string, file TargetFile) error {
	return fmt.Errorf("%w: %s needs a model to write into %s", ErrOptionsRequired, id, file.Path)
}
