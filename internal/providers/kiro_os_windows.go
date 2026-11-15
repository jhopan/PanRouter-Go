//go:build windows

package providers

import (
	"strconv"

	"golang.org/x/sys/windows"
)

// kiroOSRelease is the `os/<platform>#<release>` token. The real client reports
// os.release(), which on Windows is the kernel version the AWS SDK reads from the
// OS; RtlGetVersion is the same source without manifest shimming, so an old
// manifest cannot make the host claim a version it is not running.
func kiroOSRelease() string {
	info := windows.RtlGetVersion()
	release := strconv.FormatUint(uint64(info.MajorVersion), 10) + "." +
		strconv.FormatUint(uint64(info.MinorVersion), 10) + "." +
		strconv.FormatUint(uint64(info.BuildNumber), 10)
	return release
}
