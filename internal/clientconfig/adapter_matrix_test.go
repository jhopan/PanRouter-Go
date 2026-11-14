package clientconfig

import (
	"strings"
	"testing"
)

// S1–S3 from docs/specs/client-schema-matrix.md §8.4, run over every adapter.
// Per-adapter tests live beside their adapter; these are the ones that would
// catch a client added to the registry without the same care.

// fixtureFor returns a config for the client that already holds a key this
// package does not own, so S2 has something to check survives.
func fixtureFor(id string) string {
	switch id {
	case "claude", "opencode", "droid", "crush", "kilo", "pi":
		return "{\n  // user comment\n  \"keepMe\": \"yes\"\n}\n"
	case "cline":
		return "{\n  // user comment\n  \"keepMe\": \"yes\"\n}\n"
	case "codex":
		return "# user comment\nkeep_me = \"yes\"\n"
	case "omp":
		return "# user comment\nkeepMe: \"yes\"\n"
	case "codewhale", "forge", "deepseek-tui", "grok-build", "jcode":
		return "# user comment\nkeep_me = \"yes\"\n"
	case "copilot":
		// A root array with one entry this adapter does not own: it has to come
		// back unchanged, name and all.
		return "[{\"name\": \"keepMe\"}]\n"
	case "openclaw", "smelt":
		return "{\n  \"keepMe\": \"yes\"\n}\n"
	}
	return ""
}

func TestAdapterS1RoundTrip(t *testing.T) {
	opts := Options{BaseURL: "http://localhost:20130", APIKey: "sk-s1", Model: "m1"}
	for _, id := range IDs() {
		t.Run(id, func(t *testing.T) {
			home := t.TempDir()
			adapter, err := Resolve(id)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			paths, err := adapter.Paths(Options{Home: home})
			if err != nil {
				t.Fatalf("Paths: %v", err)
			}
			fixture := fixtureFor(id)
			for _, f := range paths.Files {
				writeFixture(t, f.Path, fixture)
			}

			o := opts
			o.Home = home
			if id == "omp" {
				o.Model = "" // omp writes no model entries (proxy discovery)
			}
			if _, err := adapter.Apply(o); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			for _, f := range paths.Files {
				if got := readFixture(t, f.Path); got == fixture {
					t.Errorf("%s: Apply changed nothing", f.Purpose)
				}
			}
			if _, err := adapter.Reset(o); err != nil {
				t.Fatalf("Reset: %v", err)
			}
			for _, f := range paths.Files {
				got := readFixture(t, f.Path)
				if n := strings.Count(got, "panrouter"); n > 0 && id != "kilo" {
					t.Errorf("%s: Reset left panrouter behind:\n%s", f.Purpose, got)
				}
			}
		})
	}
}

func TestAdapterS2PreservesForeignContent(t *testing.T) {
	opts := Options{BaseURL: "http://h:1", APIKey: "k", Model: "m"}
	for _, id := range IDs() {
		t.Run(id, func(t *testing.T) {
			home := t.TempDir()
			adapter, _ := Resolve(id)
			paths, err := adapter.Paths(Options{Home: home})
			if err != nil {
				t.Fatalf("Paths: %v", err)
			}
			fixture := fixtureFor(id)
			for _, f := range paths.Files {
				writeFixture(t, f.Path, fixture)
			}
			o := opts
			o.Home = home
			if _, err := adapter.Apply(o); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			for _, f := range paths.Files {
				got := readFixture(t, f.Path)
				for _, want := range []string{"user comment", "keepMe", "keep_me", "keepMe"} {
					if strings.Contains(fixture, want) && !strings.Contains(got, want) {
						t.Errorf("%s: Apply lost %q:\n%s", f.Purpose, want, got)
					}
				}
			}
		})
	}
}

func TestAdapterS3RefusesMalformed(t *testing.T) {
	broken := map[string]string{
		"claude": "{\"a\": ", "opencode": "{\"a\": ", "droid": "{\"a\": ",
		"crush": "{\"a\": ", "kilo": "{\"a\": ", "pi": "{\"a\": ",
		"cline": "{\"a\": ", "codex": "model = \n[[[", "omp": "\t\tbad: [\n",
		"codewhale": "a = \n[[[", "forge": "a = \n[[[", "deepseek-tui": "a = \n[[[",
		"grok-build": "a = \n[[[", "jcode": "a = \n[[[",
		"copilot": "[{\"name\": ", "openclaw": "{\"a\": ", "smelt": "{\"a\": ",
		"cowork": "{\"a\": ",
	}
	opts := Options{BaseURL: "http://h:1", APIKey: "k", Model: "m"}
	for _, id := range IDs() {
		t.Run(id, func(t *testing.T) {
			home := t.TempDir()
			adapter, _ := Resolve(id)
			paths, err := adapter.Paths(Options{Home: home})
			if err != nil {
				t.Fatalf("Paths: %v", err)
			}
			if len(paths.Files) == 0 {
				// devin owns no file, so there is no malformed input it could
				// refuse. The adapter is a detection-only client.
				t.Skip("adapter owns no config file")
			}
			for _, f := range paths.Files {
				writeFixture(t, f.Path, broken[id])
			}
			o := opts
			o.Home = home
			if _, err := adapter.Apply(o); err == nil {
				t.Fatal("Apply accepted a malformed file")
			}
			for _, f := range paths.Files {
				if got := readFixture(t, f.Path); got != broken[id] {
					t.Errorf("%s: the file changed despite the error:\n%s", f.Purpose, got)
				}
			}
		})
	}
}

// The registry is the thing `--tools` validates against, so its size and
// aliases are checked rather than assumed.
func TestRegistry(t *testing.T) {
	ids := IDs()
	if len(ids) != 19 {
		t.Fatalf("got %d adapters, want 19 (nine `connect` clients plus the ten dashboard-only clients)", len(ids))
	}
	for _, a := range []string{"claude-code", "claudecode", "factory", "kilocode", "pi-coding-agent", "oh-my-pi"} {
		if _, err := Resolve(a); err != nil {
			t.Errorf("alias %q does not resolve: %v", a, err)
		}
	}
	if _, err := Resolve("nope"); err == nil {
		t.Error("an unknown tool resolved")
	}
	all, err := ResolveAll([]string{"all"})
	if err != nil {
		t.Fatalf("ResolveAll(all): %v", err)
	}
	if len(all) != len(ids) {
		t.Errorf(`"all" expanded to %d, want %d`, len(all), len(ids))
	}
	dup, err := ResolveAll([]string{"claude", "claude-code", "claude"})
	if err != nil {
		t.Fatalf("ResolveAll: %v", err)
	}
	if len(dup) != 1 {
		t.Errorf("duplicate spellings produced %d adapters, want 1", len(dup))
	}
}
