// Package main - the profiler.
//
// At 200-500 threads the question is always "where does the time go?". The
// answer needs a CPU profile, not a guess: TLS handshakes, mutex contention
// on the token bucket, or the workers sleeping in the rate limiter look very
// different in a profile.
//
// Start it with --pprof :6060 and open the browser while the run is going:
//
//	http://localhost:6060/debug/pprof/         <- the index
//	http://localhost:6060/debug/pprof/profile?seconds=30   <- 30s CPU profile
//	http://localhost:6060/debug/pprof/heap                <- heap
//	http://localhost:6060/debug/pprof/goroutine?debug=2    <- every goroutine
//
// The server binds to LOOPBACK ONLY - a profiler that is reachable from the
// internet would leak the whole process state.
package main

import (
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"os"
	"sync"
	"time"
)

var (
	profOnce sync.Once
	profAddr string
)

// StartProfiler starts the pprof HTTP server on addr (e.g. ":6060").
// It is a no-op when addr is empty. The server runs in the background and
// never blocks the checker.
func StartProfiler(addr string) {
	if addr == "" {
		return
	}
	profOnce.Do(func() {
		profAddr = addr
		mux := http.NewServeMux()
		mux.HandleFunc("/debug/pprof/", func(w http.ResponseWriter, r *http.Request) {
			// the default pprof index is registered on DefaultServeMux;
			// forward to it so the links work
			http.DefaultServeMux.ServeHTTP(w, r)
		})
		srv := &http.Server{
			Addr:              addr,
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
		}
		go func() {
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				fmt.Fprintf(os.Stderr, "pprof: %v\n", err)
			}
		}()
		fmt.Printf("Profiler: http://localhost%s/debug/pprof/ (CPU: ?seconds=30)\n", addr)
	})
}

// ProfilerAddr returns the address the profiler listens on ("" = off).
func ProfilerAddr() string { return profAddr }
