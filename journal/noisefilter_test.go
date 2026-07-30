package journal

import (
	"testing"
	"time"
)

// writeEvents lays down a day file the reader can be pointed at.
func writeEvents(t *testing.T, events ...Event) string {
	t.Helper()
	dir := t.TempDir()
	w, err := Open(dir, 7)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for _, e := range events {
		w.Append(e)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return dir
}

// externalReject is a refused request from the internet -- what Noise() matches.
func externalReject(status int, path string) Event {
	return Event{
		At: time.Now(), Kind: KindReject, Status: status,
		Path: path, Method: "POST", Src: SourceTunnel, Count: 1,
	}
}

func localReject(status int, path string) Event {
	return Event{
		At: time.Now(), Kind: KindReject, Status: status,
		Path: path, Method: "GET", Src: SourceLocal, Count: 1,
	}
}

// TestExplicitStatusSearchFindsExternalRejects is the regression.
//
// `log -status 401 -since 72h` returned an empty list and a footnote about
// scanner noise, while the 401s being asked for were inside that count: every
// external 4xx reject goes into the same noise bucket regardless of which
// status the operator asked for, so an external 401 and a favicon 404 were
// indistinguishable. Someone checking whether their key was being probed was
// told there was nothing to see.
func TestExplicitStatusSearchFindsExternalRejects(t *testing.T) {
	dir := writeEvents(t,
		externalReject(401, "/v1/messages"), // what the operator is looking for
		externalReject(404, "/favicon.ico"), // genuine scanner noise
		externalReject(404, "/api/hello"),
	)

	res, err := Read(dir, Query{Status: 401})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	if len(res.Events) == 0 {
		t.Fatal("显式查询 401 返回空列表：要找的那条被当成扫描噪音丢掉了")
	}
	if len(res.Events) != 1 {
		t.Errorf("返回 %d 条，want 1（只有 401 该匹配）", len(res.Events))
	}
	if got := res.Events[0].Status; got != 401 {
		t.Errorf("返回的事件状态码 = %d, want 401", got)
	}
	if res.Noise != 0 {
		t.Errorf("res.Noise = %d：显式查询时不该再有被隐藏的条目", res.Noise)
	}
}

// TestDefaultListingStillSeparatesNoise.
//
// The separation is right for browsing and must survive: a public hostname is
// scanned continuously, and a default listing that shows every probe is a
// listing nobody reads. Without this, "fixing" the case above would just be
// removing the feature.
func TestDefaultListingStillSeparatesNoise(t *testing.T) {
	dir := writeEvents(t,
		externalReject(404, "/favicon.ico"),
		externalReject(404, "/api/hello"),
		localReject(404, "/typo"), // the operator's own, always listed
	)

	res, err := Read(dir, Query{})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	if res.Noise != 2 {
		t.Errorf("res.Noise = %d, want 2：默认视图应把外部扫描计数而非列出", res.Noise)
	}
	if len(res.Events) != 1 {
		t.Fatalf("列出 %d 条，want 1（只有本机那条）", len(res.Events))
	}
	if res.Events[0].Src != SourceLocal {
		t.Errorf("列出的是 %q，本机被拒请求不该被当成噪音", res.Events[0].Src)
	}
}

// TestFailedOnlyIsNotAnExplicitSearch.
//
// -failed means "show me failures", which is browsing. A public hostname
// produces enough refused scans to bury the failures that matter, so the
// separation has to stay on for it -- the distinction being drawn is between
// looking for something and looking around.
func TestFailedOnlyIsNotAnExplicitSearch(t *testing.T) {
	q := Query{FailedOnly: true}
	if !q.SuppressesNoise() {
		t.Error("-failed 关掉了噪音分离：默认失败列表会被外网扫描淹没")
	}
}

func TestSuppressesNoiseDecisionTable(t *testing.T) {
	cases := []struct {
		name string
		q    Query
		want bool
	}{
		{"默认浏览", Query{}, true},
		{"显式状态码", Query{Status: 401}, false},
		{"显式要噪音", Query{IncludeNoise: true}, false},
		{"状态码 + 噪音标志", Query{Status: 401, IncludeNoise: true}, false},
		{"只看失败", Query{FailedOnly: true}, true},
		{"按路由", Query{Route: "claude"}, true},
		{"按耗时", Query{MinLatency: time.Second}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.q.SuppressesNoise(); got != tc.want {
				t.Errorf("SuppressesNoise() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestSummaryKeepsListedAndHiddenApart.
//
// The two counts describe opposite things and were being added into one field,
// which produced a footnote saying "另有 N 次…未计入上面" about rows printed
// directly above. Wrong whenever -noise was passed, and wrong for every
// explicit search once those stopped being suppressed.
func TestSummaryKeepsListedAndHiddenApart(t *testing.T) {
	listed := Summarise([]Event{
		externalReject(401, "/v1/messages"),
		localReject(404, "/typo"),
	})
	if listed.Noise != 1 {
		t.Errorf("Summary.Noise = %d, want 1（列表内的外部被拒请求）", listed.Noise)
	}
	if listed.NoiseHidden != 0 {
		t.Errorf("Summarise 不应自行设置 NoiseHidden，得到 %d", listed.NoiseHidden)
	}
	if listed.Rejects != 2 {
		t.Errorf("Summary.Rejects = %d, want 2", listed.Rejects)
	}
}

// TestServerErrorsAreNotNoise is the regression for the 07-29/30 outage
// review.
//
// Every non-2xx response is recorded as a reject, including the proxy's own
// 5xx answers to real proxied requests -- and Noise() classified every
// external reject as scan noise regardless of status. During a four-hour
// upstream outage, 93 tunnel-side 500/502/529 responses to the operator's own
// traffic were folded into the "scan noise" footnote, and `log -failed`
// showed nothing. A 5xx is this deployment failing to serve whoever asked;
// only client-error responses to uninvited traffic are noise.
func TestServerErrorsAreNotNoise(t *testing.T) {
	cases := []struct {
		name  string
		e     Event
		noise bool
	}{
		{"外部 404 探测", externalReject(404, "/api/hello"), true},
		{"外部 401 探测", externalReject(401, "/v1/messages"), true},
		{"外部请求撞上 500", externalReject(500, "/v1/messages"), false},
		{"外部请求撞上 502", externalReject(502, "/v1/messages"), false},
		{"外部请求撞上 529", externalReject(529, "/v1/messages"), false},
		{"本机 404 从来不是噪音", localReject(404, "/typo"), false},
		{"本机 500 也不是", localReject(500, "/v1/messages"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.e.Noise(); got != tc.noise {
				t.Errorf("Noise() = %v, want %v", got, tc.noise)
			}
		})
	}
}

// TestFailedBrowseListsServerErrorRejects walks the incident end to end: the
// default failure view must show tunnel-side 5xx rejects as rows, while the
// scanner's 404s stay a footnote count.
func TestFailedBrowseListsServerErrorRejects(t *testing.T) {
	dir := writeEvents(t,
		externalReject(502, "/v1/messages"), // the operator's own request, failed
		externalReject(529, "/v1/messages"), // upstream overload passed through
		externalReject(404, "/favicon.ico"), // genuine scanner noise
		externalReject(404, "/api/hello"),
	)

	res, err := Read(dir, Query{FailedOnly: true})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(res.Events) != 2 {
		t.Fatalf("列出 %d 条，want 2：来自隧道的 502/529 是真实失败，不该折进噪音页脚", len(res.Events))
	}
	for _, e := range res.Events {
		if e.Status < 500 {
			t.Errorf("列出了状态 %d：4xx 探测应该留在噪音计数里", e.Status)
		}
	}
	if res.Noise != 2 {
		t.Errorf("res.Noise = %d, want 2（只有两条 404 探测）", res.Noise)
	}

	s := Summarise(res.Events)
	if s.Rejects != 2 {
		t.Errorf("Summary.Rejects = %d, want 2", s.Rejects)
	}
	if s.Noise != 0 {
		t.Errorf("Summary.Noise = %d, want 0：5xx 拒绝不该被标成噪音", s.Noise)
	}
}

// TestNoiseFlagListsRatherThanCounts pins what -noise means: the traffic moves
// into the table, so nothing is left hidden to report.
func TestNoiseFlagListsRatherThanCounts(t *testing.T) {
	dir := writeEvents(t,
		externalReject(404, "/favicon.ico"),
		externalReject(404, "/api/hello"),
	)

	res, err := Read(dir, Query{IncludeNoise: true})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(res.Events) != 2 {
		t.Errorf("列出 %d 条，want 2", len(res.Events))
	}
	if res.Noise != 0 {
		t.Errorf("res.Noise = %d：已经列出的条目不该同时算作被隐藏", res.Noise)
	}
}
