package proxy

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
)

// Detection of protocol pairs that have no translator.
//
// The registry's behaviour for an unregistered pair is to return the body
// unchanged -- so a request written in one dialect is forwarded to an upstream
// speaking another, and whatever the upstream makes of it comes back as an
// ordinary error. Nothing says the translation was skipped. README called this
// out as a known property and the translate/ package was built as a "fail-loud
// facade" over it, but the serving path never goes through that facade:
// CLIProxyAPI's executors call sdktranslator directly. The guard existed in a
// package nothing on the request path imports.
//
// This is where it can actually be caught. The registry consults the
// TranslateRequest and TranslateResponse hooks *only* when it has no built-in
// transformer for the pair, so being called here is itself the signal -- no
// prediction of which pairs a deployment will use, and no startup assertion
// that could be wrong in either direction. The pair that silently passed
// through is the pair reported.

// seenUntranslated remembers which pairs have already been reported.
//
// One line per pair for the life of the process, not one per request: a client
// pointed at the wrong upstream produces a steady stream of these, and a log
// that repeats the same finding thousands of times buries everything else.
var seenUntranslated sync.Map

// noteUntranslated reports a pair with no registered transformer, once.
//
// from == to is excluded and is not a defect: a Claude client talking to a
// Claude upstream needs no translation, and the body passing through unchanged
// is the correct outcome. Warning about it would train the operator to ignore
// this message.
func noteUntranslated(from, to sdktranslator.Format, direction string) {
	if strings.EqualFold(string(from), string(to)) {
		return
	}
	key := direction + ":" + string(from) + "->" + string(to)
	if _, loaded := seenUntranslated.LoadOrStore(key, struct{}{}); loaded {
		return
	}
	log.Warnf("slimproxy: 没有为 %s → %s 注册%s翻译器，报文将原样转发。"+
		"上游会收到它不认识的格式，返回的错误看起来与协议无关。"+
		"运行 \"slimproxy routes\" 查看本构建支持哪些组合",
		from, to, direction)
}

// UntranslatedPairs returns the pairs observed passing through untranslated.
//
// Exported so a diagnostic can report what the process has actually seen,
// rather than what it predicts. Empty on a healthy deployment.
func UntranslatedPairs() []string {
	var out []string
	seenUntranslated.Range(func(k, _ any) bool {
		if s, ok := k.(string); ok {
			out = append(out, s)
		}
		return true
	})
	sort.Strings(out)
	return out
}

// UntranslatedSummary renders the observed pairs for display, or "" when there
// are none.
func UntranslatedSummary() string {
	pairs := UntranslatedPairs()
	if len(pairs) == 0 {
		return ""
	}
	return fmt.Sprintf("%d 个协议组合没有翻译器，报文被原样转发: %s",
		len(pairs), strings.Join(pairs, ", "))
}
