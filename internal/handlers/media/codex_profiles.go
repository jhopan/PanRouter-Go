package media

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	json "encoding/json/v2"
	"panrouter/proxy/internal/handlerutil"
)

// Codex multi-model profiles: ~/.codex/<name>.config.toml, one file per profile,
// selected at run time with `codex -p <name>`.
//
// Port of upstream `src/app/api/cli-tools/codex-profiles/route.js`. The file is
// the whole contract — Codex reads `model` and `model_provider` from it and
// names the profile after the file — so this route never parses or rewrites
// anything else in the directory. `config.toml` itself is excluded: that is the
// active configuration, not a profile, and writing it from a profile form would
// silently change what a plain `codex` run does.

// codexProfileNameRE is upstream's guard. A profile name becomes a file name,
// so it may not carry a path separator, a dot, or anything shell-shaped.
var codexProfileNameRE = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// codexProfileModelRE finds the `model = "..."` line of a profile.
// Anchored to the line start, which is what upstream matches, so a `model`
// mentioned inside another string does not become the profile's model. The two
// alternatives are upstream's `(["'])([^"\n]+)\1` spelled without a
// backreference, which Go's regexp does not support: a double-quoted value and a
// single-quoted one, each closed by the quote that opened it.
var codexProfileModelRE = regexp.MustCompile(`(?m)^\s*model\s*=\s*(?:"([^"\n]+)"|'([^'\n]+)')`)

// codexProfile is one entry of the dashboard's profile list.
type codexProfile struct {
	Name    string `json:"name"`
	Model   string `json:"model"`
	Command string `json:"command"`
}

// CodexProfilesHandler serves /api/cli-tools/codex-profiles.
type CodexProfilesHandler struct{}

// NewCodexProfilesHandler returns the profiles handler.
func NewCodexProfilesHandler() *CodexProfilesHandler { return &CodexProfilesHandler{} }

// codexProfilesDir resolves the directory the profiles live in.
//
// CODEX_HOME wins, because the Codex CLI itself reads it: a profile written
// somewhere the CLI does not look is a profile the user cannot run. `home=` is
// the same preview/test override the settings routes beside this one accept, and
// the name check keeps the joined path inside the directory either way.
func codexProfilesDir(r *http.Request) string {
	if dir := strings.TrimSpace(os.Getenv("CODEX_HOME")); dir != "" {
		return dir
	}
	home := strings.TrimSpace(r.URL.Query().Get("home"))
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = h
		}
	}
	return filepath.Join(home, ".codex")
}

// validCodexProfileName reports whether name may become a profile file.
// `config` is reserved, case-insensitively: it is the active configuration.
func validCodexProfileName(name string) bool {
	return codexProfileNameRE.MatchString(name) &&
		len(name) <= 64 &&
		!strings.EqualFold(name, "config")
}

// validCodexProfileModel reports whether model may be written into the file.
// The value is quoted into TOML, so a quote or a newline would break out of the
// string it is meant to be.
func validCodexProfileModel(model string) bool {
	if model == "" || len(model) > 256 {
		return false
	}
	return !strings.ContainsAny(model, "\r\n\"")
}

// readCodexProfile describes one profile file.
func readCodexProfile(path, name string) codexProfile {
	p := codexProfile{Name: name, Command: "codex -p " + name}
	if content, err := os.ReadFile(path); err == nil {
		if m := codexProfileModelRE.FindSubmatch(content); m != nil {
			// Group 1 is the double-quoted value, group 2 the single-quoted one.
			if len(m[1]) > 0 {
				p.Model = string(m[1])
			} else {
				p.Model = string(m[2])
			}
		}
	}
	return p
}

// HandleGet serves GET /api/cli-tools/codex-profiles.
//
// A missing directory is not an error: a machine that has never run Codex has no
// profiles, which is the empty list the dashboard renders.
func (h *CodexProfilesHandler) HandleGet(w http.ResponseWriter, r *http.Request) {
	dir := codexProfilesDir(r)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			handlerutil.WriteJSONError(w, http.StatusInternalServerError, "Failed to read profiles")
			return
		}
		handlerutil.WriteJSON(w, http.StatusOK, map[string]any{"profiles": []codexProfile{}})
		return
	}

	profiles := make([]codexProfile, 0, len(entries))
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".config.toml")
		if !ok || name == "" || e.IsDir() {
			continue
		}
		profiles = append(profiles, readCodexProfile(filepath.Join(dir, e.Name()), name))
	}
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].Name < profiles[j].Name })
	handlerutil.WriteJSON(w, http.StatusOK, map[string]any{"profiles": profiles})
}

// HandlePost serves POST /api/cli-tools/codex-profiles: create or overwrite one
// profile. The body is replaced wholesale, like upstream — a profile is three
// lines, so merging would only preserve stray edits nobody asked to keep.
func (h *CodexProfilesHandler) HandlePost(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name  string `json:"name"`
		Model string `json:"model"`
	}
	if r.Body != nil {
		_ = json.UnmarshalRead(r.Body, &req)
	}
	name := strings.ToLower(strings.TrimSpace(req.Name))
	if !validCodexProfileName(name) {
		handlerutil.WriteJSONError(w, http.StatusBadRequest,
			"Profile name can only contain alphanumeric characters, dashes, underscores and cannot be 'config'")
		return
	}
	model := strings.TrimSpace(req.Model)
	if !validCodexProfileModel(model) {
		handlerutil.WriteJSONError(w, http.StatusBadRequest, "Invalid model name")
		return
	}

	dir := codexProfilesDir(r)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		handlerutil.WriteJSONError(w, http.StatusInternalServerError, "Failed to save profile")
		return
	}
	content := "# codex -p " + name + "\nmodel = \"" + model + "\"\nmodel_provider = \"PanRouter\"\n"
	if err := os.WriteFile(filepath.Join(dir, name+".config.toml"), []byte(content), 0o644); err != nil {
		handlerutil.WriteJSONError(w, http.StatusInternalServerError, "Failed to save profile")
		return
	}
	handlerutil.WriteJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"profile": codexProfile{Name: name, Model: model, Command: "codex -p " + name},
	})
}

// HandleDelete serves DELETE /api/cli-tools/codex-profiles. Deleting a profile
// that is already gone is a 404, not a success: the caller is looking at a list
// that is out of date, and saying "deleted" hides that.
func (h *CodexProfilesHandler) HandleDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if r.Body != nil {
		_ = json.UnmarshalRead(r.Body, &req)
	}
	name := strings.ToLower(strings.TrimSpace(req.Name))
	if !validCodexProfileName(name) {
		handlerutil.WriteJSONError(w, http.StatusBadRequest, "Invalid profile name")
		return
	}
	path := filepath.Join(codexProfilesDir(r), name+".config.toml")
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			handlerutil.WriteJSONError(w, http.StatusNotFound, "Profile not found")
			return
		}
		handlerutil.WriteJSONError(w, http.StatusInternalServerError, "Failed to delete profile")
		return
	}
	handlerutil.WriteJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Profile " + name + " deleted",
	})
}
