// Package main — session tests (no network): cookie parse/save round-trip.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseCookieStringBasic(t *testing.T) {
	cs := ParseCookieString("reddit_session=abc; token_v2=def")
	if len(cs) != 2 {
		t.Fatalf("len = %d, want 2", len(cs))
	}
	if cs[0].Name != "reddit_session" || cs[0].Value != "abc" {
		t.Fatalf("cookie0 = %+v", cs[0])
	}
	if cs[1].Name != "token_v2" || cs[1].Value != "def" {
		t.Fatalf("cookie1 = %+v", cs[1])
	}
	if cs[0].Domain != ".reddit.com" || cs[0].Path != "/" {
		t.Fatalf("defaults = %+v", cs[0])
	}
}

func TestParseCookieStringSkipsReserved(t *testing.T) {
	cs := ParseCookieString("a=1; path=/; Domain=.x; Expires=Thu, 01-Jan-2017; Max-Age=1; SameSite=Lax; Secure; HttpOnly; b=2")
	if len(cs) != 2 || cs[0].Name != "a" || cs[1].Name != "b" {
		t.Fatalf("got %+v", cs)
	}
}

func TestLoadCookiesFormats(t *testing.T) {
	dir := t.TempDir()

	// raw header
	p1 := filepath.Join(dir, "raw.txt")
	os.WriteFile(p1, []byte("reddit_session=abc; token_v2=def\n"), 0644)
	cs, err := LoadCookies(p1)
	if err != nil || len(cs) != 2 || cs[0].Name != "reddit_session" {
		t.Fatalf("raw: cs=%+v err=%v", cs, err)
	}

	// JSON object
	p2 := filepath.Join(dir, "obj.json")
	os.WriteFile(p2, []byte(`{"reddit_session":"abc","token_v2":"def"}`), 0644)
	cs, err = LoadCookies(p2)
	if err != nil || len(cs) != 2 {
		t.Fatalf("obj: cs=%+v err=%v", cs, err)
	}

	// JSON array (Playwright style)
	p3 := filepath.Join(dir, "arr.json")
	os.WriteFile(p3, []byte(`[{"name":"reddit_session","value":"abc","domain":".reddit.com","path":"/"},{"name":"x","value":"y"}]`), 0644)
	cs, err = LoadCookies(p3)
	if err != nil || len(cs) != 2 || cs[0].Name != "reddit_session" || cs[1].Domain != ".reddit.com" {
		t.Fatalf("arr: cs=%+v err=%v", cs, err)
	}

	// Netscape cookies.txt
	p4 := filepath.Join(dir, "netscape.txt")
	ns := "# comment\n.reddit.com\tTRUE\t/\tTRUE\t0\treddit_session\tabc\n"
	os.WriteFile(p4, []byte(ns), 0644)
	cs, err = LoadCookies(p4)
	if err != nil || len(cs) != 1 || cs[0].Name != "reddit_session" || cs[0].Value != "abc" {
		t.Fatalf("netscape: cs=%+v err=%v", cs, err)
	}

	// missing file → empty, no error
	cs, err = LoadCookies(filepath.Join(dir, "nope.txt"))
	if err != nil || len(cs) != 0 {
		t.Fatalf("missing: cs=%+v err=%v", cs, err)
	}
}

func TestCookieStringFormat(t *testing.T) {
	got := CookieString([]Cookie{{Name: "a", Value: "1"}, {Name: "b", Value: "2"}})
	if got != "a=1; b=2" {
		t.Fatalf("CookieString = %q", got)
	}
}

func TestSaveAndLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	in := []Cookie{{Name: "reddit_session", Value: "abc", Domain: ".reddit.com", Path: "/"}}
	if err := SaveCookieFiles(dir, in); err != nil {
		t.Fatalf("save: %v", err)
	}
	out, err := LoadCookies(filepath.Join(dir, "cookies.json"))
	if err != nil || len(out) != 1 || out[0] != in[0] {
		t.Fatalf("roundtrip: out=%+v err=%v", out, err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "cookie_string.txt"))
	if strings.TrimRight(string(raw), "\r\n") != "reddit_session=abc" {
		t.Fatalf("cookie_string.txt = %q", string(raw))
	}
}

func TestWriteStatusFormat(t *testing.T) {
	dir := t.TempDir()
	if err := WriteStatus(dir, "VALID", "testuser", "session valid", 7); err != nil {
		t.Fatalf("write: %v", err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "login_status.txt"))
	s := string(raw)
	for _, want := range []string{"STATUS=VALID", "ACCOUNT=testuser", "DETAIL=session valid", "COOKIES=7", "TIME="} {
		found := false
		for _, line := range splitLines(s) {
			if line == want || (want == "TIME=" && len(line) > len(want) && line[:len(want)] == want) {
				found = true
			}
		}
		if !found {
			t.Fatalf("login_status.txt missing %q in:\n%s", want, s)
		}
	}
}

func TestWriteStatusUnknownAccount(t *testing.T) {
	dir := t.TempDir()
	WriteStatus(dir, "BANNED", "", "account is suspended", 0)
	raw, _ := os.ReadFile(filepath.Join(dir, "login_status.txt"))
	if got := splitLines(string(raw))[1]; got != "ACCOUNT=unknown" {
		t.Fatalf("line2 = %q, want ACCOUNT=unknown", got)
	}
}

func TestReadDoneUsersDir(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "0to500.txt"), []byte("alphauser:pw1\n"), 0644)
	os.WriteFile(filepath.Join(dir, "not_found.txt"), []byte("betauser | NOT_FOUND\n"), 0644)
	os.WriteFile(filepath.Join(dir, "banned.txt"), []byte("gamabanned | SUSPENDED\n"), 0644)
	done := ReadDoneUsers(dir)
	t.Logf("done=%v resumeFiles=%v", done, ResumeFiles)
	for _, want := range []string{"alphauser", "betauser", "gamabanned"} {
		if !done[want] {
			t.Fatalf("missing %q in done set", want)
		}
	}
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, strings.TrimRight(s[start:i], "\r"))
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, strings.TrimRight(s[start:], "\r"))
	}
	return out
}
