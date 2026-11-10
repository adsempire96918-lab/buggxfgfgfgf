// Package main - runtime metrics.
//
// The single most useful number when the checker runs at 200-500 threads is
// the BLOCK RATE (how many requests Reddit refused) next to the throughput.
// Without it you are flying blind: a run can be "fast" and still be
// poisoning the exit IP.
//
// The collector is deliberately cheap: one atomic add per request, a fixed
// histogram for the latency percentiles and no per-request allocation on the
// hot path.
package main

import (
	"fmt"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// Metrics counts what happened during a run.
type Metrics struct {
	requests atomic.Int64
	ok       atomic.Int64 // HTTP 200 with a usable body
	notFound atomic.Int64 // 404 / missing account
	blocked  atomic.Int64 // 403 / 429 / block page
	challeng atomic.Int64 // jsc challenge pages
	failed   atomic.Int64 // transport errors / timeouts
	retries  atomic.Int64

	// connection accounting (T-08): a request that had to open a NEW TLS
	// connection is CPU work and a fresh-connection burst is a bot signal.
	// tls-client does not expose its transport counters, so the count comes
	// from net/http's own GotConn trace hook (Reused flag).
	newConns    atomic.Int64
	reusedConns atomic.Int64

	// queueDepth is how many workers are currently waiting for a rate-limit
	// token. It is the single best indicator that the run is RATE limited
	// rather than network limited: a deep queue with a low block rate means
	// the bucket is the bottleneck.
	queueDepth atomic.Int64
	queueWait  atomic.Int64 // total nanoseconds spent waiting in the queue

	// error reasons: a fixed-size table (no map, no allocation on the hot
	// path). The run report used to show every non-block failure as one
	// "other" bucket, which made the real cause invisible.
	errReasons [8]atomic.Int64

	// latency histogram: 24 buckets, each 50ms wide up to 1.2s, the last
	// bucket is "1.2s or more". A fixed histogram keeps the hot path
	// allocation-free and still gives p50/p95/p99.
	hist [24]atomic.Int64
	sum  atomic.Int64 // total latency in nanoseconds
	max  atomic.Int64 // slowest request in nanoseconds
}

// ErrorReasonNames are the fixed error reasons the collector counts. The
// order is stable so the report can render them in a fixed order.
var ErrorReasonNames = [8]string{
	"request_failed",
	"timeout",
	"blocked",
	"rate_limited",
	"login_required",
	"challenge",
	"browser_unavailable",
	"other",
}

// errorReasonIndex maps an error string to its slot (8 = other).
func errorReasonIndex(err string) int {
	switch err {
	case "request_failed":
		return 0
	case "timeout":
		return 1
	case "blocked":
		return 2
	case "rate_limited":
		return 3
	case "login_required":
		return 4
	case "challenge":
		return 5
	case "browser_unavailable", "browser_budget_exhausted", "browser_navigate_failed", "browser_read_failed":
		return 6
	}
	return 7
}

// NewMetrics creates an empty collector.
func NewMetrics() *Metrics { return &Metrics{} }

// RecordConn stores whether one request reused an existing connection.
// Safe on a nil collector (the one-off CLI commands have none).
func (m *Metrics) RecordConn(reused bool) {
	if m == nil {
		return
	}
	if reused {
		m.reusedConns.Add(1)
		return
	}
	m.newConns.Add(1)
}

// Record stores the outcome of ONE HTTP request.
func (m *Metrics) Record(status int, d time.Duration, challenge bool) {
	if m == nil {
		return
	}
	m.requests.Add(1)
	m.sum.Add(int64(d))
	for {
		old := m.max.Load()
		if int64(d) <= old || m.max.CompareAndSwap(old, int64(d)) {
			break
		}
	}
	b := int(d / (50 * time.Millisecond))
	if b >= len(m.hist) {
		b = len(m.hist) - 1
	}
	m.hist[b].Add(1)

	switch {
	case challenge:
		m.challeng.Add(1)
	case status == 200:
		m.ok.Add(1)
	case status == 404:
		m.notFound.Add(1)
	case status == 403 || status == 429:
		m.blocked.Add(1)
	}
}

// RecordError stores a transport-level failure (timeout, DNS, reset).
func (m *Metrics) RecordError() {
	if m != nil {
		m.failed.Add(1)
	}
}

// RecordReason counts ONE account-level failure by its error reason. This is
// what turns the report's single "other" bucket into a real breakdown.
func (m *Metrics) RecordReason(err string) {
	if m == nil || err == "" {
		return
	}
	i := errorReasonIndex(err)
	m.errReasons[i].Add(1)
}

// QueueEnter marks a worker that is about to wait for a rate-limit token.
func (m *Metrics) QueueEnter() {
	if m != nil {
		m.queueDepth.Add(1)
	}
}

// QueueLeave marks a worker that got its token (or gave up waiting).
func (m *Metrics) QueueLeave(waited time.Duration) {
	if m != nil {
		m.queueDepth.Add(-1)
		if waited > 0 {
			m.queueWait.Add(int64(waited))
		}
	}
}

// QueueDepth returns how many workers are waiting for a token right now.
func (m *Metrics) QueueDepth() int64 {
	if m == nil {
		return 0
	}
	return m.queueDepth.Load()
}

// ErrorReasons returns the count per error reason (index-aligned with
// ErrorReasonNames).
func (m *Metrics) ErrorReasons() [8]int64 {
	var out [8]int64
	if m == nil {
		return out
	}
	for i := range out {
		out[i] = m.errReasons[i].Load()
	}
	return out
}

// RecordRetry counts one retry attempt.
func (m *Metrics) RecordRetry() {
	if m != nil {
		m.retries.Add(1)
	}
}

// Snapshot is an immutable view of the counters.
type Snapshot struct {
	Requests  int64
	OK        int64
	NotFound  int64
	Blocked   int64
	Challenge int64
	Failed    int64
	Retries   int64
	Goroutine int
	HeapMB    float64
	AvgMS     float64
	MaxMS     float64
	P50MS     float64
	P95MS     float64
	P99MS     float64
	BlockPct  float64
	QueueNow  int64
	QueueAvg  float64
	Reasons   [8]int64

	// connection reuse: NewConns are TLS handshakes this run paid for.
	NewConns     int64
	ReusedConns  int64
	ConnReusePct float64
}

// Snapshot reads the counters and computes the derived numbers.
func (m *Metrics) Snapshot() Snapshot {
	if m == nil {
		return Snapshot{}
	}
	s := Snapshot{
		Requests:  m.requests.Load(),
		OK:        m.ok.Load(),
		NotFound:  m.notFound.Load(),
		Blocked:   m.blocked.Load(),
		Challenge: m.challeng.Load(),
		Failed:    m.failed.Load(),
		Retries:   m.retries.Load(),
		Goroutine: runtime.NumGoroutine(),

		NewConns:    m.newConns.Load(),
		ReusedConns: m.reusedConns.Load(),
	}
	if total := s.NewConns + s.ReusedConns; total > 0 {
		s.ConnReusePct = float64(s.ReusedConns) / float64(total) * 100
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	s.HeapMB = float64(ms.HeapAlloc) / (1024 * 1024)
	if s.Requests > 0 {
		s.AvgMS = float64(m.sum.Load()) / float64(s.Requests) / 1e6
		s.MaxMS = float64(m.max.Load()) / 1e6
		// the block rate is measured over the requests that produced a
		// verdict - a 404 is a clean answer, not a failure
		verdicts := s.OK + s.NotFound + s.Blocked
		if verdicts > 0 {
			s.BlockPct = float64(s.Blocked) / float64(verdicts) * 100
		}
	}
	s.P50MS, s.P95MS, s.P99MS = m.percentiles()
	s.QueueNow = m.queueDepth.Load()
	if s.Requests > 0 {
		s.QueueAvg = float64(m.queueWait.Load()) / float64(s.Requests) / 1e6
	}
	s.Reasons = m.ErrorReasons()
	return s
}

// percentiles computes p50/p95/p99 from the fixed histogram.
func (m *Metrics) percentiles() (p50, p95, p99 float64) {
	total := int64(0)
	for i := range m.hist {
		total += m.hist[i].Load()
	}
	if total == 0 {
		return 0, 0, 0
	}
	// the bucket i covers [i*50ms, (i+1)*50ms); use the bucket's upper edge
	// so the reported percentile is never optimistic
	at := func(q float64) float64 {
		want := int64(float64(total) * q)
		var seen int64
		for i := range m.hist {
			seen += m.hist[i].Load()
			if seen >= want {
				return float64(i+1) * 50
			}
		}
		return float64(len(m.hist)) * 50
	}
	return at(0.50), at(0.95), at(0.99)
}

// Report renders the metrics as a block of text for the run report.
func (s Snapshot) Report() string {
	var b strings.Builder
	fmt.Fprintf(&b,
		"  HTTP requests    : %d  (ok %d | notfound %d | blocked %d | challenge %d | failed %d | retries %d)\n",
		s.Requests, s.OK, s.NotFound, s.Blocked, s.Challenge, s.Failed, s.Retries)
	fmt.Fprintf(&b,
		"  Latency          : avg %.0fms | p50 %.0fms | p95 %.0fms | p99 %.0fms | max %.0fms\n",
		s.AvgMS, s.P50MS, s.P95MS, s.P99MS, s.MaxMS)
	fmt.Fprintf(&b, "  Block rate       : %.2f%% of the answered requests\n", s.BlockPct)
	fmt.Fprintf(&b,
		"  Rate limiter     : queue now %d | queue wait avg %.0fms per request\n",
		s.QueueNow, s.QueueAvg)
	fmt.Fprintf(&b, "  Connections      : %d new | %d reused | reuse %.1f%% (1 new per %.1f requests)\n",
		s.NewConns, s.ReusedConns, s.ConnReusePct, connsPerRequest(s.NewConns, s.NewConns+s.ReusedConns))
	fmt.Fprintf(&b, "  Runtime          : %d goroutines | %.1f MB heap\n", s.Goroutine, s.HeapMB)
	// the error breakdown - the "other" bucket used to hide the real cause
	any := false
	for i, n := range s.Reasons {
		if n > 0 {
			if !any {
				b.WriteString("  Error reasons    :\n")
				any = true
			}
			fmt.Fprintf(&b, "    %-22s: %d\n", ErrorReasonNames[i], n)
		}
	}
	return b.String()
}

// connsPerRequest renders the connection ratio as "one new connection per N
// requests" - the number that tells you whether keep-alive is working.
func connsPerRequest(newConns, total int64) float64 {
	if newConns <= 0 {
		return 0
	}
	return float64(total) / float64(newConns)
}

// SortDurations is a tiny helper used by the benchmark mode to report the
// latency percentiles of a sample.
func SortDurations(d []time.Duration) {
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
}
