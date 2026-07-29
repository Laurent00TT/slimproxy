//go:build !windows

package proxy

import (
	"os"
	"testing"
)

// assertRestricted checks that path is not readable by group or other.
//
// The Windows build asserts a protected DACL instead; on Unix the mode is the
// whole story, and fsperm.Restrict is a chmod.
func assertRestricted(t *testing.T, path string) {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("%s 的权限是 %04o，组和其他用户仍可访问，而这个目录里是逐字的请求正文", path, perm)
	}
}
