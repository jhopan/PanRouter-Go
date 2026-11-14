package media

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	json "encoding/json/v2"
)

// codexProfilesEnv points CODEX_HOME at a scratch directory, so nothing a test
// writes lands in the real ~/.codex.
func codexProfilesEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	return dir
}

func postCodexProfile(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/cli-tools/codex-profiles", strings.NewReader(body))
	NewCodexProfilesHandler().HandlePost(rec, req)
	return rec
}

func deleteCodexProfile(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/cli-tools/codex-profiles", strings.NewReader(body))
	NewCodexProfilesHandler().HandleDelete(rec, req)
	return rec
}

func listCodexProfiles(t *testing.T) []codexProfile {
	t.Helper()
	rec := httptest.NewRecorder()
	NewCodexProfilesHandler().HandleGet(rec, httptest.NewRequest(http.MethodGet, "/api/cli-tools/codex-profiles", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Profiles []codexProfile `json:"profiles"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("GET body is not JSON: %v (%s)", err, rec.Body.String())
	}
	return body.Profiles
}

// TestCodexProfilesRoundTrip pins the file contract: `codex -p <name>` runs the
// profile named after the file, and the two lines that decide what it points at
// are `model` and `model_provider`.
func TestCodexProfilesRoundTrip(t *testing.T) {
	dir := codexProfilesEnv(t)

	if got := listCodexProfiles(t); len(got) != 0 {
		t.Fatalf("a fresh directory listed %d profiles, want 0", len(got))
	}

	rec := postCodexProfile(t, `{"name":"Reviewer","model":"gpt-6-review"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	written, err := os.ReadFile(filepath.Join(dir, "reviewer.config.toml"))
	if err != nil {
		t.Fatalf("profile file: %v", err)
	}
	want := "# codex -p reviewer\nmodel = \"gpt-6-review\"\nmodel_provider = \"PanRouter\"\n"
	if string(written) != want {
		t.Errorf("file =\n%q\nwant\n%q", written, want)
	}

	profiles := listCodexProfiles(t)
	if len(profiles) != 1 {
		t.Fatalf("listed %d profiles, want 1", len(profiles))
	}
	if profiles[0].Name != "reviewer" || profiles[0].Model != "gpt-6-review" ||
		profiles[0].Command != "codex -p reviewer" {
		t.Errorf("profile = %+v", profiles[0])
	}

	if rec := deleteCodexProfile(t, `{"name":"reviewer"}`); rec.Code != http.StatusOK {
		t.Fatalf("DELETE = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "reviewer.config.toml")); !os.IsNotExist(err) {
		t.Errorf("profile file survived the delete: %v", err)
	}
	if rec := deleteCodexProfile(t, `{"name":"reviewer"}`); rec.Code != http.StatusNotFound {
		t.Errorf("DELETE of a missing profile = %d, want 404", rec.Code)
	}
}

// TestCodexProfilesListIgnoresTheActiveConfig keeps `config.toml` — the file a
// plain `codex` run reads — out of the profile list, and ignores anything that
// is not a `.config.toml`.
func TestCodexProfilesListIgnoresTheActiveConfig(t *testing.T) {
	dir := codexProfilesEnv(t)
	files := map[string]string{
		"config.toml":       "model = \"gpt-6\"\n",
		"alpha.config.toml": "model = \"alpha-model\"\n",
		"beta.config.toml":  "model_provider = \"PanRouter\"\n", // no model line
		"notes.toml":        "model = \"ignored\"\n",
		"zeta.config.toml":  "# nothing\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}

	got := listCodexProfiles(t)
	names := make([]string, 0, len(got))
	for _, p := range got {
		names = append(names, p.Name)
	}
	if strings.Join(names, ",") != "alpha,beta,zeta" {
		t.Fatalf("names = %v, want [alpha beta zeta]", names)
	}
	if got[0].Model != "alpha-model" {
		t.Errorf("alpha model = %q", got[0].Model)
	}
	// A profile with no model line lists an empty model rather than a guess:
	// the file is the source of truth and it does not name one.
	if got[1].Model != "" {
		t.Errorf("beta model = %q, want empty", got[1].Model)
	}
}

// TestCodexProfilesRejectsUnsafeNames pins the guard. The name becomes a file
// name, so a traversal, a reserved name or a shell-shaped value must never reach
// the disk.
func TestCodexProfilesRejectsUnsafeNames(t *testing.T) {
	dir := codexProfilesEnv(t)
	for _, name := range []string{"", "config", "CONFIG", "../escape", "a/b", "with space", "semi;colon", strings.Repeat("x", 65)} {
		rec := postCodexProfile(t, `{"name":`+quoteJSON(name)+`,"model":"gpt-6"}`)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("POST name=%q = %d, want 400: %s", name, rec.Code, rec.Body.String())
		}
		if rec := deleteCodexProfile(t, `{"name":`+quoteJSON(name)+`}`); rec.Code != http.StatusBadRequest {
			t.Errorf("DELETE name=%q = %d, want 400", name, rec.Code)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("a refused name wrote %d files into the profile directory", len(entries))
	}
}

// TestCodexProfilesRejectsUnsafeModel pins the second guard: the model is
// quoted into TOML, so a quote or a newline would break out of it.
func TestCodexProfilesRejectsUnsafeModel(t *testing.T) {
	codexProfilesEnv(t)
	for _, model := range []string{"", "   ", "a\"b", "a\nb", "a\rb", strings.Repeat("m", 257)} {
		rec := postCodexProfile(t, `{"name":"safe","model":`+quoteJSON(model)+`}`)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("POST model=%q = %d, want 400: %s", model, rec.Code, rec.Body.String())
		}
	}
}

// quoteJSON encodes a string as a JSON literal for the request bodies above.
func quoteJSON(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}
