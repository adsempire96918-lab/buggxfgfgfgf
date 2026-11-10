// T-08 — requests vs NEW TLS connections, and the reuse ratio in the metrics
// block. Without it you cannot tell a healthy keep-alive run from one that
// burns a handshake per request (CPU + a fresh-connection burst is a bot
// signal for Reddit).
package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tls_client "github.com/bogdanfinn/tls-client"
)

func TestKeepAliveMetric(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	m := NewMetrics()
	hc, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(),
		tls_client.WithInsecureSkipVerify(), // the test server is self-signed
		tls_client.WithForceHttp1(),         // the checker forces h1 as well
		tls_client.WithTimeoutMilliseconds(10000),
	)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	c := &Client{HTTP: hc, Timeout: 10 * time.Second, Metrics: m}

	const n = 4
	for i := 0; i < n; i++ {
		code, body, _, err := c.doRequest(context.Background(), srv.URL, nil)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		if code != 200 || body != "ok" {
			t.Fatalf("request %d: code=%d body=%q", i, code, body)
		}
	}
	if got := hits.Load(); got != n {
		t.Fatalf("server saw %d requests, want %d", got, n)
	}

	snap := m.Snapshot()
	if snap.Requests != n {
		t.Fatalf("metrics requests = %d, want %d", snap.Requests, n)
	}
	if total := snap.NewConns + snap.ReusedConns; total != n {
		t.Fatalf("connection accounting covered %d of %d requests (new %d / reused %d)",
			total, n, snap.NewConns, snap.ReusedConns)
	}
	if snap.NewConns != 1 {
		t.Fatalf("%d new TLS connections for %d sequential requests - keep-alive is not working",
			snap.NewConns, n)
	}
	if snap.ReusedConns != n-1 {
		t.Fatalf("reused = %d, want %d", snap.ReusedConns, n-1)
	}
	if want := 75.0; snap.ConnReusePct < want-0.01 || snap.ConnReusePct > want+0.01 {
		t.Fatalf("reuse = %.1f%%, want %.1f%%", snap.ConnReusePct, want)
	}

	report := snap.Report()
	if !strings.Contains(report, "Connections") || !strings.Contains(report, "reuse 75.0%") {
		t.Fatalf("the METRICS block does not show the connection ratio:\n%s", report)
	}
	if !strings.Contains(report, "1 new per 4.0 requests") {
		t.Fatalf("the METRICS block does not show the connection ratio:\n%s", report)
	}
}

// A nil collector must stay nil-safe on the hot path (the fetch/probe
// subcommands have no metrics).
func TestKeepAliveMetricNilSafe(t *testing.T) {
	var m *Metrics
	m.RecordConn(false) // must not panic
	if snap := m.Snapshot(); snap.NewConns != 0 {
		t.Fatalf("nil snapshot = %+v", snap)
	}
	if got := connsPerRequest(0, 10); got != 0 {
		t.Fatalf("connsPerRequest(0,10) = %v, want 0", got)
	}
	if got := connsPerRequest(2, 10); got != 5 {
		t.Fatalf("connsPerRequest(2,10) = %v, want 5", got)
	}
}
