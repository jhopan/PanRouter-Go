package clientconfig

import (
	"bytes"
	"encoding/json"
	"strings"
)

// Copilot (VS Code's chatLanguageModels.json) and Smelt (~/.smelt/config.json).
//
// src/app/api/cli-tools/copilot-settings/route.js keeps an array of language
// model providers at the document root and replaces only the entry named
// "9Router"; a root array has no key path, so this adapter plans a whole
// document. Entries it does not own are carried through as raw JSON, which is
// what keeps their fields and their order.
//
// smelt is a flat object, so it is patched key by key like the other JSON
// clients, and its file is deleted when a reset leaves nothing in it.

const (
	copilotManagedName     = "9Router"
	copilotVendor          = "azure"
	copilotFallbackKey     = "sk_9router"
	copilotMaxInputTokens  = 128000
	copilotMaxOutputTokens = 16000
	// copilotURLSuffix is appended to the gateway URL the caller sent; VS Code
	// needs the models.ai.azure.com fragment to treat the endpoint as an Azure
	// chat endpoint.
	copilotURLSuffix = "/chat/completions#models.ai.azure.com"
)

// smeltDefaultModel is the model the route writes when neither the request nor
// the existing file names one.
const smeltDefaultModel = "provider/model-id"

type copilotAdapter struct{ baseAdapter }

func newCopilotAdapter() Adapter {
	return copilotAdapter{baseAdapter{id: "copilot", paths: copilotPaths}}
}

func (a copilotAdapter) Plan(opts Options) ([]FilePlan, error) {
	paths, err := a.Paths(opts)
	if err != nil {
		return nil, err
	}
	doc, err := copilotDocument(paths.Files[0], opts)
	if err != nil {
		return nil, err
	}
	return []FilePlan{{File: paths.Files[0], Document: doc}}, nil
}

func (a copilotAdapter) Apply(opts Options) (Result, error) {
	plans, err := a.Plan(opts)
	if err != nil {
		return Result{}, err
	}
	return applyPlans(plans, nil)
}

func (a copilotAdapter) Reset(opts Options) (Result, error) {
	paths, err := a.Paths(opts)
	if err != nil {
		return Result{}, err
	}
	old, err := readClientFile(paths.Files[0])
	if err != nil {
		return Result{}, err
	}
	if len(bytes.TrimSpace(old)) == 0 {
		return Result{}, nil // upstream: "No config file to reset"
	}
	entries, err := parseCopilotEntries(old)
	if err != nil {
		return Result{}, err
	}
	kept := make([]json.RawMessage, 0, len(entries))
	for _, e := range entries {
		name, err := copilotName(e)
		if err != nil {
			return Result{}, err
		}
		if name != copilotManagedName {
			kept = append(kept, e)
		}
	}
	doc, err := jsonDocument(kept)
	if err != nil {
		return Result{}, err
	}
	return applyPlans([]FilePlan{{File: paths.Files[0], Document: doc}}, nil)
}

// copilotDocument builds the array the route writes: the existing entries with
// ours replaced or appended, and our entry rebuilt from the request.
func copilotDocument(file TargetFile, opts Options) ([]byte, error) {
	old, err := readClientFile(file)
	if err != nil {
		return nil, err
	}
	entries, err := parseCopilotEntries(old)
	if err != nil {
		return nil, err
	}
	ours, err := jsonInline(copilotEntryFor(opts))
	if err != nil {
		return nil, err
	}
	out := make([]json.RawMessage, 0, len(entries)+1)
	replaced := false
	for _, e := range entries {
		name, err := copilotName(e)
		if err != nil {
			return nil, err
		}
		if name == copilotManagedName {
			out = append(out, json.RawMessage(ours))
			replaced = true
			continue
		}
		out = append(out, e)
	}
	if !replaced {
		out = append(out, json.RawMessage(ours))
	}
	return jsonDocument(out)
}

// parseCopilotEntries decodes the root array. A document that is not an array
// is treated as no entries, which is exactly what the route does
// (Array.isArray(parsed) ? parsed : []); a malformed document is an error, so a
// broken file is reported instead of being overwritten.
func parseCopilotEntries(b []byte) ([]json.RawMessage, error) {
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, nil
	}
	var probe any
	if err := parseClientJSON(b, &probe); err != nil {
		return nil, err
	}
	if _, isArray := probe.([]any); !isArray {
		return nil, nil
	}
	// Decode the entries a second time as raw text so an entry this adapter
	// does not own goes back byte for byte, numbers included.
	var entries []json.RawMessage
	if err := parseClientJSON(b, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

// copilotName reads one entry's name without assuming its other fields.
func copilotName(raw json.RawMessage) (string, error) {
	var probe struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return "", err
	}
	return probe.Name, nil
}

// copilotEntry is one element of chatLanguageModels.json in the field order the
// route writes it.
type copilotEntry struct {
	Name   string           `json:"name"`
	Vendor string           `json:"vendor"`
	APIKey string           `json:"apiKey"`
	Models []copilotModelID `json:"models"`
}

// copilotModelID is one model inside the entry.
type copilotModelID struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	URL             string `json:"url"`
	ToolCalling     bool   `json:"toolCalling"`
	Vision          bool   `json:"vision"`
	MaxInputTokens  int    `json:"maxInputTokens"`
	MaxOutputTokens int    `json:"maxOutputTokens"`
}

func copilotEntryFor(opts Options) copilotEntry {
	key := cliAPIKey(opts.APIKey)
	if key == "" {
		key = copilotFallbackKey
	}
	url := opts.BaseURL + copilotURLSuffix
	ids := cliModels(opts)
	models := make([]copilotModelID, 0, len(ids))
	for _, id := range ids {
		models = append(models, copilotModelID{
			ID:              id,
			Name:            id,
			URL:             url,
			ToolCalling:     true,
			MaxInputTokens:  copilotMaxInputTokens,
			MaxOutputTokens: copilotMaxOutputTokens,
		})
	}
	return copilotEntry{Name: copilotManagedName, Vendor: copilotVendor, APIKey: key, Models: models}
}

// smeltAdapter owns ~/.smelt/config.json.
type smeltAdapter struct{ baseAdapter }

func newSmeltAdapter() Adapter {
	return smeltAdapter{baseAdapter{id: "smelt", paths: smeltPaths}}
}

func (a smeltAdapter) Plan(opts Options) ([]FilePlan, error) {
	paths, err := a.Paths(opts)
	if err != nil {
		return nil, err
	}
	opts = smeltWithExistingModel(paths.Files[0], opts)
	return []FilePlan{{File: paths.Files[0], Edits: []Edit{
		{KeyPath: []string{"baseUrl"}, Value: withV1(opts.BaseURL), CreateIfMissing: true},
		{KeyPath: []string{"apiKey"}, Value: cliAPIKey(opts.APIKey), CreateIfMissing: true},
		{KeyPath: []string{"model"}, Value: opts.Model, CreateIfMissing: true},
		{KeyPath: []string{"_managedBy"}, Value: "PanRouter", CreateIfMissing: true},
	}}}, nil
}

func (a smeltAdapter) Apply(opts Options) (Result, error) {
	plans, err := a.Plan(opts)
	if err != nil {
		return Result{}, err
	}
	return applyPlans(plans, nil)
}

func (a smeltAdapter) Reset(opts Options) (Result, error) {
	paths, err := a.Paths(opts)
	if err != nil {
		return Result{}, err
	}
	res, err := applyPlans([]FilePlan{{File: paths.Files[0], Edits: deleteEdits(
		[]string{"baseUrl"},
		[]string{"apiKey"},
		[]string{"model"},
		[]string{"_managedBy"},
	)}}, nil)
	if err != nil {
		return res, err
	}
	// Upstream removes the file when nothing but our keys was in it.
	removed, err := removeFileWhenEmpty(paths.Files[0])
	if err != nil {
		return res, err
	}
	res.Changed = res.Changed || removed
	return res, nil
}

// smeltWithExistingModel applies the route's model precedence: the request's model,
// then the one already in the file, then the default.
func smeltWithExistingModel(file TargetFile, opts Options) Options {
	if model := strings.TrimSpace(opts.Model); model != "" {
		opts.Model = model
		return opts
	}
	b, err := readClientFile(file)
	if err == nil {
		var existing struct {
			Model string `json:"model"`
		}
		if parseClientJSON(b, &existing) == nil {
			if model := strings.TrimSpace(existing.Model); model != "" {
				opts.Model = model
				return opts
			}
		}
	}
	opts.Model = smeltDefaultModel
	return opts
}
