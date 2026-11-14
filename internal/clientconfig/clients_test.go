package clientconfig

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The ten dashboard-only clients: the file each one owns is the file its
// upstream route reads, the first apply creates it, and a second apply changes
// nothing. The route-level byte expectations live in
// internal/handlers/media/cli_tool_clients_test.go.

// dashboardClientPathCases is keyed by client id and gives the home-relative
// paths the adapter must resolve. Anything platform-specific is computed below.
func dashboardClientPathCases(home string) map[string][]string {
	coworkRoot, firstParty := clientCoworkRoots(home)
	return map[string][]string{
		"codewhale": {filepath.Join(home, ".codewhale", "config.toml")},
		"copilot":   {filepath.Join(clientCopilotDir(home), "chatLanguageModels.json")},
		"cowork": {
			filepath.Join(coworkRoot, "configLibrary", "_meta.json"),
			filepath.Join(coworkRoot, "config.json"),
			filepath.Join(firstParty, "claude_desktop_config.json"),
		},
		"deepseek-tui": {filepath.Join(home, ".deepseek", "config.toml")},
		"devin":        {},
		"forge":        {filepath.Join(home, ".forge", "config.toml")},
		"grok-build":   {filepath.Join(home, ".grok", "config.toml")},
		"jcode": {
			filepath.Join(home, ".jcode", "config.toml"),
			filepath.Join(home, ".config", "jcode", "provider-9router.env"),
		},
		"openclaw": {filepath.Join(home, ".openclaw", "openclaw.json")},
		"smelt":    {filepath.Join(home, ".smelt", "config.json")},
	}
}

func clientCopilotDir(home string) string {
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Code", "User")
	case "windows":
		return filepath.Join(home, "roaming", "Code", "User")
	}
	return filepath.Join(home, ".config", "Code", "User")
}

func clientCoworkRoots(home string) (writeRoot, firstParty string) {
	switch runtime.GOOS {
	case "darwin":
		base := filepath.Join(home, "Library", "Application Support")
		return filepath.Join(base, "Claude-3p"), filepath.Join(base, "Claude")
	case "windows":
		return filepath.Join(home, "local", "Claude-3p"), filepath.Join(home, "roaming", "Claude")
	}
	return filepath.Join(home, ".config", "Claude-3p"), filepath.Join(home, ".config", "Claude")
}

// clientTestHome points the environment-derived resolvers at a temp tree.
func clientTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("APPDATA", filepath.Join(home, "roaming"))
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "local"))
	return home
}

func TestDashboardClientPaths(t *testing.T) {
	home := clientTestHome(t)
	want := dashboardClientPathCases(home)
	ids := dashboardClientIDs(t)
	if len(ids) != len(want) {
		t.Fatalf("registry has %d dashboard clients, test describes %d", len(ids), len(want))
	}
	for _, id := range ids {
		t.Run(id, func(t *testing.T) {
			adapter, err := Resolve(id)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			paths, err := adapter.Paths(Options{Home: home})
			if err != nil {
				t.Fatalf("Paths: %v", err)
			}
			got := make([]string, 0, len(paths.Files))
			for _, f := range paths.Files {
				got = append(got, f.Path)
			}
			expected := want[id]
			if len(got) != len(expected) {
				t.Fatalf("paths = %v, want %v", got, expected)
			}
			for i := range expected {
				if got[i] != expected[i] {
					t.Errorf("file %d = %s, want %s", i, got[i], expected[i])
				}
			}
		})
	}
}

// The env override is the client's own: jcode reads $XDG_CONFIG_HOME for its
// env file and copilot reads %APPDATA%, so the adapter must too.
func TestDashboardClientEnvOverrides(t *testing.T) {
	home := t.TempDir()
	xdg := filepath.Join(home, "xdg")
	t.Setenv("XDG_CONFIG_HOME", xdg)

	jcode, err := Resolve("jcode")
	if err != nil {
		t.Fatal(err)
	}
	paths, err := jcode.Paths(Options{Home: home})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(xdg, "jcode", "provider-9router.env"); paths.Files[1].Path != want {
		t.Errorf("jcode env file = %s, want %s", paths.Files[1].Path, want)
	}

	if runtime.GOOS != "windows" {
		t.Skip("copilot's %APPDATA% override only exists on Windows")
	}
	roaming := filepath.Join(home, "custom-roaming")
	t.Setenv("APPDATA", roaming)
	copilot, err := Resolve("copilot")
	if err != nil {
		t.Fatal(err)
	}
	paths, err = copilot.Paths(Options{Home: home})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(roaming, "Code", "User", "chatLanguageModels.json"); paths.Files[0].Path != want {
		t.Errorf("copilot config = %s, want %s", paths.Files[0].Path, want)
	}
}

// TestDashboardClientsApplyIsIdempotent runs every adapter twice and checks the
// second pass reports no change. A client whose write is not idempotent grows a
// duplicated model list or a new backup on every dashboard click.
func TestDashboardClientsApplyIsIdempotent(t *testing.T) {
	for _, id := range dashboardClientIDs(t) {
		t.Run(id, func(t *testing.T) {
			home := clientTestHome(t)
			adapter, err := Resolve(id)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			opts := Options{
				Home:    home,
				BaseURL: "http://localhost:20128",
				APIKey:  "sk-test",
				Model:   "cc/claude-sonnet-5",
				Models:  []string{"cc/claude-sonnet-5", "glm/glm-5.3"},
			}
			first, err := adapter.Apply(opts)
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			second, err := adapter.Apply(opts)
			if err != nil {
				t.Fatalf("second Apply: %v", err)
			}
			if second.Changed {
				t.Error("second Apply reported a change")
			}
			if len(second.Backups) != 0 {
				t.Errorf("second Apply created backups: %v", second.Backups)
			}
			if id == "devin" {
				// The detection-only client owns no file; neither pass may
				// claim it wrote one.
				if first.Changed || second.Changed {
					t.Error("devin claimed a file change")
				}
			}
		})
	}
}

// TestDashboardClientsResetRemovesTheGateway applies then resets every client
// and asserts no gateway URL survives anywhere the adapter wrote. It is the
// reset half of the round trip: a reset that leaves the base URL behind leaves
// the client pointed at a gateway the user just removed.
func TestDashboardClientsResetRemovesTheGateway(t *testing.T) {
	const baseURL = "http://localhost:20128"
	for _, id := range dashboardClientIDs(t) {
		t.Run(id, func(t *testing.T) {
			home := clientTestHome(t)
			adapter, err := Resolve(id)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			opts := Options{
				Home:    home,
				BaseURL: baseURL,
				APIKey:  "sk-test",
				Model:   "cc/claude-sonnet-5",
				Models:  []string{"cc/claude-sonnet-5"},
			}
			if _, err := adapter.Apply(opts); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if _, err := adapter.Reset(opts); err != nil {
				t.Fatalf("Reset: %v", err)
			}
			err = filepath.Walk(home, func(path string, info os.FileInfo, err error) error {
				if err != nil || info.IsDir() {
					return err
				}
				// A backup is the pre-reset file by definition.
				if strings.Contains(path, ".bak.") {
					return nil
				}
				b, readErr := os.ReadFile(path)
				if readErr != nil {
					return readErr
				}
				if strings.Contains(string(b), baseURL) {
					t.Errorf("Reset left the gateway URL in %s:\n%s", path, b)
				}
				return nil
			})
			if err != nil {
				t.Fatalf("walk: %v", err)
			}
		})
	}
}

// TestDashboardClientUpstreamDefaults pins the placeholders the upstream routes
// write when the request leaves a field empty. They are the values a user sees
// in a fresh config, so they are part of the contract.
func TestDashboardClientUpstreamDefaults(t *testing.T) {
	cases := []struct {
		id      string
		body    func(string) string
		wantKey string
		value   string
	}{
		{"codewhale", func(home string) string { return filepath.Join(home, ".codewhale", "config.toml") }, "model", `"provider/model-id"`},
		{"forge", func(home string) string { return filepath.Join(home, ".forge", "config.toml") }, "model", `"provider/model-id"`},
		{"deepseek-tui", func(home string) string { return filepath.Join(home, ".deepseek", "config.toml") }, "api_key", `"sk_9router"`},
		{"smelt", func(home string) string { return filepath.Join(home, ".smelt", "config.json") }, "model", `"provider/model-id"`},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			home := clientTestHome(t)
			adapter, err := Resolve(tc.id)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			// No model and no key: the adapter must fall back exactly like the
			// upstream route.
			if _, err := adapter.Apply(Options{Home: home, BaseURL: "http://localhost:20128"}); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			b, err := os.ReadFile(tc.body(home))
			if err != nil {
				t.Fatalf("read written config: %v", err)
			}
			got := string(b)
			if !strings.Contains(got, tc.value) {
				t.Errorf("written config does not carry %s = %s:\n%s", tc.wantKey, tc.value, got)
			}
		})
	}
}

// dashboardClientIDs is every id the registry added beyond the nine `connect`
// adapters, derived from the registry rather than restated, so a new client
// cannot be added without these tests covering it.
func dashboardClientIDs(t *testing.T) []string {
	t.Helper()
	connect := map[string]struct{}{
		"claude": {}, "codex": {}, "opencode": {}, "droid": {}, "crush": {},
		"kilo": {}, "cline": {}, "pi": {}, "omp": {},
	}
	var out []string
	for _, id := range IDs() {
		if _, ok := connect[id]; ok {
			continue
		}
		out = append(out, id)
	}
	return out
}
