package clientconfig

import "strings"

// CodeWhale and ForgeCode: a TOML config with one [openai] table.
//
// src/app/api/cli-tools/codewhale-settings/route.js and
// .../forge-settings/route.js both parse the existing file, replace
// `existing.openai` with {base_url, api_key, model}, and write it back with a
// "managed by PanRouter" header. The adapter keeps the table and its three keys
// and leaves the rest of the file alone, which is stricter than the upstream
// rewrite (it re-emits the whole document) and never loses a key the user had
// outside [openai].
//
// The header comment cannot be reproduced through a key splice, and it is what
// the dashboard uses to recognise a managed file; hasPanRouter on the GET route
// is served from the adapter's own plan here instead.

// openAIDefaultModel is the model upstream writes when the caller names none.
const openAIDefaultModel = "provider/model-id"

// openAITableAdapter is the shape both clients share: the file differs, the
// table does not.
type openAITableAdapter struct {
	baseAdapter
	// keyOrder is the order the upstream route's object literal has, which is
	// the order the keys appear in a freshly created table.
	keyOrder []string
}

func (a openAITableAdapter) Plan(opts Options) ([]FilePlan, error) {
	paths, err := a.Paths(opts)
	if err != nil {
		return nil, err
	}
	return []FilePlan{{File: paths.Files[0], Edits: openAITableEdits(opts, a.keyOrder)}}, nil
}

func (a openAITableAdapter) Apply(opts Options) (Result, error) {
	plans, err := a.Plan(opts)
	if err != nil {
		return Result{}, err
	}
	return applyPlans(plans, nil)
}

func (a openAITableAdapter) Reset(opts Options) (Result, error) {
	paths, err := a.Paths(opts)
	if err != nil {
		return Result{}, err
	}
	edits := make([]Edit, 0, len(a.keyOrder))
	for _, key := range a.keyOrder {
		edits = append(edits, Edit{KeyPath: []string{"openai", key}, Delete: true})
	}
	res, err := applyPlans([]FilePlan{{File: paths.Files[0], Edits: edits}}, nil)
	if err != nil {
		return res, err
	}
	// Upstream deletes the file when removing [openai] leaves nothing behind.
	removed, err := removeFileWhenEmpty(paths.Files[0])
	if err != nil {
		return res, err
	}
	res.Changed = res.Changed || removed
	return res, nil
}

func openAITableEdits(opts Options, order []string) []Edit {
	model := strings.TrimSpace(opts.Model)
	if model == "" {
		model = openAIDefaultModel
	}
	values := map[string]Edit{
		"base_url": {KeyPath: []string{"openai", "base_url"}, Value: withV1(opts.BaseURL), CreateIfMissing: true},
		"api_key":  {KeyPath: []string{"openai", "api_key"}, Value: cliAPIKey(opts.APIKey), CreateIfMissing: true},
		"model":    {KeyPath: []string{"openai", "model"}, Value: model, CreateIfMissing: true},
	}
	edits := make([]Edit, 0, len(order))
	for _, key := range order {
		if e, ok := values[key]; ok {
			edits = append(edits, e)
		}
	}
	return edits
}

// codeWhaleKeyOrder is codewhale-settings/route.js's object literal order.
var codeWhaleKeyOrder = []string{"base_url", "api_key", "model"}

// forgeKeyOrder is forge-settings/route.js's object literal order.
var forgeKeyOrder = []string{"api_key", "base_url", "model"}

func newCodeWhaleAdapter() Adapter {
	return openAITableAdapter{
		baseAdapter: baseAdapter{id: "codewhale", paths: codeWhalePaths},
		keyOrder:    codeWhaleKeyOrder,
	}
}

func newForgeAdapter() Adapter {
	return openAITableAdapter{
		baseAdapter: baseAdapter{id: "forge", paths: forgePaths},
		keyOrder:    forgeKeyOrder,
	}
}
