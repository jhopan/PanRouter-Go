//go:build !linux

package updater

// underSystemd reports whether this process was started by systemd.
//
// There is no systemd on the other platforms, so the answer is always no and
// the self-restart keeps spawning its own replacement.
func underSystemd() bool {
	return false
}
