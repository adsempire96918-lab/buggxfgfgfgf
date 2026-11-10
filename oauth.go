// Package main - OAuth app-only token provider (parity with
// check_users.TokenProvider + reddit_fetch.get_oauth_token).
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// TokenProvider caches an app-only OAuth token and refreshes it when it
// expires or when Reddit rejects it.
type TokenProvider struct {
	clientID     string
	clientSecret string
	timeout      time.Duration

	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

// NewTokenProvider builds a provider ("" ids = disabled).
func NewTokenProvider(clientID, clientSecret string, timeout time.Duration) *TokenProvider {
	return &TokenProvider{clientID: clientID, clientSecret: clientSecret, timeout: timeout}
}

// Enabled reports whether credentials were supplied.
func (p *TokenProvider) Enabled() bool {
	return p != nil && p.clientID != "" && p.clientSecret != ""
}

// Get returns a valid token, fetching a new one when needed.
func (p *TokenProvider) Get() (string, error) {
	if !p.Enabled() {
		return "", fmt.Errorf("oauth credentials missing")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.token != "" && time.Now().Before(p.expiresAt) {
		return p.token, nil
	}
	return p.fetchLocked()
}

// Invalidate drops the cached token (called after a 401).
func (p *TokenProvider) Invalidate() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.token = ""
	p.expiresAt = time.Time{}
}

// fetchLocked requests a new app-only token (caller holds the lock).
func (p *TokenProvider) fetchLocked() (string, error) {
	form := "grant_type=client_credentials"
	req, err := http.NewRequest(http.MethodPost,
		"https://www.reddit.com/api/v1/access_token",
		strings.NewReader(form))
	if err != nil {
		return "", err
	}
	cred := base64.StdEncoding.EncodeToString([]byte(p.clientID + ":" + p.clientSecret))
	req.Header.Set("Authorization", "Basic "+cred)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", defaultUA)

	client := &http.Client{Timeout: p.timeout}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var payload struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
		Error       string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", fmt.Errorf("oauth_token_parse: %w", err)
	}
	if resp.StatusCode != 200 || payload.AccessToken == "" {
		return "", fmt.Errorf("oauth_token_http_%d: %s", resp.StatusCode, payload.Error)
	}
	p.token = payload.AccessToken
	life := time.Duration(payload.ExpiresIn) * time.Second
	if life < 60*time.Second {
		life = 60 * time.Second
	}
	p.expiresAt = time.Now().Add(life - 2*time.Minute)
	if p.expiresAt.Before(time.Now()) {
		p.expiresAt = time.Now().Add(60 * time.Second)
	}
	return p.token, nil
}
