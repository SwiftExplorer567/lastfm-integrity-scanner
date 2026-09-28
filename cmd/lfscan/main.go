// Command lfscan scans Last.fm accounts for faked scrobbles.
//
//	lfscan check <user>... [-format json,html,png] [-out dir]
//	lfscan scan <user>... [-mode auto|full|quick] [-format html,pdf,png] [-out dir]
//	lfscan serve [-addr :8080]
//	lfscan render <report.json> [-format html,pdf,png,svg] [-out dir]
//	lfscan demo [-out dir]
//
// Configuration comes from the environment (or a .env file in the working
// directory): LASTFM_API_KEY (required for scan/serve), LFSCAN_DATA_DIR,
// LFSCAN_TOKEN, LFSCAN_RPS, LFSCAN_WORKERS, LFSCAN_ADDR.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/swiftexplorer567/lastfm-integrity-scanner/internal/analysis"
	"github.com/swiftexplorer567/lastfm-integrity-scanner/internal/lastfm"
	"github.com/swiftexplorer567/lastfm-integrity-scanner/internal/mockfm"
	"github.com/swiftexplorer567/lastfm-integrity-scanner/internal/render"
	"github.com/swiftexplorer567/lastfm-integrity-scanner/internal/scanner"
	"github.com/swiftexplorer567/lastfm-integrity-scanner/internal/server"
	"github.com/swiftexplorer567/lastfm-integrity-scanner/internal/synth"
)

func main() {
	loadDotEnv(".env")
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch os.Args[1] {
	case "scan":
		err = cmdScan(ctx, os.Args[2:])
	case "check":
		err = cmdCheck(ctx, os.Args[2:])
	case "serve":
		err = cmdServe(ctx, os.Args[2:])
	case "render":
		err = cmdRender(os.Args[2:])
	case "demo":
		err = cmdDemo(os.Args[2:])
	case "demo-check":
		err = cmdDemoCheck(ctx, os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `lfscan: Last.fm scrobble integrity scanner

  lfscan check <user>... [-format json,html,png] [-out dir]   pre-import check, under 10 s per account
  lfscan scan <user>... [-mode auto|full|quick] [-format json,html,pdf,png,svg] [-out dir] [-refresh]
  lfscan serve [-addr :8080]
  lfscan render <report.json> [-format html,pdf,png,svg] [-out dir]
  lfscan demo [-out dir]          reports for generated example accounts, no API needed
  lfscan demo-check [-out dir]    pre-import checks against a built-in fake Last.fm, no API needed

Environment: LASTFM_API_KEY, LFSCAN_DATA_DIR (./data), LFSCAN_TOKEN, LFSCAN_RPS (5),
LFSCAN_WORKERS (8), LFSCAN_ADDR (:8080). A .env file in the working directory is read too.
`)
}

func newScanner() (*scanner.Scanner, error) {
	key := os.Getenv("LASTFM_API_KEY")
	if key == "" {
		return nil, errors.New("LASTFM_API_KEY is not set")
	}
	client := lastfm.New(lastfm.Config{
		APIKey:  key,
		BaseURL: os.Getenv("LASTFM_BASE_URL"),
		RPS:     envFloat("LFSCAN_RPS", 5),
		Burst:   int(envFloat("LFSCAN_BURST", 10)),
	})
	opts := scanner.DefaultOptions()
	opts.Workers = int(envFloat("LFSCAN_WORKERS", float64(opts.Workers)))
	opts.AutoFullMaxScrobbles = int64(envFloat("LFSCAN_AUTO_FULL_MAX", float64(opts.AutoFullMaxScrobbles)))
	opts.DurationLookups = int(envFloat("LFSCAN_DURATION_LOOKUPS", float64(opts.DurationLookups)))
	return scanner.New(client, envStr("LFSCAN_DATA_DIR", "data"), opts)
}

func cmdScan(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("scan", flag.ExitOnError)
	mode := fs.String("mode", "auto", "auto, full or quick")
	formats := fs.String("format", "json,html", "comma-separated: json,html,pdf,png,svg")
	out := fs.String("out", "reports", "directory for report files")
	refresh := fs.Bool("refresh", false, "ignore the local cache and download again")
	users, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	if len(users) == 0 {
		return errors.New("scan: give at least one Last.fm username")
	}
	fmts, err := parseFormats(*formats)
	if err != nil {
		return err
	}
	s, err := newScanner()
	if err != nil {
		return err
	}
	failed := 0
	for _, u := range users {
		last := time.Time{}
		rep, err := s.Scan(ctx, scanner.Request{User: u, Mode: *mode, Refresh: *refresh}, func(stage string, done, total int) {
			if time.Since(last) < 500*time.Millisecond && done != total {
				return
			}
			last = time.Now()
			if total > 0 {
				fmt.Fprintf(os.Stderr, "\r%-12s %s: %d/%d   ", u, stage, done, total)
			} else {
				fmt.Fprintf(os.Stderr, "\r%-12s %s…          ", u, stage)
			}
		})
		fmt.Fprint(os.Stderr, "\r\033[K")
		if rep == nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", u, err)
			failed++
			continue
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: warning: %v\n", u, err)
		}
		printSummary(rep)
		if err := writeReports(rep, fmts, *out); err != nil {
			return err
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d scans failed", failed, len(users))
	}
	return nil
}

func cmdCheck(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	formats := fs.String("format", "json,html,png", "comma-separated: json,html,pdf,png,svg")
	out := fs.String("out", "reports", "directory for report files")
	users, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	if len(users) == 0 {
		return errors.New("check: give at least one Last.fm username")
	}
	fmts, err := parseFormats(*formats)
	if err != nil {
		return err
	}
	s, err := newScanner()
	if err != nil {
		return err
	}
	return runChecks(ctx, s, users, fmts, *out)
}

func runChecks(ctx context.Context, s *scanner.Scanner, users []string, fmts []render.Format, out string) error {
	failed := 0
	for _, u := range users {
		rep, err := s.Check(ctx, u, true)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", u, err)
			failed++
			continue
		}
		g := rep.Gate
		fmt.Printf("%-24s %-7s score %3d  %s scrobbles  %.1fs  %d requests  %d/%d periods  %s plays sampled\n",
			rep.User.Name, strings.ToUpper(g.Decision), rep.Score.Value, analysis.Num(rep.User.Playcount),
			float64(g.ElapsedMS)/1000, g.Requests, g.PeriodsRead, g.Periods, analysis.Num(g.SampledPlay))
		fmt.Printf("    %s\n", g.Reason)
		for _, sg := range rep.Signals {
			if sg.Points > 0 {
				fmt.Printf("    %+4d  %s: %s\n", sg.Points, sg.Title, sg.Detail)
			}
		}
		if err := writeReports(rep, fmts, out); err != nil {
			return err
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d checks failed", failed, len(users))
	}
	return nil
}

// cmdDemoCheck runs the pre-import check against an in-process fake
// Last.fm serving generated accounts, with realistic latency and the real
// rate limit, so the check can be tried without an API key.
func cmdDemoCheck(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("demo-check", flag.ExitOnError)
	out := fs.String("out", "reports/demo-check", "directory for report files")
	formats := fs.String("format", "json,html,pdf,png", "comma-separated formats")
	fs.Parse(args)
	fmts, err := parseFormats(*formats)
	if err != nil {
		return err
	}
	mock := mockfm.New()
	mock.Latency = 150 * time.Millisecond
	accounts := []struct {
		name string
		p    synth.Profile
		days int
	}{
		{"demo-bot-4m", synth.Faker, 5 * 365},
		{"demo-veteran", synth.Honest, 10 * 365},
		{"demo-two-devices", synth.TwoDevices, 3 * 365},
		{"demo-double-scrobbler", synth.DoubleScrobbler, 2 * 365},
		{"demo-scripted", synth.Scripted, 365},
	}
	fmt.Fprintln(os.Stderr, "generating accounts…")
	var names []string
	for i, a := range accounts {
		start := time.Now().AddDate(0, 0, -a.days-1)
		g := synth.Generate(a.name, synth.Options{Profile: a.p, Seed: uint64(40 + i), Start: start, Days: a.days})
		mock.AddUser(&mockfm.User{History: g.History, Registered: start.Unix(), Durations: g.Durations})
		names = append(names, a.name)
	}
	srv := httptest.NewServer(mock)
	defer srv.Close()
	client := lastfm.New(lastfm.Config{APIKey: "demo", BaseURL: srv.URL})
	s, err := scanner.New(client, filepath.Join(os.TempDir(), "lfscan-demo"), scanner.DefaultOptions())
	if err != nil {
		return err
	}
	return runChecks(ctx, s, names, fmts, *out)
}

func cmdServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", envStr("LFSCAN_ADDR", ":8080"), "listen address")
	concurrent := fs.Int("concurrent", 2, "scans running at once")
	fs.Parse(args)
	s, err := newScanner()
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	token := os.Getenv("LFSCAN_TOKEN")
	if token == "" {
		log.Warn("LFSCAN_TOKEN is not set: the API is open to anyone who can reach it")
	}
	srv := &http.Server{
		Addr:              *addr,
		Handler:           server.New(s, server.Config{Token: token, MaxConcurrent: *concurrent}, log).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shut)
	}()
	log.Info("listening", "addr", *addr)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func cmdRender(args []string) error {
	fs := flag.NewFlagSet("render", flag.ExitOnError)
	formats := fs.String("format", "html,pdf,png", "comma-separated: json,html,pdf,png,svg")
	out := fs.String("out", "reports", "directory for report files")
	files, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	fmts, err := parseFormats(*formats)
	if err != nil {
		return err
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		var rep analysis.Report
		if err := json.Unmarshal(b, &rep); err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
		if err := writeReports(&rep, fmts, *out); err != nil {
			return err
		}
	}
	return nil
}

// cmdDemo analyzes generated accounts, one per kind of listener, so the
// reports can be seen without an API key.
func cmdDemo(args []string) error {
	fs := flag.NewFlagSet("demo", flag.ExitOnError)
	out := fs.String("out", "reports/demo", "directory for report files")
	formats := fs.String("format", "json,html,pdf,png,svg", "comma-separated formats")
	fs.Parse(args)
	fmts, err := parseFormats(*formats)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for i, p := range synth.Profiles {
		start := now.AddDate(-1, 0, 0)
		g := synth.Generate("demo-"+string(p), synth.Options{Profile: p, Seed: uint64(100 + i), Start: start, Days: 364})
		durs := map[uint32]int{}
		for idx, t := range g.History.Tracks {
			if d, ok := g.Durations[t]; ok {
				durs[uint32(idx)] = d
			}
		}
		t0 := time.Now()
		rep := analysis.Analyze(analysis.Input{
			Profile:   analysis.Profile{Name: g.History.User, Playcount: int64(len(g.History.Plays)), Registered: start.Unix()},
			History:   g.History,
			Scope:     "full",
			From:      g.History.First(),
			To:        now.Unix(),
			Durations: durs,
			Params:    analysis.DefaultParams(),
			Now:       now,
		})
		rep.Measurement.ElapsedSec = time.Since(t0).Seconds()
		printSummary(rep)
		if err := writeReports(rep, fmts, *out); err != nil {
			return err
		}
	}
	return nil
}

func printSummary(r *analysis.Report) {
	fmt.Printf("%-24s score %3d %-12s leaderboard: %-7s scrobbles %s → adjusted %s (%s, %s plays analyzed, %.1fs)\n",
		r.User.Name, r.Score.Value, r.Score.Label, r.Leaderboard.Action,
		analysis.Num(r.Leaderboard.RawScrobbles), analysis.Num(r.Leaderboard.AdjustedScrobbles),
		r.Measurement.Scope, analysis.Num(r.Integrity.PlaysAnalyzed), r.Measurement.ElapsedSec)
	for _, s := range r.Signals {
		fmt.Printf("    %+4d  %s: %s\n", s.Points, s.Title, s.Detail)
	}
}

func writeReports(r *analysis.Report, fmts []render.Format, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, f := range fmts {
		path := filepath.Join(dir, render.Filename(r, f))
		file, err := os.Create(path)
		if err != nil {
			return err
		}
		w := bufio.NewWriter(file)
		if err := render.Render(w, r, f); err != nil {
			file.Close()
			return fmt.Errorf("%s: %w", path, err)
		}
		if err := w.Flush(); err != nil {
			file.Close()
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
		fmt.Printf("    wrote %s\n", path)
	}
	return nil
}

func parseFormats(s string) ([]render.Format, error) {
	var out []render.Format
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part == "" {
			continue
		}
		f, err := render.ParseFormat(part)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}

// parseInterleaved accepts flags before or after positional arguments.
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func envStr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envFloat(k string, def float64) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(k), 64); err == nil && v > 0 {
		return v
	}
	return def
}

// loadDotEnv sets variables from a KEY=VALUE file without overriding the
// real environment.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(strings.TrimPrefix(k, "export "))
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if _, set := os.LookupEnv(k); !set {
			os.Setenv(k, v)
		}
	}
}
