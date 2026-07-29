package proxy

import (
	"testing"

	"golang.org/x/sys/windows"
)

// assertRestricted checks that path no longer inherits ACEs from its parent.
//
// Windows is where this matters: Go maps a 0700 mode to the read-only
// attribute and nothing else, so the only durable evidence that a directory was
// hardened is a protected DACL. Same check the fsperm package makes -- repeated
// here because what is being pinned is different: not that Restrict works, but
// that Build reaches it for a request-log directory that already existed.
func assertRestricted(t *testing.T, path string) {
	t.Helper()

	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("读回 %s 的安全描述符失败: %v", path, err)
	}
	control, _, err := sd.Control()
	if err != nil {
		t.Fatalf("读取控制位失败: %v", err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		t.Errorf("%s 的 DACL 未被标记为 protected：父目录的继承 ACE 仍然生效，"+
			"而这个目录里是逐字的请求正文", path)
	}
}
