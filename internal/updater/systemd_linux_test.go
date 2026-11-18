//go:build linux

package updater

import (
	"os"
	"strconv"
	"testing"
)

// TestUnderSystemd_NeedsTheMarkerAndThisPID pins the detection: a process is
// supervised only when systemd's marker is present AND names this process.
//
// The PID check is what keeps a shell that inherited the environment from
// looking supervised — such a shell would then hand its restart to a
// supervisor that is not watching it, and the gateway would simply exit.
func TestUnderSystemd_NeedsTheMarkerAndThisPID(t *testing.T) {
	tests := []struct {
		name       string
		invocation string
		execPID    string
		want       bool
	}{
		{name: "no marker at all", want: false},
		{name: "marker without a pid", invocation: "abc123", want: true},
		{name: "marker with this pid", invocation: "abc123", execPID: strconv.Itoa(os.Getpid()), want: true},
		{name: "marker with another pid", invocation: "abc123", execPID: strconv.Itoa(os.Getpid() + 1), want: false},
		{name: "pid without a marker", execPID: strconv.Itoa(os.Getpid()), want: false},
		{name: "unparsable pid", invocation: "abc123", execPID: "not-a-number", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(systemdEnvMarker, tt.invocation)
			t.Setenv(systemdPIDEnv, tt.execPID)
			if got := underSystemd(); got != tt.want {
				t.Errorf("underSystemd() = %v, want %v", got, tt.want)
			}
		})
	}
}
