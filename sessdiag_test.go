package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

// TestSessionFilesParse checks that the Netscape-style session files in
// sassion/ parse correctly and shows the JWT expiry of each session.
func TestSessionFilesParse(t *testing.T) {
	files := FindSessionFiles("sassion")
	if len(files) == 0 {
		t.Skip("no session files")
	}
	for _, f := range files {
		cs, err := LoadCookies(f)
		if err != nil {
			t.Logf("%s: parse error %v", filepath.Base(f), err)
			continue
		}
		names := []string{}
		exp := int64(0)
		for _, c := range cs {
			names = append(names, c.Name)
			if c.Name == "token_v2" || c.Name == "reddit_session" {
				if e := jwtExp(c.Value); e > exp {
					exp = e
				}
			}
		}
		state := "expired"
		if exp > time.Now().Unix() {
			state = "alive"
		}
		t.Logf("%-58s cookies=%d %v exp=%s (%s)", filepath.Base(f), len(cs), names,
			time.Unix(exp, 0).UTC().Format("2006-01-02"), state)
	}
}

// jwtExp reads the "exp" claim from a JWT payload (no signature check).
func jwtExp(token string) int64 {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return 0
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0
	}
	var claims struct {
		Exp float64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return 0
	}
	return int64(claims.Exp)
}

// TestSessionBrowserDiag injects one real session into Chrome and prints
// what Reddit actually returns (diagnostic for the injection path).
func TestSessionBrowserDiag(t *testing.T) {
	if os.Getenv("DIAG") == "" {
		t.Skip("set DIAG=1")
	}
	files := FindSessionFiles("sassion")
	var target string
	for _, f := range files {
		if strings.Contains(f, "Admirable") {
			target = f
			break
		}
	}
	if target == "" && len(files) > 0 {
		target = files[0]
	}
	cs, err := LoadCookies(target)
	if err != nil || len(cs) == 0 {
		t.Fatalf("load %s: %v", target, err)
	}
	t.Logf("target=%s cookies=%d", filepath.Base(target), len(cs))

	l := launcher.New().Headless(true).UserDataDir("sassion\\_diag_profile").
		Set("user-agent", uniqueChromeUA()).NoSandbox(true)
	if bin := findChrome(); bin != "" {
		l = l.Bin(bin)
	}
	url, err := l.Launch()
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	browser := rod.New().ControlURL(url)
	if err := browser.Connect(); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer browser.Close()

	params := []*proto.NetworkCookieParam{}
	for _, c := range cs {
		d := c.Domain
		if d == "" {
			d = defaultCookieDomain
		}
		p := c.Path
		if p == "" {
			p = "/"
		}
		params = append(params, &proto.NetworkCookieParam{
			Name: c.Name, Value: c.Value, Domain: d, Path: p, Secure: true,
		})
	}
	if err := browser.SetCookies(params); err != nil {
		t.Fatalf("setcookies: %v", err)
	}
	got, _ := browser.GetCookies()
	names := []string{}
	for _, c := range got {
		names = append(names, c.Name)
	}
	t.Logf("browser cookies after inject: %v", names)

	page, _ := browser.Page(proto.TargetCreateTarget{URL: "about:blank"})
	page.Timeout(30 * time.Second).MustNavigate("https://www.reddit.com/api/v1/me")
	body, _ := page.HTML()
	if len(body) > 400 {
		t.Logf("me body head: %s", strings.ReplaceAll(body[:400], "\n", " "))
		body = body[max(0, len(body)-400):]
	}
	t.Logf("me body tail: %s", strings.ReplaceAll(body, "\n", " "))
	t.Logf("contains data key: %v", strings.Contains(body, `"data"`))
}

func TestSessionFolderExists(t *testing.T) {
	if _, err := os.Stat("sassion"); err != nil {
		t.Skip("sassion folder not present")
	}
}
