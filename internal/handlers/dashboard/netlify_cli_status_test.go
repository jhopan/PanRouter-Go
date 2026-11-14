package dashboard

import (
	json "encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

type netlifyCLIStatusBody struct {
	Available     bool   `json:"available"`
	Source        string `json:"source"`
	Path          string `json:"path"`
	AutoInstall   bool   `json:"autoInstall"`
	StaleInstall  bool   `json:"staleInstall"`
	ManualCommand string `json:"manualCommand"`
	InstallURL    string `json:"installUrl"`
	Checked       []struct {
		Source string `json:"source"`
		Path   string `json:"path"`
		Exists bool   `json:"exists"`
	} `json:"checked"`
}

// Port-of-upstream test for
// src/app/api/proxy-pools/netlify-cli-status/route.js.
func TestNetlifyCLIStatus(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("DATA_DIR", dataDir)

	origPathSearch := netlifyPathSearch
	netlifyPathSearch = func() string { return "" }
	t.Cleanup(func() { netlifyPathSearch = origPathSearch })

	runtimeEntry := filepath.Join(dataDir, "runtime", "netlify-cli", "node_modules", "netlify-cli", "bin", "run.js")
	staleDir := filepath.Join(dataDir, "runtime", "netlify-cli")

	tests := []struct {
		name         string
		prepare      func(t *testing.T)
		onPath       string
		wantAvail    bool
		wantSource   string
		wantStale    bool
		wantChecked  int
	}{
		{
			name:        "absent everywhere",
			prepare:     func(*testing.T) {},
			wantAvail:   false,
			wantStale:   false,
			wantChecked: 3,
		},
		{
			name: "a runtime install answers available",
			prepare: func(t *testing.T) {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(runtimeEntry), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(runtimeEntry, []byte("// cli"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
			wantAvail:  true,
			wantSource: "runtime",
		},
		{
			name: "an empty runtime dir reports debris",
			prepare: func(t *testing.T) {
				t.Helper()
				if err := os.MkdirAll(staleDir, 0o755); err != nil {
					t.Fatal(err)
				}
			},
			wantAvail:   false,
			wantStale:   true,
			wantChecked: 3,
		},
		{
			name:       "a binary on the known path directories answers available",
			prepare:    func(*testing.T) {},
			onPath:     `C:\Users\someone\AppData\Roaming\npm\netlify.cmd`,
			wantAvail:  true,
			wantSource: "path",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Each case starts from a clean runtime tree.
			if err := os.RemoveAll(filepath.Join(dataDir, "runtime")); err != nil {
				t.Fatal(err)
			}
			netlifyPathSearch = func() string { return tt.onPath }
			t.Cleanup(func() { netlifyPathSearch = func() string { return "" } })
			tt.prepare(t)

			h := NewDashboardHandler(nil)
			rec := httptest.NewRecorder()
			h.HandleGetNetlifyCLIStatus(rec, httptest.NewRequest(http.MethodGet, "/api/proxy-pools/netlify-cli-status", nil))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
			}
			var body netlifyCLIStatusBody
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("unmarshal: %v (%s)", err, rec.Body.String())
			}
			if body.ManualCommand != "npm i -g netlify-cli" {
				t.Errorf("manualCommand = %q", body.ManualCommand)
			}
			if body.Available != tt.wantAvail {
				t.Fatalf("available = %v, want %v: %s", body.Available, tt.wantAvail, rec.Body.String())
			}
			if tt.wantAvail {
				if body.Source != tt.wantSource {
					t.Errorf("source = %q, want %q", body.Source, tt.wantSource)
				}
				if body.Path == "" {
					t.Error("path is empty")
				}
				return
			}
			if !body.AutoInstall {
				t.Error("autoInstall = false, want true on the not-installed branch")
			}
			if body.StaleInstall != tt.wantStale {
				t.Errorf("staleInstall = %v, want %v", body.StaleInstall, tt.wantStale)
			}
			if body.InstallURL == "" {
				t.Error("installUrl is empty")
			}
			if len(body.Checked) != tt.wantChecked {
				t.Errorf("checked = %d, want %d", len(body.Checked), tt.wantChecked)
			}
			for _, candidate := range body.Checked {
				if candidate.Source == "" || candidate.Path == "" {
					t.Errorf("incomplete candidate %+v", candidate)
				}
				if candidate.Exists {
					t.Errorf("candidate %s reported exists on the not-found branch", candidate.Source)
				}
			}
		})
	}
}