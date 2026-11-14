package dbbackup

import "testing"

// The repository field accepts every shape that appears in the wild; the owner
// comes from the URL when it carries one, otherwise from the username field.
func TestParseRepoRef(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		fallback string
		want     *RepoRef
	}{
		{"https url with .git", "https://github.com/jhopan12/AutoBuy.git", "", &RepoRef{"jhopan12", "AutoBuy"}},
		{"https url", "https://github.com/jhopan12/AutoBuy", "", &RepoRef{"jhopan12", "AutoBuy"}},
		{"ssh remote", "git@github.com:jhopan12/AutoBuy.git", "", &RepoRef{"jhopan12", "AutoBuy"}},
		{"bare host", "github.com/jhopan12/AutoBuy", "", &RepoRef{"jhopan12", "AutoBuy"}},
		{"owner slash repo", "jhopan12/AutoBuy", "", &RepoRef{"jhopan12", "AutoBuy"}},
		{"bare name uses the username", "AutoBuy", "jhopan12", &RepoRef{"jhopan12", "AutoBuy"}},
		{"trailing slash", "https://github.com/jhopan12/AutoBuy/", "", &RepoRef{"jhopan12", "AutoBuy"}},
		{"nested path takes the last two segments", "https://github.com/org/team/repo", "", &RepoRef{"team", "repo"}},
		{"bare name without a username has no owner", "AutoBuy", "", nil},
		{"empty", "", "jhopan12", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseRepoRef(tt.raw, tt.fallback)
			if tt.want == nil {
				if got != nil {
					t.Fatalf("ParseRepoRef(%q) = %+v, want nil", tt.raw, got)
				}
				return
			}
			if got == nil {
				t.Fatalf("ParseRepoRef(%q) = nil, want %+v", tt.raw, tt.want)
			}
			if *got != *tt.want {
				t.Errorf("ParseRepoRef(%q) = %+v, want %+v", tt.raw, got, tt.want)
			}
		})
	}
}

// The interval is a band, not a pass-through: an unset or nonsense value means
// the daily default, and a chosen value is clamped to [1, 168].
func TestResolveIntervalHours(t *testing.T) {
	tests := []struct {
		name string
		raw  any
		want int
	}{
		{"unset", nil, DefaultIntervalHours},
		{"json number 0", float64(0), DefaultIntervalHours},
		{"negative", float64(-5), DefaultIntervalHours},
		{"not a number", "soon", DefaultIntervalHours},
		{"below one hour", float64(0.5), 1},
		{"six hours", float64(6), 6},
		{"int six hours", 6, 6},
		{"numeric string", "12", 12},
		{"above a week", float64(200), MaxIntervalHours},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ResolveIntervalHours(tt.raw); got != tt.want {
				t.Errorf("ResolveIntervalHours(%v) = %d, want %d", tt.raw, got, tt.want)
			}
		})
	}
}

// A stored config parses the same whichever runtime wrote it: the new shape
// (bare repo + owner column) and the legacy "owner/name" both report an owner.
func TestConfigFromSettings(t *testing.T) {
	raw := map[string]any{
		"dbBackup": map[string]any{
			"enabled":         true,
			"intervalHours":   float64(12),
			"githubUser":      "alice",
			"githubRepo":      "backups",
			"githubRepoOwner": "alice",
			"githubToken":     "ghp_x",
		},
	}
	cfg := ConfigFromSettings(raw)
	if !cfg.Enabled || cfg.IntervalHours != 12 || cfg.GithubUser != "alice" || cfg.GithubToken != "ghp_x" {
		t.Fatalf("cfg = %+v, want the stored values", cfg)
	}
	if owner, repo := cfg.Reported(); owner != "alice" || repo != "backups" {
		t.Errorf("Reported() = %q/%q, want alice/backups", owner, repo)
	}

	legacy := ConfigFromSettings(map[string]any{
		"dbBackup": map[string]any{"githubUser": "bob", "githubRepo": "bob/legacy-repo"},
	})
	if owner, repo := legacy.Reported(); owner != "bob" || repo != "legacy-repo" {
		t.Errorf("legacy Reported() = %q/%q, want bob/legacy-repo", owner, repo)
	}
	if legacy.IntervalHours != DefaultIntervalHours || legacy.Enabled {
		t.Errorf("legacy cfg = %+v, want disabled with the default interval", legacy)
	}

	empty := ConfigFromSettings(nil)
	if owner, repo := empty.Reported(); owner != "" || repo != "" {
		t.Errorf("empty Reported() = %q/%q, want empty", owner, repo)
	}
}

// The repository name is a plain name: a value carrying a path separator or a
// space is refused before it reaches a clone URL. The accepted set is upstream's
// /^[\w.-]+$/ exactly, so a name one runtime accepts the other accepts too.
func TestRepoNameLooksValid(t *testing.T) {
	tests := map[string]bool{
		"backups":        true,
		"my.backup_repo": true,
		"repo-1":         true,
		"bad name":       false,
		"a/b":            false,
		"":               false,
	}
	for name, want := range tests {
		if got := RepoNameLooksValid(name); got != want {
			t.Errorf("RepoNameLooksValid(%q) = %v, want %v", name, got, want)
		}
	}
}
