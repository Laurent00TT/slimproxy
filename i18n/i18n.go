// Package i18n selects which language the operator sees.
//
// The mechanism is deliberately primitive: every user-facing string in this
// program is written twice, at the point it is used, as i18n.T("中文", "English").
// There is no message catalog, no key namespace, no external translation file.
// For a tool whose entire surface is a few hundred short strings, a catalog
// buys indirection -- the reader of runTunnelDown can no longer see what the
// operator will -- and costs a build step, without making a single string
// easier to translate. The known weakness of the inline form, strings that
// silently miss translation, is covered by a test instead: lint_test.go parses
// every non-test file and fails on any Chinese string literal outside a T call.
package i18n

import "sync/atomic"

// Lang identifies one supported interface language.
type Lang int32

const (
	// Zh is the zero value on purpose. Tests exercise packages without going
	// through main, and every existing assertion in this repository is written
	// against the Chinese text -- a zero value of En would have flipped some
	// four hundred of them the moment this package was linked in. Locale
	// detection is main's job precisely so that importing i18n has no
	// behavioural effect on its own.
	Zh Lang = iota
	En
)

// current is atomic not because the language changes at runtime -- it is set
// during startup, before the first goroutine that could read it -- but because
// "set early, read everywhere" is an invariant a future change can silently
// break, and the race detector should report that as contention on one value
// rather than as undefined behaviour scattered across every string lookup.
var current atomic.Int32

// Set selects the language. Call it from startup code only: strings already
// rendered do not re-render, and a language that changes mid-frame produces a
// panel that is half one and half the other.
func Set(l Lang) { current.Store(int32(l)) }

// Active reports the selected language.
func Active() Lang { return Lang(current.Load()) }

// T returns the string for the active language.
//
// Both texts sit at the call site, always in zh, en order. The argument order
// is load-bearing: the lint test identifies the Chinese argument by content,
// and a reviewer comparing the two translations must not have to check which
// is which per call.
func T(zh, en string) string {
	if Active() == En {
		return en
	}
	return zh
}

// NewError builds an error whose text follows the active language.
//
// For package-level sentinel errors. `var ErrX = errors.New(T(...))` would
// freeze whichever language was active at init -- which is always the default,
// because main has not run yet. Identity comparison via errors.Is is untouched:
// Is matches on the pointer, and there is still exactly one value per sentinel.
// The text is simply computed at the moment someone reads it, which is the
// moment the language is known.
func NewError(zh, en string) error { return &translatedError{zh: zh, en: en} }

type translatedError struct{ zh, en string }

func (e *translatedError) Error() string { return T(e.zh, e.en) }

// Parse maps a config value onto a language.
//
// Empty is valid and means "not stated", reported by ok=false so the caller
// falls through to locale detection. An unrecognised value is also ok=false
// rather than an error: the config validator owns rejecting bad values with an
// explanation, and this function must not force everything that touches a
// language string to carry an error path for a case the validator already
// rules out.
func Parse(s string) (Lang, bool) {
	switch s {
	case "zh":
		return Zh, true
	case "en":
		return En, true
	}
	return Zh, false
}

// FromLocale maps a system locale name (zh-CN, zh_CN.UTF-8, en-US, ...) onto
// the nearest supported language.
//
// Anything Chinese selects Chinese, everything else -- including the empty
// string a detection failure produces -- selects English. English is the wider
// net by design: an operator who reads neither language is better served by
// the one their tooling, search results and this project's upstream all speak.
func FromLocale(locale string) Lang {
	if len(locale) >= 2 && locale[0] == 'z' && locale[1] == 'h' {
		return Zh
	}
	return En
}
