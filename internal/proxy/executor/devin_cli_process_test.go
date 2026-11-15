package executor

import (
	"runtime"
	"strings"
	"testing"
)

// TestDevinCLIBinaryPrefersExplicitOverride pins the first arm of the binary
// resolution: CLI_DEVIN_BIN is the operator's override and is used verbatim,
// even when it names nothing on disk (the spawn then reports that path).
func TestDevinCLIBinaryPrefersExplicitOverride(t *testing.T) {
	t.Setenv("CLI_DEVIN_BIN", "  /opt/devin/bin/devin  ")
	if got := devinCLIBinary(); got != "/opt/devin/bin/devin" {
		t.Errorf("devinCLIBinary() = %q, want the trimmed override", got)
	}
}

// TestDevinCLIBinaryCandidatesIncludeTheInstallerPaths pins the Windows arm the
// PATH lookup cannot cover: the official installer writes into
// %LOCALAPPDATA%, and a daemon started without a login shell does not have it.
func TestDevinCLIBinaryCandidatesIncludeTheInstallerPaths(t *testing.T) {
	candidates := devinCLIBinaryCandidates()
	if len(candidates) == 0 {
		t.Fatal("no candidates at all")
	}
	if runtime.GOOS != "windows" {
		return
	}
	want := []string{
		"devin\\cli\\bin\\devin.exe",
		".local\\bin\\devin.exe",
		"scoop\\shims\\devin.exe",
		"Programs\\devin\\devin.exe",
	}
	for _, suffix := range want {
		found := false
		for _, candidate := range candidates {
			if strings.HasSuffix(strings.ToLower(candidate), strings.ToLower(suffix)) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("candidate list lacks %q: %v", suffix, candidates)
		}
	}
}

// TestDevinCLIProcessSpec pins the argv and environment one child is started
// with: the default agent, the env override, and the permission mode that keeps
// the agent from blocking on its first tool call.
func TestDevinCLIProcessSpec(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		t.Setenv("CLI_DEVIN_BIN", "/opt/devin/bin/devin")
		t.Setenv("CLI_DEVIN_AGENT_TYPE", "")
		t.Setenv("DEVIN_PERMISSION_MODE", "")
		spec := devinCLIProcessSpec(t.TempDir(), "")
		if strings.Join(spec.Args, " ") != "acp" {
			t.Errorf("argv = %v, want the full agent by default", spec.Args)
		}
		if !hasEnv(spec.Env, "DEVIN_PERMISSION_MODE=bypass") {
			t.Error("DEVIN_PERMISSION_MODE defaults to bypass: without it the agent blocks on the first tool prompt")
		}
		if hasEnvPrefix(spec.Env, "XDG_CONFIG_HOME=") {
			t.Error("XDG_CONFIG_HOME was set with no MCP servers to point it at")
		}
		if !hasEnvPrefix(spec.Env, "PATH=") && runtime.GOOS != "windows" {
			t.Error("the child's environment does not inherit the parent's PATH")
		}
	})

	t.Run("env overrides", func(t *testing.T) {
		t.Setenv("CLI_DEVIN_BIN", "/opt/devin/bin/devin")
		t.Setenv("CLI_DEVIN_AGENT_TYPE", "summarizer")
		t.Setenv("DEVIN_PERMISSION_MODE", "accept-edits")
		spec := devinCLIProcessSpec(t.TempDir(), "/tmp/devin-mcp-x")
		if strings.Join(spec.Args, " ") != "acp --agent-type summarizer" {
			t.Errorf("argv = %v, want the summarizer agent type", spec.Args)
		}
		if !hasEnv(spec.Env, "DEVIN_PERMISSION_MODE=accept-edits") {
			t.Errorf("environment lacks the operator's permission mode: %v", spec.Env)
		}
		if countEnvPrefix(spec.Env, "DEVIN_PERMISSION_MODE=") != 1 {
			t.Errorf("DEVIN_PERMISSION_MODE is set more than once: %v", spec.Env)
		}
		if !hasEnv(spec.Env, "XDG_CONFIG_HOME=/tmp/devin-mcp-x") {
			t.Error("the throwaway MCP config directory did not reach the child")
		}
	})
}

// TestDevinCLIStderrKeepsABoundedTail pins the stderr capture: the tail is what
// a failed turn quotes, and the buffer never grows past its bound however much
// the child narrates.
func TestDevinCLIStderrKeepsABoundedTail(t *testing.T) {
	capture := newDevinCLIStderr()
	noise := strings.Repeat("x", devinCLIStderrLimit*2)
	capture.drain(strings.NewReader(noise + "final line\n"))
	capture.wait()

	tail := capture.tail()
	if !strings.HasSuffix(tail, "final line") {
		t.Errorf("tail does not end with the last thing the child said: %q", tail)
	}
	if len(tail) > devinCLIStderrLimit {
		t.Errorf("tail is %d bytes, want at most %d", len(tail), devinCLIStderrLimit)
	}
	if got := devinCLIOneLine("line one\nline two"); got != "line one line two" {
		t.Errorf("devinCLIOneLine() = %q, want one collapsed line", got)
	}
	if got := devinCLIOneLine(strings.Repeat("y", 1000)); len(got) > 410 {
		t.Errorf("collapsed line is %d bytes, want it bounded", len(got))
	}
}

// TestDevinCLIExitCodeReadsBothShapes pins the two ways a child's status
// arrives, so a failure message reads the same either way.
func TestDevinCLIExitCodeReadsBothShapes(t *testing.T) {
	if got := devinCLIExitCode(&devinCLIProcessExit{code: 3}); got != 3 {
		t.Errorf("devinCLIExitCode(lane exit) = %d, want 3", got)
	}
	if got := devinCLIExitCode(errNoStatus{}); got != -1 {
		t.Errorf("devinCLIExitCode(unknown) = %d, want -1", got)
	}
	message := devinCLIChildFailure(&devinCLIProcessExit{code: 7}, nil).Error()
	if !strings.Contains(message, "code 7") {
		t.Errorf("failure message = %q, want the exit code", message)
	}
}

type errNoStatus struct{}

func (errNoStatus) Error() string { return "no status" }

func hasEnv(env []string, entry string) bool {
	for _, candidate := range env {
		if candidate == entry {
			return true
		}
	}
	return false
}

func hasEnvPrefix(env []string, prefix string) bool {
	return countEnvPrefix(env, prefix) > 0
}

func countEnvPrefix(env []string, prefix string) int {
	count := 0
	for _, candidate := range env {
		if strings.HasPrefix(candidate, prefix) {
			count++
		}
	}
	return count
}
