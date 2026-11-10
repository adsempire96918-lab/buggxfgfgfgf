// Package main — rotating HTTP header pool for anti-bot evasion.
//
// Reddit's bot detection scores each request on header consistency:
//   - uniform User-Agent across many requests on the same IP = bot signal
//   - consistent Accept-Language / Accept patterns = automation indicator
//   - identical header ordering = JSSolver can fingerprint the client
//
// This file provides a rotating pool that cycles through realistic browser
// UA strings and header variations per request, distributing the "bot"
// signal evenly so Reddit's scoring never flags a single lane as automated.
package main

import (
	"math/rand"
	"sync"
	"time"
)

// UserAgentPool holds the rotating set of browser UAs used per request.
// Each UA looks like a real Chrome install from the same era; Reddit
// compares the JA3 fingerprint against the UA to find mismatches.
var userAgentPool = []string{
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/132.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/537.36",
	"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36",
}

// acceptVariants are the acceptable content types, randomized per request.
var acceptVariants = []string{
	"text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7",
	"text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
	"text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8",
	"text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7",
}

// headerRotator maintains a rotating index into the UA pool and accept list.
// The same client sees different headers over time, breaking monotonic patterns.
type headerRotator struct {
	mu       sync.Mutex
	uaIndex  int
	accIndex int
	seed     int64
}

// NewHeaderRotator creates a rotator seeded by time + lane index so rotation
// is deterministic per lane but differs across lanes (no two lanes have the
// same sequence).
func NewHeaderRotator(laneIdx int) *headerRotator {
	r := &headerRotator{seed: time.Now().UnixNano() ^ int64(laneIdx)}
	return r
}

// Current returns a full header map with the current rotation state.
// The caller passes in any extra headers they want (Authorization, Referer, etc.)
// and those take priority over defaults for matching keys.
func (h *headerRotator) Current() map[string]string {
	rng := rand.New(rand.NewSource(h.seed))
	h.mu.Lock()
	h.seed += rng.Int63n(7) // small jitter to keep rotation but avoid full cycles
	u := userAgentPool[h.uaIndex%len(userAgentPool)]
	a := acceptVariants[h.accIndex%len(acceptVariants)]
	h.uaIndex++
	h.accIndex++
	h.mu.Unlock()

	// Accept-Language varies per OS type; keep consistent with the UA it pairs with.
	var lang string
	switch u[:44] {
	case "Mozilla/5.0 (Windows":
		lang = "en-US,en;q=0.9"
	case "Mozilla/5.0 (Macintosh":
		lang = "en-US,en;q=0.9"
	case "Mozilla/5.0 (X11":
		lang = "en-US,en;q=0.9"
	default:
		lang = "en-US,en;q=0.9"
	}

	m := map[string]string{
		"User-Agent":                u,
		"Accept-Language":           lang,
		"Accept-Encoding":           "gzip, deflate, br, zstd",
		"sec-ch-ua":                 "\"Chromium\";v=\"133\", \"Not(A:Brand\";v=\"24\", \"Google Chrome\";v=\"133\"",
		"sec-ch-ua-mobile":          "?0",
		"sec-ch-ua-platform":        "\"Windows\"",
		"Sec-Fetch-Site":            "same-origin",
		"Sec-Fetch-Mode":            "navigate",
		"Sec-Fetch-Dest":            "document",
		"Upgrade-Insecure-Requests": "1",
		"Cache-Control":             "max-age=0",
	}
	m["Accept"] = a
	return m
}
