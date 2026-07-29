package journal

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Laurent00TT/slimproxy/i18n"
)

// Query selects events to read back.
//
// Zero value means "everything present", which is the useful default for a
// command whose flags are all optional.
type Query struct {
	Since time.Time
	Until time.Time

	// Status matches a specific upstream status. Zero means any.
	Status int
	// MinLatency keeps only requests at least this slow.
	MinLatency time.Duration
	// Route is a case-insensitive substring of "client → provider".
	Route string
	// Model is a case-insensitive substring of the model name.
	Model string
	// FailedOnly keeps only failed requests.
	FailedOnly bool

	// IncludeNoise keeps refused traffic that did not come from this machine.
	//
	// Off by default, and that default is the point: a public hostname is
	// scanned continuously, so including it makes every listing mostly a
	// record of the internet's curiosity. It stays available because "how much
	// is being thrown at me" is a real question -- just a different one.
	IncludeNoise bool

	// Limit caps the result, keeping the most recent. Zero means no cap.
	Limit int
}

// SuppressesNoise reports whether refused outside traffic will be counted
// instead of listed.
//
// The separation is a default-view policy, and it was being applied to
// deliberate searches as well. `log -status 401 -since 72h` returned an empty
// list and a footnote about scanner noise -- while the 401s the operator asked
// for sat in that count, because Noise() looks at Kind and Src and never at the
// status. Someone checking whether their key was being probed was told nothing
// was there.
//
// So an explicit status turns the policy off. Asking for one status is not
// browsing; it is looking for something specific, and a filter that quietly
// removes matches is worse than no filter.
//
// -failed deliberately does not count as explicit. It means "show me failures",
// which is browsing -- and a public hostname produces enough refused scans to
// bury the failures that matter, which is the case the separation exists for.
// Route, Model and MinLatency need no handling: reject events carry none of
// those fields, so Match has already dropped them before this is consulted.
func (q Query) SuppressesNoise() bool {
	if q.IncludeNoise {
		return false
	}
	return q.Status == 0
}

// Match reports whether an event satisfies the query.
//
// State events (proxy, tunnel, credential, doctor) pass every request-shaped
// filter. They are the context that makes a filtered listing interpretable --
// a burst of 502s means something different next to a tunnel reconnect -- and
// dropping them because they have no status code would remove exactly the line
// that explains the rest.
func (q Query) Match(e Event) bool {
	if !q.Since.IsZero() && e.At.Before(q.Since) {
		return false
	}
	if !q.Until.IsZero() && e.At.After(q.Until) {
		return false
	}
	// Noise is deliberately NOT filtered here.
	//
	// It is separated at render time instead, so it can still be counted.
	// Dropping it during the read hid it so thoroughly that the summary line
	// reporting how much had been excluded could never fire -- which is not
	// splitting the traffic, it is discarding half of it and saying nothing.
	if e.Kind != KindRequest && e.Kind != KindReject {
		return true
	}

	if q.FailedOnly && !e.Failed() && e.Kind != KindReject {
		return false
	}
	if q.Status != 0 && e.Status != q.Status {
		return false
	}
	if q.MinLatency > 0 && time.Duration(e.LatencyMs)*time.Millisecond < q.MinLatency {
		return false
	}
	if q.Route != "" && !strings.Contains(strings.ToLower(e.Route), strings.ToLower(q.Route)) {
		return false
	}
	if q.Model != "" && !strings.Contains(strings.ToLower(e.Model), strings.ToLower(q.Model)) {
		return false
	}
	return true
}

// Result is what a query found.
type Result struct {
	// Events are the matches worth listing, oldest first.
	Events []Event
	// Noise counts refused requests from outside this machine that were
	// excluded from Events. Reported rather than dropped: "how much is being
	// thrown at me" is a real question, just a different one from "how is my
	// proxy doing".
	Noise int
}

// Read returns matching events in chronological order.
//
// Streams each day file rather than loading it: a busy week is millions of
// lines, and a query command that needs them all in memory at once is one that
// stops working exactly when there is enough history to be worth querying.
// Limit keeps the most recent matches, in a ring, for the same reason.
func Read(dir string, q Query) (Result, error) {
	var res Result

	days, err := Days(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return res, fmt.Errorf(i18n.T(
				"还没有事件日志目录 %s；代理运行一段时间后才会有记录",
				"no event journal directory %s yet; records appear after the proxy has run for a while"), dir)
		}
		return res, err
	}
	if len(days) == 0 {
		return res, nil
	}

	var ring []Event
	push := func(e Event) {
		if e.Noise() && q.SuppressesNoise() {
			// Counted, not listed. And it does not consume a slot in Limit:
			// a scanner would otherwise push out every request the operator
			// actually asked to see.
			n := e.Count
			if n == 0 {
				n = 1
			}
			res.Noise += n
			return
		}
		if q.Limit <= 0 || len(ring) < q.Limit {
			ring = append(ring, e)
			return
		}
		copy(ring, ring[1:])
		ring[len(ring)-1] = e
	}

	for _, day := range days {
		if skipDay(day, q) {
			continue
		}
		if err := scanDay(filepath.Join(dir, day+".jsonl"), q, push); err != nil {
			return res, err
		}
	}
	res.Events = ring
	return res, nil
}

// skipDay avoids opening files that cannot contain matches.
func skipDay(day string, q Query) bool {
	d, err := time.ParseInLocation("2006-01-02", day, time.Local)
	if err != nil {
		return false
	}
	end := d.AddDate(0, 0, 1)
	if !q.Since.IsZero() && !end.After(q.Since) {
		return true
	}
	if !q.Until.IsZero() && d.After(q.Until) {
		return true
	}
	return false
}

func scanDay(path string, q Query, push func(Event)) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Rotated away between listing and reading. Not an error worth
			// failing a query over.
			return nil
		}
		return err
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e Event
		if json.Unmarshal(line, &e) != nil {
			// A truncated last line is normal after a crash. Skipping it
			// silently is right; failing the whole query over it would make
			// the journal useless in exactly the situation it exists for.
			continue
		}
		if q.Match(e) {
			push(e)
		}
	}
	return sc.Err()
}

// Summary aggregates a result set.
type Summary struct {
	Requests int
	Failed   int
	Rejects  int
	// Noise counts refused outside traffic among the events that were listed.
	Noise int
	// NoiseHidden counts refused outside traffic withheld from the listing,
	// set by the caller from Result.Noise.
	//
	// Separate from Noise, and the two must never be added: one describes rows
	// the reader can see, the other rows they cannot. Summing them is what
	// produced a footnote reading "另有 N 次…未计入上面的失败数" about requests
	// sitting in the table directly above it -- wrong whenever -noise was
	// passed, and wrong for every explicit -status search once those stopped
	// being suppressed.
	NoiseHidden int

	// ByStatus counts failures per upstream status, which is what separates
	// "replace the credential" from "wait".
	ByStatus map[int]int

	// SlowestMs and P50Ms describe the latency distribution of the matches.
	SlowestMs int64
	P50Ms     int64

	// MaxQuota is the highest five-hour window utilisation seen, or -1.
	MaxQuota float64

	// CacheRead and CacheCreation total the prompt-cache activity.
	CacheRead     int64
	CacheCreation int64
	// CacheWanted counts inbound requests that carried cache_control markers,
	// and CacheMissed how many requests followed one without reading anything
	// from cache.
	//
	// The pair is the alarm: markers going in with nothing coming back means
	// every turn of every conversation is paying full price, which is invisible
	// in the replies and shows up only as the quota draining several times
	// faster than it should.
	CacheWanted int
	CacheMissed int
}

// Summarise reduces events to the numbers worth printing under a listing.
func Summarise(events []Event) Summary {
	s := Summary{ByStatus: map[int]int{}, MaxQuota: -1}
	var lat []int64
	for _, e := range events {
		switch e.Kind {
		case KindRequest:
			s.Requests++
			if e.Failed() {
				s.Failed++
				s.ByStatus[e.Status]++
			}
			if e.LatencyMs > 0 {
				lat = append(lat, e.LatencyMs)
			}
			if e.Quota5h != nil && *e.Quota5h > s.MaxQuota {
				s.MaxQuota = *e.Quota5h
			}
			s.CacheRead += e.CacheRead
			s.CacheCreation += e.CacheCreation
			if e.CacheRead == 0 && e.CacheCreation == 0 {
				s.CacheMissed++
			}
		case KindFidelity:
			if e.CacheControls > 0 {
				s.CacheWanted++
			}
		case KindReject:
			n := e.Count
			if n == 0 {
				n = 1
			}
			s.Rejects += n
			if e.Noise() {
				s.Noise += n
			}
		}
	}
	if len(lat) > 0 {
		sortInt64(lat)
		s.SlowestMs = lat[len(lat)-1]
		s.P50Ms = lat[len(lat)/2]
	}
	return s
}

func sortInt64(v []int64) {
	// Insertion sort: these slices are the size of one query's matches, and
	// pulling in sort.Slice for it would allocate a closure per call.
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}
