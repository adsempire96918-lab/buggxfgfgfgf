// Package main — TLS fingerprint variation strategy.
//
// Why this matters: Reddit and Cloudflare score each connection's JA3
// (TLS ClientHello fingerprint). A fixed profile = fixed JA3 = easy to
// detect as automation. We rotate between known-working profiles to
// distribute detection risk across different JA3 signatures.
package main

import (
	"sync"

	"github.com/bogdanfinn/tls-client/profiles"
)

// profileTable lists the TLS-client profiles that will rotate through.
// Only profiles known to exist in tls-client v1.18 are used; adding
// non-existent ones breaks the build.
var profileTable = []struct {
	name    string
	profile profiles.ClientProfile
}{
	{"chrome_133", profiles.Chrome_133},
	{"chrome_144", profiles.Chrome_144},
}

// ProfileRotator cycles through TLS profiles per session/lane.
type ProfileRotator struct {
	mu        sync.Mutex
	index     int
	rotations int
	threshold int // reset every N rotations
}

// NewProfileRotator creates a rotator seeded by lane index + rotation count.
func NewProfileRotator(laneIdx int) *ProfileRotator {
	return &ProfileRotator{rotations: 0, threshold: 40} // change every ~40 requests
}

// Next returns the next profile to use for a new TLS connection.
func (pr *ProfileRotator) Next() profiles.ClientProfile {
	pr.mu.Lock()
	defer pr.mu.Unlock()

	if pr.rotations >= pr.threshold {
		pr.index = 0
		pr.rotations = 0
	}

	p := profileTable[pr.index%len(profileTable)]
	pr.rotations++
	pr.index++
	return p.profile
}
