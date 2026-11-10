// Package main - live status panel tests.
package main

import (
	"strings"
	"testing"
	"time"
)

// The panel must show the same fields as the Python script's rich table.
func TestLivePanelFields(t *testing.T) {
	l := NewLiveStatus(3972)
	l.checked, l.valid, l.invalid, l.failed = 1184, 683, 261, 0
	p := l.panel()
	for _, want := range []string{
		"Live Status", "Total To Check", "3972", "Checked", "1184",
		"Valid", "683", "Invalid (Banned/Suspended)", "261",
		"Failed (Connection/Timeout)", "Checks per minute",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("panel is missing %q:\n%s", want, p)
		}
	}
	// the panel may use the Unicode or ASCII box set depending on the
	// console code page (Windows legacy cp) - only the OUTLINE matters.
	// NOTE: the box glyphs are multi-byte UTF-8, so they must be compared as
	// whole strings - a byte slice like box[0:1] would cut a rune in half.
	tl, br := "+", "+"
	if consoleUTF8() {
		tl, br = "╭", "╯"
	}
	if !strings.HasPrefix(p, tl) || !strings.HasSuffix(p, br) {
		t.Errorf("panel is not bordered:\n%s", p)
	}
}

// The panel must be a fixed number of lines so the redraw can move the
// cursor up by exactly that many lines.
func TestLivePanelLineCount(t *testing.T) {
	l := NewLiveStatus(10)
	p := l.panel()
	lines := strings.Count(p, "\n") + 1
	if lines != 12 { // title + header + separator + 8 rows + bottom border
		t.Fatalf("panel has %d lines, want 12:\n%s", lines, p)
	}
}

// The checks-per-minute rate must be computed from the elapsed time.
func TestLiveCPM(t *testing.T) {
	l := NewLiveStatus(100)
	l.started = time.Now().Add(-30 * time.Second)
	l.checked = 60
	if cpm := l.cpm(); cpm < 100 || cpm > 140 {
		t.Fatalf("cpm = %d, want ~120 (60 checks in 30s)", cpm)
	}
}

// The input filter must reject exactly what the Python script rejects.
// NOTE: the Python script checks the username REGEX first, so a username
// with brackets or a space (and a 2-char username) is counted as
// "invalid username", not under the later counters. The [not_saved] counter
// is therefore only reachable through the PASSWORD side.
func TestCredentialFilter(t *testing.T) {
	lines := []string{
		"gooduser:goodpassword",    // valid
		"ab:longenoughpassword",    // 2 chars -> invalid username (regex first)
		"validuser:short",          // password too short
		"12345678:longenoughpass",  // numeric username
		"gooduser2:[not_saved]",    // not_saved marker (password side)
		"unknown:longenoughpass",   // blacklisted
		"has space:longenoughpass", // invalid regex
		"no_colon_here",            // no colon -> not counted at all
	}
	f := FilterCredentials(lines)
	if f.Total != 7 {
		t.Errorf("Total = %d, want 7 (the no-colon line is not counted)", f.Total)
	}
	if f.Valid != 1 || len(f.Lines) != 1 || f.Lines[0] != "gooduser:goodpassword" {
		t.Errorf("valid = %d lines=%v, want 1 gooduser", f.Valid, f.Lines)
	}
	if f.InvalidUsername != 2 { // "ab" (too short for the regex) and "has space"
		t.Errorf("InvalidUsername = %d, want 2", f.InvalidUsername)
	}
	if f.ShortPassword != 1 {
		t.Errorf("ShortPassword = %d, want 1", f.ShortPassword)
	}
	if f.NumericUsername != 1 {
		t.Errorf("NumericUsername = %d, want 1", f.NumericUsername)
	}
	if f.NotSaved != 1 {
		t.Errorf("NotSaved = %d, want 1", f.NotSaved)
	}
	if f.Blacklisted != 1 {
		t.Errorf("Blacklisted = %d, want 1", f.Blacklisted)
	}
}

// A 20-character username is the maximum the Python regex allows.
func TestCredentialFilterUsernameLength(t *testing.T) {
	f := FilterCredentials([]string{"abcdefghijklmnopqrst:longenoughpass"})
	if f.Valid != 1 {
		t.Errorf("20-char username rejected (valid=%d)", f.Valid)
	}
	f = FilterCredentials([]string{"abcdefghijklmnopqrstu:longenoughpass"})
	if f.InvalidUsername != 1 {
		t.Errorf("21-char username accepted (invalid=%d)", f.InvalidUsername)
	}
}
