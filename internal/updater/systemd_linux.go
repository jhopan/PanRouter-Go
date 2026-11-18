//go:build linux

package updater

import (
	"os"
	"strconv"
	"strings"
)

// underSystemd reports whether this process was started by systemd.
//
// It decides who restarts the gateway after a self-update. A supervised
// process must NOT spawn its own replacement: the supervisor already holds
// the unit, and a second process competes with it for the listening port
// while the supervisor is still counting the first one as alive.
//
// The check is deliberately narrow — the marker must be present AND name this
// process — so a shell that merely inherited the environment is not mistaken
// for a supervised run.
func underSystemd() bool {
	invocation := strings.TrimSpace(os.Getenv(systemdEnvMarker))
	if invocation == "" {
		return false
	}
	raw := strings.TrimSpace(os.Getenv(systemdPIDEnv))
	if raw == "" {
		// systemd sets INVOCATION_ID for every unit, including ones where
		// SYSTEMD_EXEC_PID is absent. The marker alone is enough to say a
		// supervisor started this process.
		return true
	}
	pid, err := strconv.Atoi(raw)
	if err != nil {
		return false
	}
	return pid == os.Getpid()
}
