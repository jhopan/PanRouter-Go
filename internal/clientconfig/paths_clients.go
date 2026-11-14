package clientconfig

import (
	"os"
	"path/filepath"
	"runtime"
)

// Path resolution for the clients whose upstream dashboard routes
// (src/app/api/cli-tools/<client>-settings) existed before this package had an
// adapter for them: codewhale, copilot, cowork, deepseek-tui, devin, forge,
// grok-build, jcode, openclaw, smelt.
//
// Every path and every environment override is the one its upstream route reads
// and writes, so the file this patches is the file the CLI itself opens. Like
// paths.go, each resolver takes the home directory as an argument; the ones that
// upstream derives from an environment variable read that variable here, at
// call time, because the route does the same.

// codeWhalePaths is ~/.codewhale/config.toml.
func codeWhalePaths(home string) Paths {
	dir := filepath.Join(home, ".codewhale")
	return Paths{
		Dir:   dir,
		Files: []TargetFile{{Path: filepath.Join(dir, "config.toml"), Dir: dir, Format: FormatTOML, Purpose: "config"}},
	}
}

// copilotPaths is VS Code's chatLanguageModels.json. VS Code keeps it in a
// platform-specific user directory and the upstream route mirrors that split:
// %APPDATA%\Code\User on Windows, ~/Library/Application Support/Code/User on
// macOS, ~/.config/Code/User elsewhere. It deliberately does not honour
// $XDG_CONFIG_HOME — VS Code does not either.
func copilotPaths(home string) Paths {
	var base string
	switch runtime.GOOS {
	case "windows":
		base = os.Getenv("APPDATA")
		if base == "" {
			base = filepath.Join(home, "AppData", "Roaming")
		}
		base = filepath.Join(base, "Code", "User")
	case "darwin":
		base = filepath.Join(home, "Library", "Application Support", "Code", "User")
	default:
		base = filepath.Join(home, ".config", "Code", "User")
	}
	return Paths{
		Dir:   base,
		Files: []TargetFile{{Path: filepath.Join(base, "chatLanguageModels.json"), Dir: base, Format: FormatJSONC, Purpose: "config"}},
	}
}

// coworkCandidates lists the Claude Desktop data directories in the order the
// upstream route probes them. Only the first one is written.
func coworkCandidates(home string) []string {
	switch runtime.GOOS {
	case "darwin":
		base := filepath.Join(home, "Library", "Application Support")
		return []string{filepath.Join(base, "Claude-3p"), filepath.Join(base, "Claude")}
	case "windows":
		localApp := os.Getenv("LOCALAPPDATA")
		if localApp == "" {
			localApp = filepath.Join(home, "AppData", "Local")
		}
		roaming := os.Getenv("APPDATA")
		if roaming == "" {
			roaming = filepath.Join(home, "AppData", "Roaming")
		}
		return []string{
			filepath.Join(localApp, "Claude-3p"),
			filepath.Join(roaming, "Claude-3p"),
			filepath.Join(localApp, "Claude"),
			filepath.Join(roaming, "Claude"),
		}
	}
	return []string{
		filepath.Join(home, ".config", "Claude-3p"),
		filepath.Join(home, ".config", "Claude"),
	}
}

// coworkFirstPartyRoot is where Claude Desktop's own config sits (the file whose
// deploymentMode switches it to 3p mode).
func coworkFirstPartyRoot(home string) string {
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Claude")
	case "windows":
		roaming := os.Getenv("APPDATA")
		if roaming == "" {
			roaming = filepath.Join(home, "AppData", "Roaming")
		}
		return filepath.Join(roaming, "Claude")
	}
	return filepath.Join(home, ".config", "Claude")
}

// coworkRoots returns the directory cowork writes to and the one it reads an
// existing _meta.json from. Upstream reads from the first candidate that already
// has a configLibrary and writes to the first candidate unconditionally.
func coworkRoots(home string) (writeRoot, readRoot string) {
	candidates := coworkCandidates(home)
	writeRoot = candidates[0]
	readRoot = candidates[0]
	for _, dir := range candidates {
		if info, err := os.Stat(filepath.Join(dir, "configLibrary")); err == nil && info.IsDir() {
			readRoot = dir
			break
		}
	}
	return writeRoot, readRoot
}

// coworkPaths lists the three files that exist for every apply. The per-profile
// configLibrary/<appliedId>.json depends on what _meta.json holds, so the
// adapter adds it once it has read the id.
func coworkPaths(home string) Paths {
	writeRoot, _ := coworkRoots(home)
	configDir := filepath.Join(writeRoot, "configLibrary")
	firstParty := coworkFirstPartyRoot(home)
	return Paths{
		Dir: writeRoot,
		Files: []TargetFile{
			{Path: filepath.Join(configDir, "_meta.json"), Dir: configDir, Format: FormatJSONC, Purpose: "meta"},
			{Path: filepath.Join(writeRoot, "config.json"), Dir: writeRoot, Format: FormatJSONC, Purpose: "config"},
			{Path: filepath.Join(firstParty, "claude_desktop_config.json"), Dir: firstParty, Format: FormatJSONC, Purpose: "desktop-config"},
		},
	}
}

// deepSeekTUIPaths is ~/.deepseek/config.toml.
func deepSeekTUIPaths(home string) Paths {
	dir := filepath.Join(home, ".deepseek")
	return Paths{
		Dir:   dir,
		Files: []TargetFile{{Path: filepath.Join(dir, "config.toml"), Dir: dir, Format: FormatTOML, Purpose: "config"}},
	}
}

// devinPaths is empty on purpose: upstream's devin route detects the binary and
// reports its version, and writes no configuration at all — "the binary handles
// its own auth".
func devinPaths(string) Paths { return Paths{} }

// forgePaths is ~/.forge/config.toml.
func forgePaths(home string) Paths {
	dir := filepath.Join(home, ".forge")
	return Paths{
		Dir:   dir,
		Files: []TargetFile{{Path: filepath.Join(dir, "config.toml"), Dir: dir, Format: FormatTOML, Purpose: "config"}},
	}
}

// grokBuildPaths is ~/.grok/config.toml.
func grokBuildPaths(home string) Paths {
	dir := filepath.Join(home, ".grok")
	return Paths{
		Dir:   dir,
		Files: []TargetFile{{Path: filepath.Join(dir, "config.toml"), Dir: dir, Format: FormatTOML, Purpose: "config"}},
	}
}

// jcodePaths is two files: ~/.jcode/config.toml holds the provider profile, and
// $XDG_CONFIG_HOME/jcode/provider-9router.env holds the key the profile
// references through api_key_env. The env file is ordered second because the
// config has to name the key before it exists.
func jcodePaths(home string) Paths {
	configDir := filepath.Join(home, ".jcode")
	envDir := filepath.Join(xdgConfigHome(home), "jcode")
	return Paths{
		Dir: configDir,
		Files: []TargetFile{
			{Path: filepath.Join(configDir, "config.toml"), Dir: configDir, Format: FormatTOML, Purpose: "config"},
			// The env file is dotenv, which is TOML's root key/value grammar
			// without the sections: KEY="value" lines. Reusing the TOML writer
			// keeps the header comment, the user's other keys and the fail-closed
			// parse behaviour without a fourth patch dialect.
			{Path: filepath.Join(envDir, "provider-9router.env"), Dir: envDir, Format: FormatTOML, Purpose: "provider-env"},
		},
	}
}

// openClawPaths is ~/.openclaw/openclaw.json. Per-agent models.json files live
// under each agent's own agentDir, which is only known once the settings file
// has been read, so the adapter adds those plans itself.
func openClawPaths(home string) Paths {
	dir := filepath.Join(home, ".openclaw")
	return Paths{
		Dir:   dir,
		Files: []TargetFile{{Path: filepath.Join(dir, "openclaw.json"), Dir: dir, Format: FormatJSONC, Purpose: "settings"}},
	}
}

// smeltPaths is ~/.smelt/config.json.
func smeltPaths(home string) Paths {
	dir := filepath.Join(home, ".smelt")
	return Paths{
		Dir:   dir,
		Files: []TargetFile{{Path: filepath.Join(dir, "config.json"), Dir: dir, Format: FormatJSONC, Purpose: "config"}},
	}
}
