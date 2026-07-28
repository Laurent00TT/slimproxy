package journal

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeDay(t *testing.T, dir, day string, lines ...string) {
	t.Helper()
	body := ""
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, day+".jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func today() string { return time.Now().Format("2006-01-02") }
func nowStamp() string {
	return time.Now().Add(-time.Minute).Format(time.RFC3339)
}

// TestNoiseIsCountedNotDiscarded.
//
// Was: noise was filtered during the read, so the summary line reporting how
// much had been excluded could never fire. That is not splitting the traffic
// from the signal, it is discarding half of it and saying nothing -- on a
// deployment already being scanned from the internet.
func TestNoiseIsCountedNotDiscarded(t *testing.T) {
	dir := t.TempDir()
	writeDay(t, dir, today(),
		`{"ts":"`+nowStamp()+`","kind":"req","route":"openai → claude","ok":true,"ms":900}`,
		`{"ts":"`+nowStamp()+`","kind":"rej","method":"HEAD","path":"/api/hello","status":404,"src":"tunnel","n":37}`,
	)

	res, err := Read(dir, Query{Since: time.Now().Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Events) != 1 {
		t.Errorf("默认应只列出 1 条请求，实际 %d", len(res.Events))
	}
	if res.Noise != 37 {
		t.Errorf("噪音计数 = %d，应为 37：被排除不等于被丢弃", res.Noise)
	}

	withNoise, err := Read(dir, Query{Since: time.Now().Add(-time.Hour), IncludeNoise: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(withNoise.Events) != 2 {
		t.Errorf("-noise 下应列出 2 条，实际 %d", len(withNoise.Events))
	}
}

// TestNoiseDoesNotConsumeTheLimit.
//
// A scanner produces far more rejects than the operator has requests. If noise
// took slots in the ring, `log -n 50` on a probed endpoint would return fifty
// scan lines and none of the requests it was run to find.
func TestNoiseDoesNotConsumeTheLimit(t *testing.T) {
	dir := t.TempDir()
	lines := []string{`{"ts":"` + nowStamp() + `","kind":"req","route":"openai → claude","ok":true,"ms":900}`}
	for i := 0; i < 200; i++ {
		lines = append(lines, `{"ts":"`+nowStamp()+`","kind":"rej","path":"/x","status":404,"src":"tunnel","n":1}`)
	}
	writeDay(t, dir, today(), lines...)

	res, err := Read(dir, Query{Since: time.Now().Add(-time.Hour), Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Events) != 1 {
		t.Errorf("列出 %d 条，应只有那 1 个请求——噪音不该挤占名额", len(res.Events))
	}
	if res.Noise != 200 {
		t.Errorf("噪音计数 = %d，应为 200", res.Noise)
	}
}

// TestStateEventsSurviveRequestFilters.
//
// A tunnel reconnect has no status code and no latency. Dropping it because it
// fails a request-shaped filter would remove exactly the line that explains the
// requests around it -- which is the whole reason these share one timeline.
func TestStateEventsSurviveRequestFilters(t *testing.T) {
	dir := t.TempDir()
	writeDay(t, dir, today(),
		`{"ts":"`+nowStamp()+`","kind":"tunnel","state":"up","detail":"2 连接"}`,
		`{"ts":"`+nowStamp()+`","kind":"req","route":"openai → claude","ok":false,"status":429,"ms":20000}`,
		`{"ts":"`+nowStamp()+`","kind":"req","route":"openai → claude","ok":true,"ms":800}`,
	)

	res, err := Read(dir, Query{
		Since: time.Now().Add(-time.Hour), FailedOnly: true, Status: 429,
	})
	if err != nil {
		t.Fatal(err)
	}

	var sawTunnel, sawReq bool
	for _, e := range res.Events {
		if e.Kind == KindTunnel {
			sawTunnel = true
		}
		if e.Kind == KindRequest {
			sawReq = true
		}
	}
	if !sawReq {
		t.Error("过滤后应保留匹配的失败请求")
	}
	if !sawTunnel {
		t.Error("状态事件被请求过滤器筛掉了：解释失败的那一行不见了")
	}
}

// TestCorruptLineDoesNotFailTheQuery.
//
// A half-written last line is normal after a crash -- which is precisely when
// someone reaches for this. Failing the whole query over it would make the
// journal useless in the situation it exists for.
func TestCorruptLineDoesNotFailTheQuery(t *testing.T) {
	dir := t.TempDir()
	writeDay(t, dir, today(),
		`{"ts":"`+nowStamp()+`","kind":"req","route":"a → b","ok":true,"ms":100}`,
		`{"ts":"`+nowStamp()+`","kind":"req","rou`, // truncated by a kill
	)

	res, err := Read(dir, Query{Since: time.Now().Add(-time.Hour)})
	if err != nil {
		t.Fatalf("截断的一行不应让查询失败: %v", err)
	}
	if len(res.Events) != 1 {
		t.Errorf("应读出 1 条完好的事件，实际 %d", len(res.Events))
	}
}

// TestSummaryDistinguishesFailureCauses: 401 and 429 need opposite responses,
// so a summary that only counted "failures" would hide the one thing worth
// knowing.
func TestSummaryDistinguishesFailureCauses(t *testing.T) {
	events := []Event{
		{Kind: KindRequest, OK: boolPtr(false), Status: 429, LatencyMs: 2000},
		{Kind: KindRequest, OK: boolPtr(false), Status: 401, LatencyMs: 100},
		{Kind: KindRequest, OK: boolPtr(true), LatencyMs: 800, Quota5h: f64Ptr(0.91)},
	}
	s := Summarise(events)
	if s.Requests != 3 || s.Failed != 2 {
		t.Errorf("计数不对: %+v", s)
	}
	if s.ByStatus[429] != 1 || s.ByStatus[401] != 1 {
		t.Errorf("失败未按状态码区分: %+v", s.ByStatus)
	}
	if s.MaxQuota != 0.91 {
		t.Errorf("配额峰值 = %v，应为 0.91", s.MaxQuota)
	}
}

// TestQuotaAbsentIsNotZero: a provider that reports nothing must not look like
// a freshly reset window.
func TestQuotaAbsentIsNotZero(t *testing.T) {
	s := Summarise([]Event{{Kind: KindRequest, OK: boolPtr(true)}})
	if s.MaxQuota >= 0 {
		t.Errorf("没有配额读数时应为负，实际 %v", s.MaxQuota)
	}
}
