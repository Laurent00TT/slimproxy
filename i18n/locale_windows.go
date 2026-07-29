//go:build windows

package i18n

import (
	"os"
	"syscall"
	"unsafe"
)

// SystemLocale reports the user's display locale, e.g. "zh-CN".
//
// The POSIX variables are consulted first, Windows second. Backwards at first
// sight -- this is Windows -- but the operator running this under Git Bash or
// MSYS with LANG exported has stated a preference more specific than the OS
// install language, and the OS answer is what the fallback is for, not what
// overrides an explicit setting.
func SystemLocale() string {
	if env := localeFromEnv(); env != "" {
		return env
	}
	// GetUserDefaultLocaleName writes a NUL-terminated UTF-16 name like
	// "zh-CN". LOCALE_NAME_MAX_LENGTH is 85.
	buf := make([]uint16, 85)
	n, _, _ := procGetUserDefaultLocaleName.Call(
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n == 0 {
		// Failure deliberately yields "", which FromLocale maps to English.
		return ""
	}
	// n counts the terminating NUL; slice it off.
	return syscall.UTF16ToString(buf[:n-1])
}

var procGetUserDefaultLocaleName = syscall.NewLazyDLL("kernel32.dll").NewProc("GetUserDefaultLocaleName")

func localeFromEnv() string {
	for _, k := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}
