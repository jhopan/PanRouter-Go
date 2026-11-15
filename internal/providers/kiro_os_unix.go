//go:build linux || darwin

package providers

import (
	"strings"

	"golang.org/x/sys/unix"
)

// kiroOSRelease is the `os/<platform>#<release>` token: the kernel release the
// real client would report from os.release(). A user-agent token cannot carry
// whitespace, so any space in the release is folded to an underscore — a stray
// space would split the header into a bogus extra token.
func kiroOSRelease() string {
	var uts unix.Utsname
	if err := unix.Uname(&uts); err != nil {
		return "unknown"
	}
	return strings.ReplaceAll(strings.TrimRight(string(uts.Release[:]), "\x00"), " ", "_")
}
