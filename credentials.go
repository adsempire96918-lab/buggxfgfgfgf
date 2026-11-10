// Package main - input credential validation.
//
// This is the Go version of the Python script's input filter. It runs BEFORE
// any request is spent, so broken lines never cost a Reddit lookup:
//
//   - the username must match ^[a-zA-Z0-9_-]{3,20}$
//   - the username must be at least 6 characters
//   - "[not_saved]" markers are dropped
//   - numeric-only usernames are dropped
//   - unknown / none / admin are dropped
//   - the password must be at least 8 characters
package main

import (
	"fmt"
	"regexp"
	"strings"
)

// validUsernameRe mirrors the Python regex exactly.
var validUsernameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{3,20}$`)

// blacklistedTerms mirrors the Python BLACKLISTED_TERMS set.
var blacklistedTerms = map[string]bool{"unknown": true, "none": true, "admin": true}

// CredentialFilter is the result of validating the input file.
type CredentialFilter struct {
	Total           int // lines that contain ":"
	InvalidUsername int // failed the username regex
	ShortUsername   int // username shorter than 6 characters
	NotSaved        int // "[not_saved]" marker
	NumericUsername int // digits only
	Blacklisted     int // unknown / none / admin
	ShortPassword   int // password shorter than 8 characters
	Valid           int // lines that will be checked
	Lines           []string
}

// FilterCredentials validates the input lines the same way the Python script
// does and returns the lines that are worth checking.
func FilterCredentials(lines []string) *CredentialFilter {
	f := &CredentialFilter{}
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if !strings.Contains(line, ":") {
			continue // the Python script only counts lines with ":"
		}
		f.Total++
		username, password, _ := strings.Cut(line, ":")
		usernameLc := strings.ToLower(username)
		passwordLc := strings.ToLower(password)

		switch {
		case !validUsernameRe.MatchString(username):
			f.InvalidUsername++
			continue
		case len(username) < 6:
			f.ShortUsername++
			continue
		case usernameLc == "[not_saved]" || passwordLc == "[not_saved]":
			f.NotSaved++
			continue
		case isAllDigits(username):
			f.NumericUsername++
			continue
		case blacklistedTerms[usernameLc] || blacklistedTerms[passwordLc]:
			f.Blacklisted++
			continue
		case len(password) < 8:
			f.ShortPassword++
			continue
		}
		f.Valid++
		f.Lines = append(f.Lines, line)
	}
	return f
}

// isAllDigits reports whether s consists only of digits.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// Print shows the validation summary (the Python script's header block).
func (f *CredentialFilter) Print() {
	fmt.Printf("Total lines in input.txt: %d\n", f.Total)
	fmt.Printf("Rejected for invalid usernames: %d\n", f.InvalidUsername)
	fmt.Printf("Rejected for short usernames (<6 chars): %d\n", f.ShortUsername)
	fmt.Printf("Rejected for blacklisted values: %d\n", f.Blacklisted)
	fmt.Printf("Rejected for valid usernames but short passwords: %d\n", f.ShortPassword)
	fmt.Printf("Rejected for [NOT_SAVED] markers: %d\n", f.NotSaved)
	fmt.Printf("Rejected for numeric-only usernames: %d\n", f.NumericUsername)
	fmt.Printf("Proceeding to check %d accounts on Reddit...\n\n", f.Valid)
}
