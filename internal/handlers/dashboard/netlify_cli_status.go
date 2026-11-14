package dashboard

import (
	"net/http"
	"os"
	"path/filepath"
	"runtime"

	"panrouter/proxy/internal/config"
	"panrouter/proxy/internal/handlerutil"
)

// Port of upstream src/app/api/proxy-pools/netlify-cli-status/route.js.
//
// The route answers "can this machine run netlify-cli?" before the dashboard
// offers a Deploy, so an operator learns they have to install it instead of
// watching a deploy fail a minute later. The resolution order is the one the
// deploy path documents: a local dependency, the lazily-installed runtime copy,
// the legacy runtime location, then a binary on the well-known PATH directories.
//
// The Go gateway's own Netlify deploy uses the API directly and needs no CLI
// (internal/handlers/media/netlify_deploy.go), but the status contract is
// shared with the upstream dashboard, so the answer must stay the same shape:
// available/source/path when found, and the manual command, docs link and the
// candidate list when not.

const netlifyManualCommand = "npm i -g netlify-cli"

const netlifyInstallURL = "https://docs.netlify.com/api-and-cli-guides/cli-guides/get-started-with-cli/"

// netlifyCLICandidate is one probed install location.
type netlifyCLICandidate struct {
	Source string `json:"source"`
	Path   string `json:"path"`
}

// netlifyRuntimeBase is upstream's runtimeBase(): `<DATA_DIR>/runtime`, where
// DATA_DIR is the same directory this gateway already resolves for every other
// piece of runtime state.
func netlifyRuntimeBase() string {
	return filepath.Join(config.ResolveDataDir(), "runtime")
}

// netlifyDependencyPath is the analogue of upstream's
// `require.resolve("netlify-cli/bin/run.js")`: a copy that shipped beside the
// gateway binary. A Go binary carries no node_modules of its own, so the
// nearest thing an operator can have is that directory next to the executable.
func netlifyDependencyPath() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Join(filepath.Dir(exe), "node_modules", "netlify-cli", "bin", "run.js")
}

// netlifyCLICandidates returns the fixed locations, in upstream's order. A
// candidate whose path could not be resolved is omitted rather than probed as
// the empty string, which os.Stat would answer with the current directory.
func netlifyCLICandidates() []netlifyCLICandidate {
	out := make([]netlifyCLICandidate, 0, 3)
	if dep := netlifyDependencyPath(); dep != "" {
		out = append(out, netlifyCLICandidate{Source: "dependency", Path: dep})
	}
	base := netlifyRuntimeBase()
	out = append(out,
		netlifyCLICandidate{
			Source: "runtime",
			Path:   filepath.Join(base, "netlify-cli", "node_modules", "netlify-cli", "bin", "run.js"),
		},
		netlifyCLICandidate{
			Source: "runtime-legacy",
			Path:   filepath.Join(base, "node_modules", "netlify-cli", "bin", "run.js"),
		},
	)
	return out
}

// findNetlifyOnPath mirrors upstream findOnPath: the npm global bin directory,
// the launcher's own tools directory, the working directory and its .bin.
func findNetlifyOnPath() string {
	name := "netlify"
	if runtime.GOOS == "windows" {
		name = "netlify.cmd"
	}
	cwd, _ := os.Getwd()
	dirs := []string{
		filepath.Join(os.Getenv("APPDATA"), "npm"),
		filepath.Join(os.Getenv("LOCALAPPDATA"), "hermes", "tools", "node-26.7.0-win32-x64"),
		cwd,
		filepath.Join(cwd, "node_modules", ".bin"),
	}
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, name)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	return ""
}

// netlifyPathSearch is the PATH-directories probe. A var so a test can prove
// the "not installed" branch on a machine that happens to have netlify.cmd in
// %APPDATA%/npm.
var netlifyPathSearch = findNetlifyOnPath

// HandleGetNetlifyCLIStatus handles GET /api/proxy-pools/netlify-cli-status.
func (h *DashboardHandler) HandleGetNetlifyCLIStatus(w http.ResponseWriter, r *http.Request) {
	checked := netlifyCLICandidates()
	for _, candidate := range checked {
		if info, err := os.Stat(candidate.Path); err == nil && !info.IsDir() {
			handlerutil.WriteJSON(w, http.StatusOK, map[string]any{
				"available":     true,
				"source":        candidate.Source,
				"path":          candidate.Path,
				"manualCommand": netlifyManualCommand,
			})
			return
		}
	}

	if onPath := netlifyPathSearch(); onPath != "" {
		handlerutil.WriteJSON(w, http.StatusOK, map[string]any{
			"available":     true,
			"source":        "path",
			"path":          onPath,
			"manualCommand": netlifyManualCommand,
		})
		return
	}

	// A runtime directory that exists but has no entry point is debris from a
	// killed install. Reporting it explains why the next attempt may be slow.
	staleInstall := false
	if info, err := os.Stat(filepath.Join(netlifyRuntimeBase(), "netlify-cli")); err == nil && info.IsDir() {
		staleInstall = true
	}

	reported := make([]map[string]any, 0, len(checked))
	for _, candidate := range checked {
		reported = append(reported, map[string]any{
			"source": candidate.Source,
			"path":   candidate.Path,
			"exists": false,
		})
	}

	handlerutil.WriteJSON(w, http.StatusOK, map[string]any{
		"available":     false,
		"autoInstall":   true,
		"staleInstall":  staleInstall,
		"manualCommand": netlifyManualCommand,
		"installUrl":    netlifyInstallURL,
		"checked":       reported,
	})
}
