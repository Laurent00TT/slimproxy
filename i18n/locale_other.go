//go:build !windows

package i18n

import "os"

// SystemLocale reports the user's locale from the POSIX environment, e.g.
// "zh_CN.UTF-8". Precedence follows the convention: LC_ALL overrides
// LC_MESSAGES overrides LANG.
func SystemLocale() string {
	for _, k := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}
