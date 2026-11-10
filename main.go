// userkarmacheck-go — Reddit username karma checker (native Go rewrite).
// Subcommands:
//
//	fetch <username>  one user (JSON first, HTML fallback)
//	probe             verify a cookie file against /api/v1/me
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// planThreads validates the requested concurrency so a wrong number can
// never waste resources or hammer Reddit.
//
// Rules:
//   - threads are capped at maxSafeThreads (a hard ceiling)
//   - threads never exceed the number of lanes (a thread without a lane
//     would just wait)
//   - threads never exceed 4x the session count (more workers than
//     accounts to work on is pointless)
//   - a value of 0 means "auto": pick the lane count
//
// The ceiling was raised from 256 to 500 after the measured load test: the
// network path to Reddit needs ~500 concurrent connections to saturate the
// per-IP budget (the RTT is ~1.2 s, so throughput = concurrency / latency).
// The AIMD controller and the shared per-IP bucket keep the rate safe - the
// thread count alone never raises the request rate.
func planThreads(threads, lanes, sessions int) int {
	const maxSafeThreads = 512
	if threads <= 0 {
		threads = lanes
	}
	if lanes > 0 && threads > lanes {
		threads = lanes
	}
	if sessions > 0 && threads > sessions*4 {
		threads = sessions * 4
	}
	if threads > maxSafeThreads {
		threads = maxSafeThreads
	}
	if threads < 1 {
		threads = 1
	}
	return threads
}

// printPlan shows what the run WOULD do without touching a single file.
// It is the safe way to try a new thread count.
func printPlan(input, output string, threads, lanes, sessions int, accounts, dupes, skipped int, multi bool) {
	fmt.Println()
	fmt.Println("============================================================")
	fmt.Println("  RUN PLAN (dry run - nothing was written)")
	fmt.Println("============================================================")
	fmt.Printf("  %-18s %s\n", "Input file:", input)
	fmt.Printf("  %-18s %s\n", "Output dir:", output)
	fmt.Printf("  %-18s %d\n", "Accounts:", accounts)
	fmt.Printf("  %-18s %d\n", "Duplicate rows:", dupes)
	fmt.Printf("  %-18s %d\n", "Already checked:", skipped)
	fmt.Printf("  %-18s %d\n", "Sessions:", sessions)
	fmt.Printf("  %-18s %d\n", "Lanes:", lanes)
	fmt.Printf("  %-18s %d\n", "Threads:", threads)
	fmt.Printf("  %-18s %v\n", "Multi-engine:", multi)
	fmt.Println("============================================================")
	fmt.Println("  No file was created, changed or deleted.")
	fmt.Println("============================================================")
}

func main() {
	if len(os.Args) < 2 {
		checkCmd(nil) // default command = check (parity with check_users.py)
		return
	}
	switch os.Args[1] {
	case "sessions":
		sessionsCmd(os.Args[2:])
	case "login":
		loginCmd(os.Args[2:])
	case "check":
		checkCmd(os.Args[2:])
	case "fetch":
		fetchCmd(os.Args[2:])
	case "fetchhtml":
		fetchHTMLCmd(os.Args[2:])
	case "probe":
		probeCmd(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `usage: userkarmacheck <command> [flags]

commands:
  login [flags]        open real Chrome, log in manually, save cookies.json
  check [flags]        check a list of usernames (worker pool)
  fetch <username>     fetch one user (JSON first, HTML fallback)
  probe                verify a cookie file against /api/v1/me
`)
}

// fetchHTMLCmd — HTML-only fetch (debug helper).
func fetchHTMLCmd(args []string) {
	fs := flag.NewFlagSet("fetchhtml", flag.ExitOnError)
	proxy := fs.String("p", "", "proxy: host:port[:user:pass]")
	timeout := fs.Duration("timeout", 30*time.Second, "request timeout")
	fs.Parse(args)
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: userkarmacheck fetchhtml <username> [-p proxy]")
		os.Exit(2)
	}
	c, err := newClient(*proxy, *timeout, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "client: %v\n", err)
		os.Exit(1)
	}
	res := c.FetchUserHTML(context.Background(), fs.Arg(0))
	out, _ := json.MarshalIndent(res, "", "  ")
	fmt.Println(string(out))
	if res.OK {
		os.Exit(0)
	}
	os.Exit(5)
}

// sessionsCmd - validate saved cookie sessions inside a real browser
// (cookie injection only - no user:pass is ever submitted).
func sessionsCmd(args []string) {
	fs := flag.NewFlagSet("sessions", flag.ExitOnError)
	dir := fs.String("dir", "sassion", "session folder to scan")
	headless := fs.Bool("headless", false, "run Chrome headless")
	timeout := fs.Duration("timeout", 30*time.Second, "per-page timeout")
	fs.Parse(args)

	files := FindSessionFiles(*dir)
	if len(files) == 0 {
		fmt.Printf("No session files found in %s\n", *dir)
		fmt.Println("Put cookies.json or a raw 'Cookie:' header .txt file there")
		fmt.Println("(one sub-folder per account is supported).")
		os.Exit(1)
	}
	fmt.Printf("Validating %d session file(s) in %s\n", len(files), *dir)
	fmt.Println("Method: real Chrome + cookie injection (no credentials submitted)")
	results := ValidateSessions(*dir, *headless, *timeout)

	valid, dead, banned, unknown := 0, 0, 0, 0
	for _, r := range results {
		switch r.State {
		case SessValid:
			valid++
		case SessDead:
			dead++
		case SessBanned:
			banned++
		default:
			unknown++
		}
	}
	fmt.Printf("\nvalid=%d dead=%d banned=%d unknown=%d (total %d)\n",
		valid, dead, banned, unknown, len(results))
	fmt.Printf("Report: %s\n", filepath.Join(*dir, "sassion_report.txt"))
}

// loginCmd — browser login (rod -> real Chrome).
func loginCmd(args []string) {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	timeout := fs.Duration("timeout", 300*time.Second, "how long to wait for the manual login")
	profile := fs.String("profile", "", "browser profile dir (default: unique per run)")
	headless := fs.Bool("headless", false, "run without a visible window")
	fs.Parse(args)
	os.Exit(runLogin(LoginFlags{Timeout: *timeout, Profile: *profile, Headless: *headless}))
}

// checkCmd — the full checker with Python-compatible flags.
func checkCmd(args []string) {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	input := fs.String("i", "input.txt", "input list, user:pass lines")
	output := fs.String("o", ".", "output directory for karma files")
	threads := fs.Int("threads", 4, "concurrent workers")
	proxiesFile := fs.String("proxies", "", "proxy list file (host:port[:user:pass])")
	cookiesFile := fs.String("cookies", "cookies.json", "cookie file (\"\" = none)")
	delay := fs.Float64("delay", 0.4, "delay after each user (seconds)")
	rate := fs.Float64("rate", 60.0, "requests per minute per IP (0 = no bucket)")
	burst := fs.Int("burst", 2, "burst size per IP (keep small - bursts get blocked)")
	timeout := fs.Duration("timeout", 30*time.Second, "request timeout")
	retries := fs.Int("retries", 3, "retries on network/rate-limit errors")
	maxBlocked := fs.Int("max-blocked", 10, "abort after N blocked/unauthorized in a row")
	noPreflight := fs.Bool("no-preflight", false, "skip the startup auth check")
	noResume := fs.Bool("no-resume", false, "re-check usernames already saved")
	mode := fs.String("mode", "auto", "auto | json | html")
	token := fs.String("token", "", "raw Reddit OAuth access token")
	cookieString := fs.String("cookie-string", "", "raw 'Cookie:' header")
	clientID := fs.String("client-id", "", "Reddit app client id (OAuth)")
	clientSecret := fs.String("client-secret", "", "Reddit app client secret (OAuth)")
	sessionsDir := fs.String("sessions", "", "session folder -> multi-session mode")
	ratePerIP := fs.Float64("rate-per-ip", 600, "requests per minute per IP (multi mode)")
	ratePerSession := fs.Float64("rate-per-session", 72, "requests per minute per session/account")
	lanesPerIP := fs.Int("lanes-per-ip", 0, "max lanes per IP (0 = every session)")
	warmup := fs.Int("warmup", 10, "slow first N requests (multi mode)")
	lanes := fs.Int("lanes", 0, "lane pool size (0 = one lane per session)")
	maxPerSession := fs.Int("max-per-session", 25, "users one session serves before the lane rotates")
	aimd := fs.Bool("aimd", true, "closed-loop rate control (auto slow down on blocks)")
	preflightSample := fs.Int("preflight-sample", 40, "sessions to probe at startup (0 = all)")
	vpnPath := fs.String("vpn", DefaultExpressVPN, "expressvpnctl.exe path for IP rotation on rate limit")
	vpnRotate := fs.Bool("vpn-rotate", true, "rotate the VPN IP when a rate limit is hit")
	vpnLocations := fs.String("vpn-locations", "", "file with VPN region ids, one per line (default: auto-discover)")
	vpnCountry := fs.String("vpn-country", "", "only rotate VPN regions containing this text, e.g. usa (\"\" = all)")
	vpnGap := fs.Float64("vpn-gap", 1, "seconds between VPN disconnect and connect")
	vpnConnectTimeout := fs.Float64("vpn-connect-timeout", 45, "max seconds waiting for the VPN Connected state")
	vpnThreshold := fs.Int("vpn-threshold", 6, "consecutive throttled results before a VPN rotation")
	vpnMaxRotations := fs.Int("vpn-max-rotations", 30, "max VPN rotations per run (0 = unlimited)")
	vpnMinInterval := fs.Float64("vpn-min-interval", 30, "minimum seconds between VPN rotations")
	vpnNetworkLock := fs.Bool("vpn-network-lock", false, "enable ExpressVPN Network Lock (kill switch) for the run")
	vpnVerifyIP := fs.Bool("vpn-verify-ip", true, "verify the exit IP really changed after a rotation (api.ipify.org)")
	browserPages := fs.Int("browser-pages", 4, "parallel Chrome tabs for the NSFW browser fallback")
	browserBudget := fs.Int("browser-budget", 60, "max browser fallback fetches per run (0 = unlimited)")
	quiet := fs.Bool("quiet", false, "quiet mode: no per-user progress lines, only the final run report")
	// ---- advanced rate-limit avoidance (advanced.go) ----
	adaptiveRate := fs.Bool("adaptive-rate", true, "auto-adjust the per-group rate from the observed success rate")
	minRate := fs.Float64("min-rate", 0, "floor for the adaptive rate per group (0 = auto: 15 without proxy, 30 with)")
	maxRate := fs.Float64("max-rate", 0, "ceiling for the adaptive rate per group (0 = auto: 60 without proxy, 120 with)")
	rateSpeed := fs.Float64("rate-speed", 0, "how fast the adaptive rate moves (0 = auto: 0.15 without proxy, 0.5 with)")
	sourceTracking := fs.Bool("source-tracking", true, "remember which session/proxy actually returns data")
	debugFlag := fs.Bool("debug", false, "print the rate-bucket and source-health panels (also KARMA_DEBUG=1)")
	multiEngine := fs.Bool("multi-engine", false, "run several independent checkers (session + anon) that share one queue")
	engineWorkers := fs.Int("engine-workers", 12, "workers per engine in multi-engine mode")
	dryRun := fs.Bool("dry-run", false, "show the plan (counts, threads, lanes) and exit WITHOUT touching any file")
	pprofAddr := fs.String("pprof", "", "start the pprof profiler on this address (e.g. :6060) - loopback only")
	fs.Parse(args)
	SetQuiet(*quiet)
	if *debugFlag {
		os.Setenv("KARMA_DEBUG", "1")
	}
	// the profiler: at 200-500 threads you need a CPU profile to see where
	// the time really goes (TLS handshakes vs bucket contention vs sleeping)
	StartProfiler(*pprofAddr)
	// Ctrl+C must flush the result buffers so the next run can resume from
	// the exact point where the run was stopped.
	InstallSignalFlush()

	vpnCtl := NewVPNController(VPNConfig{
		Path:           *vpnPath,
		Rotate:         *vpnRotate,
		RegionsFile:    *vpnLocations,
		Country:        *vpnCountry,
		Gap:            time.Duration(*vpnGap * float64(time.Second)),
		ConnectTimeout: time.Duration(*vpnConnectTimeout * float64(time.Second)),
		Threshold:      *vpnThreshold,
		MaxRotations:   *vpnMaxRotations,
		MinInterval:    time.Duration(*vpnMinInterval * float64(time.Second)),
		NetworkLock:    *vpnNetworkLock,
		VerifyIP:       *vpnVerifyIP,
	})

	users, err := LoadLines(*input)
	if err != nil {
		fmt.Fprintf(os.Stderr, "input: %v\n", err)
		os.Exit(1)
	}
	if len(users) == 0 {
		fmt.Printf("No usernames in %s\n", *input)
		os.Exit(1)
	}

	// validate the credentials BEFORE spending any Reddit request (the same
	// rules as the Python script): broken lines are reported and skipped.
	cred := FilterCredentials(users)
	if !Quiet() {
		cred.Print()
	}
	rawInputTotal := len(users)
	users = cred.Lines
	if len(users) == 0 {
		fmt.Println("No valid credential pairs to check.")
		os.Exit(1)
	}

	var proxies []string
	if *proxiesFile != "" {
		if raw, err := os.ReadFile(*proxiesFile); err == nil {
			for _, l := range strings.Split(string(raw), "\n") {
				l = strings.TrimSpace(l)
				if l != "" && !strings.HasPrefix(l, "#") {
					proxies = append(proxies, l)
				}
			}
		}
	}

	// multi-session mode: every cookie session in the folder becomes a lane
	if *sessionsDir != "" {
		sessions, err := LoadSessionPool(*sessionsDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "sessions: %v\n", err)
			os.Exit(1)
		}

		// ---- dry run: show the plan, touch nothing -------------------
		if *dryRun {
			uniq, dupes := DedupLines(users)
			_, skipped, _ := PrepareWork(users, *output, *noResume)
			lanes := *lanes
			if lanes <= 0 || lanes > len(sessions) {
				lanes = len(sessions)
			}
			threads := planThreads(*threads, lanes, len(sessions))
			printPlan(*input, *output, threads, lanes, len(sessions), len(uniq), dupes, skipped, *multiEngine)
			return
		}
		var mproxies []string
		if *proxiesFile != "" {
			if raw, err := os.ReadFile(*proxiesFile); err == nil {
				for _, l := range strings.Split(string(raw), "\n") {
					l = strings.TrimSpace(l)
					if l != "" && !strings.HasPrefix(l, "#") {
						mproxies = append(mproxies, l)
					}
				}
			}
		}
		fmt.Printf("Multi-session mode: %d session(s) from %s\n", len(sessions), *sessionsDir)

		// ---- safe concurrency ------------------------------------------
		// The requested thread count is validated against the lane and
		// session counts so a wrong number can never waste resources.
		safeLanes := *lanes
		if safeLanes <= 0 || safeLanes > len(sessions) {
			safeLanes = len(sessions)
		}
		safeThreads := planThreads(*threads, safeLanes, len(sessions))
		if safeThreads != *threads {
			fmt.Printf("Thread count adjusted: %d -> %d (lanes=%d, sessions=%d)\n",
				*threads, safeThreads, safeLanes, len(sessions))
		}

		cfg := MultiConfig{
			Input: *input, Output: *output, Workers: safeThreads,
			Proxies: mproxies, Sessions: sessions,
			RatePerIP: *ratePerIP, RatePerSession: *ratePerSession, Burst: *burst, LanesPerIP: *lanesPerIP,
			Lanes: safeLanes, MaxPerSession: *maxPerSession, AIMD: *aimd, PreflightSample: *preflightSample,
			VPN:    vpnCtl,
			Warmup: *warmup, Timeout: *timeout, Retries: *retries,
			MaxBlocked: *maxBlocked, NoPreflight: *noPreflight, NoResume: *noResume,
			Token:        *token,
			BrowserPages: *browserPages, BrowserBudget: *browserBudget,
			InputTotal: int64(rawInputTotal), InputRejected: int64(cred.Total - cred.Valid),
			// advanced rate-limit avoidance
			AdaptiveRate:    *adaptiveRate,
			MinRatePerGroup: *minRate,
			MaxRatePerGroup: *maxRate,
			RateAdjustment:  *rateSpeed,
			SourceTracking:  *sourceTracking,
			NoProxyMode:     len(mproxies) == 0,
		}
		ch := NewMultiChecker(cfg)

		// ---- multi-engine mode -------------------------------------------
		// Several independent checkers share ONE queue. When one of them
		// gets blocked it parks itself and the others keep the run going;
		// when every engine is parked the VPN rotates and they all resume.
		if *multiEngine {
			// the same duplicate + resume protection the single-engine path
			// has - without it the multi-engine runner would re-check
			// accounts that earlier runs already saved.
			todo, skipped, dupes := PrepareWork(users, *output, *noResume)
			if dupes > 0 {
				fmt.Printf("Removed %d duplicate user:pass row(s) from the input.\n", dupes)
			}
			if skipped > 0 {
				fmt.Printf("Skipped %d account(s) already checked by an earlier run.\n", skipped)
			}
			if len(todo) == 0 {
				fmt.Println("Nothing to do - all usernames already checked.")
				return
			}
			specs := []EngineSpec{
				{Name: "sessions", SessionDir: *sessionsDir, MaxLanes: *lanes,
					Workers: *engineWorkers, RatePerIP: *ratePerIP, RatePerSess: *ratePerSession},
				{Name: "sessions-b", SessionDir: *sessionsDir, MaxLanes: 10,
					Workers: 6, RatePerIP: 60, RatePerSess: 20},
				{Name: "anonymous", SessionDir: "", MaxLanes: 4,
					Workers: 4, RatePerIP: 30, RatePerSess: 10},
			}
			engines := MustBuildEngines(specs, cfg)
			if len(engines) == 0 {
				fmt.Fprintln(os.Stderr, "no engines could be built")
				os.Exit(1)
			}
			fmt.Printf("Multi-engine mode: %d engine(s) sharing one queue\n", len(engines))
			for _, e := range engines {
				fmt.Printf("  engine %-12s workers=%d rate=%d/min\n", e.Name, e.Workers, int(e.Rate))
			}
			runner := NewMultiEngineRunner(engines, todo, vpnCtl)
			runner.Run(context.Background())
			return
		}

		if !ch.Run(context.Background(), users) {
			os.Exit(1)
		}
		return
	}

	var cookies []Cookie
	if *cookieString != "" {
		cookies = ParseCookieString(*cookieString)
	} else if *cookiesFile != "" {
		if cs, err := LoadCookies(*cookiesFile); err == nil {
			cookies = cs
		}
	}

	tp := NewTokenProvider(*clientID, *clientSecret, *timeout)

	htmlMode := false
	switch *mode {
	case "html":
		htmlMode = true
	case "auto":
		htmlMode = len(cookies) == 0 && *token == "" && !tp.Enabled()
	case "json":
		htmlMode = false
	}

	cfg := CheckConfig{
		Input: *input, Output: *output, Workers: *threads,
		Proxies: proxies, Cookies: cookies, Token: *token,
		TokenProvider: tp,
		HTMLMode:      htmlMode, Delay: time.Duration(*delay * float64(time.Second)),
		Rate: *rate, Burst: *burst, Timeout: *timeout, Retries: *retries,
		MaxBlocked: *maxBlocked, NoPreflight: *noPreflight, NoResume: *noResume,
		VPN:          vpnCtl,
		BrowserPages: *browserPages, BrowserBudget: *browserBudget,
		InputTotal: int64(rawInputTotal), InputRejected: int64(cred.Total - cred.Valid),
	}
	ch := NewChecker(cfg)
	if !ch.Run(context.Background(), users) {
		os.Exit(1)
	}
}

func fetchCmd(args []string) {
	fs := flag.NewFlagSet("fetch", flag.ExitOnError)
	proxy := fs.String("p", "", "proxy: host:port[:user:pass]")
	timeout := fs.Duration("timeout", 30*time.Second, "request timeout")
	fs.Parse(args)
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: userkarmacheck fetch <username> [-p proxy]")
		os.Exit(2)
	}
	user := fs.Arg(0)

	c, err := newClient(*proxy, *timeout, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "client: %v\n", err)
		os.Exit(1)
	}
	start := time.Now()
	res := c.FetchUser(context.Background(), user, "", true)
	elapsed := time.Since(start)

	out, _ := json.MarshalIndent(res, "", "  ")
	fmt.Println(string(out))
	fmt.Fprintf(os.Stderr, "source=%s elapsed=%.1fs\n", res.Source, elapsed.Seconds())
	if res.OK {
		os.Exit(0)
	}
	switch {
	case res.NotFound:
		os.Exit(3)
	case res.Banned:
		os.Exit(4)
	default:
		os.Exit(5)
	}
}

func probeCmd(args []string) {
	fs := flag.NewFlagSet("probe", flag.ExitOnError)
	proxy := fs.String("p", "", "proxy: host:port[:user:pass]")
	cookieFile := fs.String("c", "cookies.json", "cookie file to verify")
	timeout := fs.Duration("timeout", 30*time.Second, "request timeout")
	fs.Parse(args)

	cookies, err := LoadCookies(*cookieFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cookies: %v\n", err)
		os.Exit(1)
	}
	c, err := newClient(*proxy, *timeout, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "client: %v\n", err)
		os.Exit(1)
	}
	state, account, karma, detail := c.SessionState(context.Background(), cookies)
	if state == SessionValid {
		fmt.Printf("state=valid account=%s karma=%d\n", account, karma)
	} else {
		fmt.Printf("state=%s detail=%s\n", state, detail)
	}
	if state != SessionValid {
		os.Exit(6)
	}
}
