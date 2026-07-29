//go:build !windows

package fsperm

import (
	"fmt"
	"os"

	"github.com/Laurent00TT/slimproxy/i18n"
)

// restrict removes group and other access.
//
// The permission bits passed to os.WriteFile and os.MkdirAll apply only when
// the object is created, so a file that already exists keeps whatever mode it
// had -- including a mode from an earlier, more permissive version of this
// program. This tightens it either way.
func restrict(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf(i18n.T("无法读取 %s 的信息: %w", "cannot stat %s: %w"), path, err)
	}
	mode := os.FileMode(0o600)
	if info.IsDir() {
		// Directories need the execute bit to be traversable at all.
		mode = 0o700
	}
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf(i18n.T("收紧 %s 的权限失败: %w", "tightening permissions on %s failed: %w"), path, err)
	}
	return nil
}
