package daemon

import (
	"io"
	stdlog "log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAttachConsoleLogMirrorsIntoTheRunLog pins the fix for a supervised gateway
// losing its console.
//
// A Windows scheduled task hands the process no writable stdout, so every line
// the operator reads — which account served the request, how many tokens went in
// and out — went nowhere, and `panrouter-go logs` showed a file that had stopped
// at the previous run's "Server stopped gracefully". The tee puts the process's
// own output into that same file, so the file answers whichever way the gateway
// was started.
func TestAttachConsoleLogMirrorsIntoTheRunLog(t *testing.T) {
	t.Setenv("DATA_DIR", t.TempDir())
	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		t.Fatalf("create run dir: %v", err)
	}

	// The stdlib logger is global; capture the previous destination so the test
	// cannot leak its own file into whatever runs next.
	previous := stdlog.Writer()
	stdlog.SetOutput(io.Discard)
	t.Cleanup(func() { stdlog.SetOutput(previous) })

	detach, err := AttachConsoleLog()
	if err != nil {
		t.Fatalf("AttachConsoleLog: %v", err)
	}

	const line = "[21:11:36] 🟡 ▶ POST th/x → tokenharbor/x · ACC:Key 2 · REQ:9c6d61"
	stdlog.Print(line)

	data, err := os.ReadFile(LogPath())
	if err != nil {
		t.Fatalf("read run log: %v", err)
	}
	if !strings.Contains(string(data), line) {
		t.Errorf("run log does not carry the console line:\n%s", data)
	}

	// The detach restores the previous destination: a process that keeps logging
	// after it must not write into a closed file.
	detach()
	if got := stdlog.Writer(); got != previous {
		t.Errorf("Writer() after detach = %v, want the previous destination", got)
	}
	if err := os.WriteFile(filepath.Join(Dir(), "probe"), []byte("x"), 0o644); err != nil {
		t.Fatalf("run dir is not writable after detach: %v", err)
	}
}
