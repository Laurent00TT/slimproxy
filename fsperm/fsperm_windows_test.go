//go:build windows

package fsperm

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// TestRestrictSeversInheritance.
//
// The measured defect: os.WriteFile(path, body, 0o600) on Windows sets the
// read-only attribute and nothing else, so the file keeps its parent's
// inherited ACEs. On the development machine that meant a local group held
// ReadAndExecute over the inbound API keys and OAuth refresh tokens.
//
// Adding an ACE for the current user does not fix that -- the inherited entries
// remain. Only a protected DACL does, so that is what this asserts.
func TestRestrictSeversInheritance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret.yaml")
	if err := os.WriteFile(path, []byte("api-keys:\n  - k\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := Restrict(path); err != nil {
		t.Fatalf("Restrict: %v", err)
	}

	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("读回安全描述符失败: %v", err)
	}
	control, _, err := sd.Control()
	if err != nil {
		t.Fatalf("读取控制位失败: %v", err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		t.Error("DACL 未被标记为 protected：父目录的继承 ACE 仍然生效，" +
			"这正是本机上凭据可被其他组读取的原因")
	}

	// The file must still be usable by us afterwards -- a restriction that
	// locks out the process itself would take down the proxy.
	if _, err := os.ReadFile(path); err != nil {
		t.Errorf("收紧权限后自己读不了: %v", err)
	}
	if err := os.WriteFile(path, []byte("api-keys:\n  - k2\n"), 0o600); err != nil {
		t.Errorf("收紧权限后自己写不了: %v", err)
	}
}

// TestRestrictWorksOnDirectories: auths/ and logs/ are directories, and what
// gets created inside them later must inherit the restriction rather than the
// original parent's entries.
func TestRestrictWorksOnDirectories(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "auths")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Restrict(dir); err != nil {
		t.Fatalf("Restrict: %v", err)
	}

	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("读回安全描述符失败: %v", err)
	}
	control, _, err := sd.Control()
	if err != nil {
		t.Fatal(err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		t.Error("目录的 DACL 未被 protected")
	}

	// A credential written afterwards must still be creatable and readable.
	inner := filepath.Join(dir, "claude.json")
	if err := os.WriteFile(inner, []byte(`{"type":"claude"}`), 0o600); err != nil {
		t.Fatalf("收紧后无法在目录内创建文件: %v", err)
	}
	if _, err := os.ReadFile(inner); err != nil {
		t.Errorf("收紧后无法读取目录内的文件: %v", err)
	}
}

// TestRestrictReportsAMissingPath rather than silently doing nothing -- callers
// log the error, and a silent no-op here would recreate the exact class of bug
// this package exists to close.
func TestRestrictReportsAMissingPath(t *testing.T) {
	if err := Restrict(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("对不存在的路径应报错")
	}
}
