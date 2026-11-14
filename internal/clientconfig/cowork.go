package clientconfig

import "path/filepath"

// Cowork: Claude Desktop's 3p (third-party inference) mode.
//
// src/app/api/cli-tools/cowork-settings/route.js writes four things:
//
//   - Claude's own claude_desktop_config.json gains deploymentMode = "3p",
//   - <data>/configLibrary/_meta.json names the profile that is active,
//   - <data>/configLibrary/<appliedId>.json is that profile: the relax-security
//     block plus the inference gateway, its key, its models and the managed MCP
//     servers,
//   - <data>/config.json gains operonSkipMcpApprovals so the managed servers do
//     not prompt on every tool call.
//
// The profile id is generated once and then reused. Upstream generates a random
// UUID; this adapter uses a fixed opaque id, because a plan that cannot predict
// the file name it is about to write is not a plan — both Apply and a second
// Apply have to name the same path for the write to be idempotent.
//
// The plugin list, the local-bridge plugin list and the per-agent overrides the
// upstream POST accepts are not part of this route's request: the dashboard's
// settings route sends baseUrl/apiKey/model(s), so the adapter writes the
// default remote plugin set and no bridge or custom entries.

const (
	coworkProvider = "gateway"
	// coworkDefaultAppliedID is the opaque profile id (a UUID v4 shape, not a
	// generated one) so Plan and Apply name the same file.
	coworkDefaultAppliedID = "00000000-0000-4000-8000-000000000001"
	coworkDefaultEntry     = "Default"
	coworkSkip             = "operonSkipMcpApprovals"
	coworkMetaIDKey        = "appliedId"
)

type coworkAdapter struct{ baseAdapter }

func newCoworkAdapter() Adapter {
	return coworkAdapter{baseAdapter{id: "cowork", paths: coworkPaths}}
}

func (a coworkAdapter) Plan(opts Options) ([]FilePlan, error) {
	paths, err := a.Paths(opts)
	if err != nil {
		return nil, err
	}
	models := cliModels(opts)
	if len(models) == 0 {
		return nil, errModelRequired("cowork", paths.Files[0])
	}
	home, err := coworkHome(opts)
	if err != nil {
		return nil, err
	}
	writeRoot, readRoot := coworkRoots(home)
	metaFile := paths.Files[0]
	meta, atWrite, err := coworkMeta(metaFile, readRoot)
	if err != nil {
		return nil, err
	}
	appliedID := coworkMetaID(meta)
	if appliedID == "" {
		meta = coworkDefaultMeta()
		appliedID = coworkDefaultAppliedID
	}

	plans := make([]FilePlan, 0, 4)
	if !atWrite {
		doc, err := jsonDocument(meta)
		if err != nil {
			return nil, err
		}
		plans = append(plans, FilePlan{File: metaFile, Document: doc})
	}

	configDir := filepath.Join(writeRoot, "configLibrary")
	profileFile := TargetFile{
		Path:    filepath.Join(configDir, appliedID+".json"),
		Dir:     configDir,
		Format:  FormatJSONC,
		Purpose: "profile",
	}
	profile, err := jsonDocument(coworkConfigFor(opts, models))
	if err != nil {
		return nil, err
	}
	plans = append(plans, FilePlan{File: profileFile, Document: profile})

	skip := coworkSkipApprovals()
	skipJSON, err := jsonInline(skip)
	if err != nil {
		return nil, err
	}
	plans = append(plans, FilePlan{File: paths.Files[1], Edits: []Edit{{
		KeyPath:         []string{coworkSkip},
		ValueJSON:       skipJSON,
		CreateIfMissing: true,
	}}})

	plans = append(plans, coworkDesktopPlan(paths.Files[2]))
	return plans, nil
}

func (a coworkAdapter) Apply(opts Options) (Result, error) {
	plans, err := a.Plan(opts)
	if err != nil {
		return Result{}, err
	}
	return applyPlans(plans, nil)
}

func (a coworkAdapter) Reset(opts Options) (Result, error) {
	paths, err := a.Paths(opts)
	if err != nil {
		return Result{}, err
	}
	home, err := coworkHome(opts)
	if err != nil {
		return Result{}, err
	}
	_, readRoot := coworkRoots(home)
	meta, _, err := coworkMeta(paths.Files[0], readRoot)
	if err != nil {
		return Result{}, err
	}
	appliedID := coworkMetaID(meta)
	if appliedID == "" {
		return Result{}, nil // upstream: "No active config to reset"
	}
	// Upstream resets the profile in the directory it read the id from
	// (getConfigDir), which is not always the directory Apply writes to.
	configDir := filepath.Join(readRoot, "configLibrary")
	profileFile := TargetFile{
		Path:    filepath.Join(configDir, appliedID+".json"),
		Dir:     configDir,
		Format:  FormatJSONC,
		Purpose: "profile",
	}
	plans := []FilePlan{
		// Upstream writes "{}" rather than deleting: Claude Desktop expects the
		// profile file it was told about to exist.
		{File: profileFile, Document: []byte("{}")},
		{File: paths.Files[1], Edits: []Edit{{
			KeyPath:         []string{coworkSkip},
			ValueJSON:       "{}",
			CreateIfMissing: true,
		}}},
	}
	// The 1p cleanup only runs on a file that is there and still holds a legacy
	// entry; touching it otherwise would create a Claude directory on a machine
	// that never installed it.
	if legacy := coworkLegacyDesktopPlan(paths.Files[2]); legacy != nil {
		plans = append(plans, *legacy)
	}
	return applyPlans(plans, nil)
}

// coworkHome is the home the cowork roots resolve against. The adapter needs it
// itself because Paths only reports the file list, and cowork has to know the
// read root and the write root separately.
func coworkHome(opts Options) (string, error) {
	if opts.Home != "" {
		return opts.Home, nil
	}
	return homeDir()
}

// coworkMeta returns the profile metadata and whether the write-side file
// already names a profile. An unreadable or malformed read-side file counts as
// missing, which is what the route's readJson does; a malformed write-side file
// is reported, because that is the file this adapter owns.
func coworkMeta(writeFile TargetFile, readRoot string) (map[string]any, bool, error) {
	writeBytes, err := readClientFile(writeFile)
	if err != nil {
		return nil, false, err
	}
	meta, ok, err := coworkParseMeta(writeBytes)
	if err != nil {
		return nil, false, err
	}
	if ok && coworkMetaID(meta) != "" {
		return meta, true, nil
	}
	readDir := filepath.Join(readRoot, "configLibrary")
	readFile := TargetFile{
		Path:    filepath.Join(readDir, "_meta.json"),
		Dir:     readDir,
		Format:  FormatJSONC,
		Purpose: "meta",
	}
	if readFile.Path != writeFile.Path {
		readBytes, readErr := readClientFile(readFile)
		if readErr == nil {
			if meta, ok, parseErr := coworkParseMeta(readBytes); parseErr == nil && ok && coworkMetaID(meta) != "" {
				// The id came from the other Claude installation: it is reused,
				// but the write still lands in the directory upstream writes.
				return meta, false, nil
			}
		}
	}
	return coworkDefaultMeta(), false, nil
}

func coworkParseMeta(b []byte) (map[string]any, bool, error) {
	meta := map[string]any{}
	if len(b) == 0 {
		return meta, false, nil
	}
	if err := parseClientJSON(b, &meta); err != nil {
		return nil, false, err
	}
	if meta == nil {
		return map[string]any{}, true, nil
	}
	return meta, true, nil
}

func coworkMetaID(meta map[string]any) string {
	id, _ := meta[coworkMetaIDKey].(string)
	return id
}

func coworkDefaultMeta() map[string]any {
	return map[string]any{
		coworkMetaIDKey: coworkDefaultAppliedID,
		"entries":       []any{map[string]any{"id": coworkDefaultAppliedID, "name": coworkDefaultEntry}},
	}
}
