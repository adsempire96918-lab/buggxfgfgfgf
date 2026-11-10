// Package main - session pool: every cookie session of a folder becomes a
// usable lane. Sessions unlock NSFW profiles and isolate account-level
// blocks; the IP stays the primary rate-limit budget.
package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Session health states.
const (
	PoolUnknown  = "unknown"
	PoolHealthy  = "healthy"
	PoolRejected = "rejected"
)

// Session is one logged-in Reddit account (a cookie set).
type Session struct {
	Name    string
	File    string
	Account string
	Cookies []Cookie
	State   string
	Detail  string
	Uses    int64
	LastUse time.Time
	Bucket  *TokenBucket // per-account rate budget
	// baseRate is the CONFIGURED per-session rate (--rate-per-session). The
	// bucket rate is baseRate scaled by the health score (T-07), so a clean
	// account may use up to 1.5x and a blocked one 0.5x of it.
	baseRate float64
	Score    int64     // health score: +1 clean, -1 blocked
	Quar     time.Time // quarantine until
	leased   bool      // currently handed to a lane (not offered to others)
	mu       sync.Mutex
}

// SessionPool hands out sessions to the lanes.
type SessionPool struct {
	mu       sync.Mutex
	sessions []*Session
	next     int
}

// NewSessionPool builds the pool and gives every session its own bucket.
func NewSessionPool(sessions []*Session, perSessionRate float64, burst int) *SessionPool {
	for _, s := range sessions {
		if s.Bucket == nil {
			s.Bucket = NewTokenBucket(perSessionRate, burst)
		}
		if s.baseRate <= 0 {
			s.baseRate = perSessionRate
			if s.baseRate <= 0 {
				s.baseRate = s.Bucket.Rate()
			}
		}
	}
	return &SessionPool{sessions: sessions}
}

// Len returns the pool size.
func (p *SessionPool) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.sessions)
}

// Next returns the next usable session (quarantined ones are skipped).
//
// With a large pool (hundreds of accounts) the UNLEASED sessions are handed
// out first, so every lane starts on its own account and one blocked session
// never starves the others. When every session is already leased the pool
// falls back to the plain round-robin, which recycles an existing session
// instead of failing.
func (p *SessionPool) Next() *Session {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := len(p.sessions)
	if n == 0 {
		return nil
	}

	// Pass 1: a free (never leased) session.
	for i := 0; i < n; i++ {
		s := p.sessions[p.next%n]
		p.next = (p.next + 1) % n
		if s == nil {
			continue
		}
		if time.Now().Before(s.Quar) {
			continue
		}
		s.mu.Lock()
		free := !s.leased
		if free {
			s.leased = true
		}
		s.mu.Unlock()
		if free {
			return s
		}
	}

	// Pass 2: every session is in use - recycle the next one (the lane that
	// holds it will release it when it rotates away).
	for i := 0; i < n; i++ {
		s := p.sessions[p.next%n]
		p.next = (p.next + 1) % n
		if s == nil {
			continue
		}
		if time.Now().Before(s.Quar) {
			continue
		}
		return s
	}
	return nil
}

// Release marks a session as no longer held by a lane, so the pool can hand
// it to another lane again.
func (s *Session) Release() {
	s.mu.Lock()
	s.leased = false
	s.mu.Unlock()
}

// Report records the outcome for a session (health score + quarantine).
//
// The score also drives the session's OWN rate budget (T-07): a clean account
// is allowed up to 1.5x its configured rate, a repeatedly blocked one drops to
// 0.5x. Only this session's bucket moves - the per-IP bucket in front of it
// still caps the total of every lane on the exit IP.
func (s *Session) Report(ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ok {
		s.Score++
		if s.Score > 10 {
			s.Score = 10
		}
		s.applyRateLocked()
		return
	}
	s.Score--
	if s.Score <= -3 {
		s.Score = -3
		s.Quar = time.Now().Add(10 * time.Minute)
	}
	s.applyRateLocked()
}

// Per-session adaptive rate band (T-07). The band is the contract: no score
// may push a session above 1.5x or below 0.5x of its configured rate.
const (
	sessionRateMinFactor = 0.5
	sessionRateMaxFactor = 1.5
)

// sessionRateFactor maps a health score to a rate multiplier.
//
//	score  0 -> 1.00x   (neutral: configured rate)
//	score 10 -> 1.50x   (10 clean results in a row)
//	score -3 -> 0.50x   (3 blocks in a row: the session is quarantined)
func sessionRateFactor(score int64) float64 {
	switch {
	case score >= 0:
		f := 1.0 + 0.5*float64(score)/10.0
		if f > sessionRateMaxFactor {
			return sessionRateMaxFactor
		}
		return f
	default:
		f := 1.0 + 0.5*float64(score)/3.0
		if f < sessionRateMinFactor {
			return sessionRateMinFactor
		}
		return f
	}
}

// applyRateLocked sets the bucket rate from baseRate and the score. The caller
// holds s.mu.
func (s *Session) applyRateLocked() {
	if s.Bucket == nil || s.baseRate <= 0 {
		return
	}
	s.Bucket.SetRate(s.baseRate * sessionRateFactor(s.Score))
}

// SetBaseRate sets the configured per-session rate and re-applies the score.
func (s *Session) SetBaseRate(perMin float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if perMin <= 0 {
		return
	}
	s.baseRate = perMin
	s.applyRateLocked()
}

// BaseRate returns the configured (unscaled) per-session rate.
func (s *Session) BaseRate() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.baseRate > 0 {
		return s.baseRate
	}
	if s.Bucket != nil {
		return s.Bucket.Rate()
	}
	return 0
}

// EffectiveRate returns the CURRENT per-session rate (base x health factor).
func (s *Session) EffectiveRate() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Bucket == nil {
		return 0
	}
	return s.Bucket.Rate()
}

// ScoreValue returns the health score.
func (s *Session) ScoreValue() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Score
}

// ClearPenalty lifts the quarantine and resets a negative score to zero.
// Used after a VPN IP rotation: the blocks were caused by the old exit IP,
// not by this account, so the session deserves a clean slate.
func (s *Session) ClearPenalty() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Score < 0 {
		s.Score = 0
	}
	s.Quar = time.Time{}
	s.applyRateLocked()
}

// ClearPenalty lifts the quarantine for every session and resets negative scores.
// Used after a VPN IP rotation: the blocks were caused by the old exit IP,
// not by this account, so the sessions deserve a clean slate.
func (p *SessionPool) ClearPenalty() {
	p.mu.Lock()
	sessions := make([]*Session, len(p.sessions))
	copy(sessions, p.sessions)
	p.mu.Unlock()
	for _, s := range sessions {
		if s != nil {
			// the single-session version also re-applies the health-scaled
			// rate, so a lifted penalty really restores the base rate (T-07)
			s.ClearPenalty()
		}
	}
}

// LoadSessionPool loads every cookie session file in a folder.
//
// A .txt file may hold ONE session (the usual "[karma] [user].txt" form) or
// MANY sessions (the Python sessions.txt form: one bare reddit_session value
// per line). A multi-line file is split into one session per line, so a
// single sessions.txt can feed many lanes.
func LoadSessionPool(dir string) ([]*Session, error) {
	files := FindSessionFiles(dir)
	if len(files) == 0 {
		return nil, fmt.Errorf("no session files found in %s", dir)
	}
	out := make([]*Session, 0, len(files))
	for _, f := range files {
		// A .txt file may hold ONE session (the usual "[karma] [user].txt"
		// Netscape form) or MANY sessions (the Python sessions.txt form: one
		// bare reddit_session value per line). LoadSessionValues tells the
		// two apart and returns one cookie set per session.
		var sets [][]Cookie
		if strings.HasSuffix(strings.ToLower(f), ".txt") {
			sets, _ = LoadSessionValues(f)
		}
		if len(sets) == 0 {
			cs, err := LoadCookies(f)
			if err != nil || len(cs) == 0 {
				continue
			}
			sets = [][]Cookie{cs}
		}

		meta := ParseSessionMeta(f)
		name := meta.Username
		if name == "" {
			name = usernameFromFileName(f)
		}
		if name == "" {
			name = filepath.Base(f)
		}

		for i, set := range sets {
			sname := name
			if len(sets) > 1 {
				sname = fmt.Sprintf("%s#%d", name, i+1)
			}
			out = append(out, &Session{
				Name:    sname,
				File:    f,
				Account: meta.Username,
				Cookies: withOver18(set),
				State:   PoolUnknown,
				Detail:  fmt.Sprintf("%d cookies", len(set)),
			})
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no usable cookie files in %s", dir)
	}
	return out, nil
}

// withOver18 adds the age-confirmation cookie if it is missing (belt and
// braces: a session already removes the age gate, but over18=1 makes sure
// Reddit never asks again).
func withOver18(cs []Cookie) []Cookie {
	for _, c := range cs {
		if c.Name == "over18" {
			return cs
		}
	}
	return append(append([]Cookie{}, cs...), Cookie{
		Name: "over18", Value: "1", Domain: defaultCookieDomain, Path: "/",
	})
}

// SetState updates the session health (thread safe).
func (s *Session) SetState(state, detail string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.State = state
	s.Detail = detail
}

// Touch records a successful use.
func (s *Session) Touch() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Uses++
	s.LastUse = time.Now()
}

// ProbeSession checks a session with ONE fast HTTP call (/api/v1/me).
// 200 -> the session is accepted (Reddit did not reject it).
// 401/403 -> rejected (expired / revoked).
func ProbeSession(ctx context.Context, proxy string, timeout time.Duration, s *Session) (state, detail string) {
	c, err := newClient(proxy, timeout, 0)
	if err != nil {
		return SessionUnknown, "client_error: " + err.Error()
	}
	c.ApplyCookies(s.Cookies)
	code, _, err := c.Get(ctx, "https://www.reddit.com/api/v1/me", map[string]string{
		"Accept": "application/json", "Referer": "https://www.reddit.com/",
	})
	if err != nil {
		return SessionUnknown, "request_failed: " + err.Error()
	}
	switch code {
	case 200:
		return PoolHealthy, "session accepted"
	case 401, 403:
		return PoolRejected, fmt.Sprintf("HTTP %d - session rejected", code)
	default:
		return PoolUnknown, fmt.Sprintf("HTTP %d", code)
	}
}
