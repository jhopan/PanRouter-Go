package updater

// The self-restart after a self-update.
//
// The defect this pins was invisible until the update ran on a real gateway:
// the restart resolved the running executable AFTER the swap. On Linux
// /proc/self/exe is the inode being executed, so once the swap renamed the
// binary to `.old` the link followed it there — and the swap deletes that
// `.old` before the restart looks the path up. The restart failed with
// "lstat …/panrouter-go.old: no such file or directory", so the binary on
// disk was the new one while the process kept running the old one.
//
// The supervisor question lives in the same file because it decides what the
// restart does with the path: a supervised process hands the restart back to
// its supervisor instead of spawning a replacement, and under systemd a
// spawned replacement is a second process in the same cgroup competing for
// the listening port.

import (
	"bytes"
	"net/http"
	"os"
	"testing"
)

// TestPerformSelfUpdate_RecordsTheInstalledPath pins the first defect: the
// path recorded for the restart is the one the new binary was written to, and
// it still resolves after the swap deleted the `.old` name.
//
// The stub stands in for the running executable, and the swap renames it
// exactly as the real one does — so the recorded path is read back from disk
// afterwards, which is what the restart does.
func TestPerformSelfUpdate_RecordsTheInstalledPath(t *testing.T) {
	stub := stubRunningBinary(t, []byte("ORIGINAL_RUNNING_BINARY"))
	// The payload has to clear the archive heuristics (score >= 3 and more
	// than 1 KiB) or the extraction falls back to the raw archive and the
	// digest would be of the wrong bytes.
	payload := bytes.Repeat([]byte("NEW_BINARY_PAYLOAD"), 200)
	server := releaseServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write(updateAssetTarGz(t, payload))
	})

	if err := PerformSelfUpdate(server.URL+"/panrouter-go_linux_amd64.tar.gz", ComputeSHA256(payload)); err != nil {
		t.Fatalf("PerformSelfUpdate: %v", err)
	}

	path, ok := UpdatedBinaryPath()
	if !ok {
		t.Fatal("UpdatedBinaryPath reported no update; the restart would fall back to the running inode")
	}
	if path != stub {
		t.Errorf("UpdatedBinaryPath = %q, want %q (the path the new binary was written to)", path, stub)
	}
	// The whole point: the recorded path must still resolve. The old name
	// does not — the swap deleted it.
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the recorded path does not resolve: %v", err)
	}
	if _, err := os.Stat(path + ".old"); err == nil {
		t.Error("the .old name survived the swap; the restart would have resolved the stale path")
	}
}

// TestUpdatedBinaryPath_EmptyBeforeAnyUpdate pins that the accessor reports
// "no update yet" rather than an empty path a restart would try to run.
func TestUpdatedBinaryPath_EmptyBeforeAnyUpdate(t *testing.T) {
	installedPathMu.Lock()
	previous := updatedBinaryPath
	updatedBinaryPath = ""
	installedPathMu.Unlock()
	t.Cleanup(func() {
		installedPathMu.Lock()
		updatedBinaryPath = previous
		installedPathMu.Unlock()
	})

	if path, ok := UpdatedBinaryPath(); ok {
		t.Errorf("UpdatedBinaryPath = %q, ok=true; want no path before any update", path)
	}
}

// TestSystemdRestartExitStatusIsTheReservedOne pins the number the process
// exits with.
//
// 256 is the status systemd reserves for a deliberate restart request; a
// different value would make the supervisor treat a planned restart as a
// crash. The unit side of the same number is pinned in the service package.
func TestSystemdRestartExitStatusIsTheReservedOne(t *testing.T) {
	if SystemdRestartExitStatus != 256 {
		t.Errorf("SystemdRestartExitStatus = %d, want 256 (the status systemd reserves for a restart request)", SystemdRestartExitStatus)
	}
}
