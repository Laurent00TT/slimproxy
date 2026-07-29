package i18n

import "testing"

// TestZeroValueIsChinese pins the default that every other package's tests
// stand on. Some four hundred assertions in this repository compare against
// Chinese text without ever calling Set; if the zero value drifts to English
// they all fail at once, and this test says why before they do.
func TestZeroValueIsChinese(t *testing.T) {
	t.Cleanup(func() { Set(Zh) })
	Set(Zh) // restore in case an earlier test in the package moved it
	if got := T("中文", "english"); got != "中文" {
		t.Fatalf("零值语言下 T 返回 %q，测试生态建立在默认中文之上", got)
	}
}

func TestSetSelects(t *testing.T) {
	t.Cleanup(func() { Set(Zh) })
	Set(En)
	if got := T("中文", "english"); got != "english" {
		t.Fatalf("Set(En) 后 T 返回 %q", got)
	}
}

func TestParse(t *testing.T) {
	cases := []struct {
		in   string
		want Lang
		ok   bool
	}{
		{"zh", Zh, true},
		{"en", En, true},
		{"", Zh, false},        // not stated → caller falls back to locale
		{"zh-CN", Zh, false},   // config takes the short form only
		{"english", Zh, false}, // validator's job to reject, not ours
	}
	for _, tc := range cases {
		got, ok := Parse(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("Parse(%q) = (%v,%v)，应为 (%v,%v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// TestFromLocale covers the shapes real systems produce, not just the tidy
// ones: BCP-47 with region, POSIX with codeset, script subtags, and the empty
// string a failed detection yields.
func TestFromLocale(t *testing.T) {
	zh := []string{"zh-CN", "zh_CN.UTF-8", "zh-Hant-TW", "zh"}
	en := []string{"en-US", "en_GB.UTF-8", "ja-JP", "de_DE", "C", "POSIX", ""}
	for _, l := range zh {
		if FromLocale(l) != Zh {
			t.Errorf("FromLocale(%q) 应为中文", l)
		}
	}
	for _, l := range en {
		if FromLocale(l) != En {
			t.Errorf("FromLocale(%q) 应为英文（含检测失败的空串）", l)
		}
	}
}
