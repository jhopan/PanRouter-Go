//go:build !linux && !darwin && !windows

package providers

import "runtime"

// kiroOSRelease has no portable source without a per-platform syscall, so a
// target outside the release matrix reports its GOOS rather than inventing a
// kernel version. No shipped artifact takes this path.
func kiroOSRelease() string {
	return runtime.GOOS
}
