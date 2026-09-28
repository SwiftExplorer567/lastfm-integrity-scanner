package ui

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/swiftexplorer567/lastfm-integrity-scanner/internal/lastfm"
	"github.com/swiftexplorer567/lastfm-integrity-scanner/internal/mockfm"
	"github.com/swiftexplorer567/lastfm-integrity-scanner/internal/scanner"
	"github.com/swiftexplorer567/lastfm-integrity-scanner/internal/synth"
)

func console(t *testing.T) (*httptest.Server, *scanner.Scanner) {
	t.Helper()
	mock := mockfm.New()
	for i, u := range []struct {
		name string
		p    synth.Profile
	}{{"bot", synth.Faker}, {"human", synth.Honest}} {
		start := time.Now().AddDate(-1, 0, 0)
		g := synth.Generate(u.name, synth.Options{Profile: u.p, Seed: uint64(i + 1), Start: start, Days: 360})
		mock.AddUser(&mockfm.User{History: g.History, Registered: start.Unix(), Durations: g.Durations})
	}
	fm := httptest.NewServer(mock)
	t.Cleanup(fm.Close)
	client := lastfm.New(lastfm.Config{APIKey: "k", BaseURL: fm.URL, RPS: 100, Burst: 200, FixedRate: true})
	s, err := scanner.New(client, t.TempDir(), scanner.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	s.Opts.Check.Patient = true
	c, err := New(s, 2, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(c.Handler())
	t.Cleanup(srv.Close)
	return srv, s
}

func call(t *testing.T, srv *httptest.Server, method, path, body string, out any) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 {
		t.Fatalf("%s %s: %d %s", method, path, res.StatusCode, b)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("%s %s: %v: %s", method, path, err, b)
		}
	}
	return res
}

func TestConsoleFlow(t *testing.T) {
	srv, _ := console(t)

	// Profile links, duplicates and separators are all accepted.
	var q struct{ Queued int }
	call(t, srv, "POST", "/api/check", `{"users":"https://www.last.fm/user/bot, human\nbot  nobody"}`, &q)
	if q.Queued != 3 {
		t.Fatalf("queued %d, want 3", q.Queued)
	}
	var jobs struct{ Jobs []job }
	deadline := time.Now().Add(30 * time.Second)
	for {
		call(t, srv, "GET", "/api/jobs", "", &jobs)
		done := 0
		for _, j := range jobs.Jobs {
			if j.Status != "queued" && j.Status != "running" {
				done++
			}
		}
		if done == len(jobs.Jobs) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("checks did not finish: %+v", jobs.Jobs)
		}
		time.Sleep(100 * time.Millisecond)
	}
	status := map[string]string{}
	for _, j := range jobs.Jobs {
		status[j.User] = j.Status
	}
	if status["bot"] != "block" || status["human"] != "pass" || status["nobody"] != "not found" {
		t.Fatalf("job results %v", status)
	}

	call(t, srv, "POST", "/api/labels/bot", `{"label":"fake","note":"same 5 songs all night"}`, nil)
	call(t, srv, "POST", "/api/labels/human", `{"label":"fake"}`, nil)
	var acc struct{ Accounts []Account }
	call(t, srv, "GET", "/api/accounts", "", &acc)
	if len(acc.Accounts) != 2 {
		t.Fatalf("%d accounts, want 2", len(acc.Accounts))
	}
	for _, a := range acc.Accounts {
		if a.Label != "fake" || !a.Capture || a.CheckedAt == "" {
			t.Errorf("%s: label %q capture %v", a.User, a.Label, a.Capture)
		}
		if a.User == "bot" && (a.Note == "" || a.Decision != "block") {
			t.Errorf("bot: %+v", a)
		}
	}

	for _, f := range []string{"html", "json", "png", "pdf", "svg"} {
		for _, u := range []string{"bot", "human"} {
			res := call(t, srv, "GET", "/api/accounts/"+u+"/report."+f+"?download=1", "", nil)
			if cd := res.Header.Get("Content-Disposition"); !strings.Contains(cd, "check-") {
				t.Errorf("%s.%s: Content-Disposition %q", u, f, cd)
			}
		}
	}

	var rc struct {
		Rechecked int
		Changes   []change
	}
	call(t, srv, "POST", "/api/recheck", "", &rc)
	if rc.Rechecked != 2 || len(rc.Changes) != 0 {
		t.Errorf("recheck: %+v; the same rules on the same data must not change anything", rc)
	}
	call(t, srv, "GET", "/api/accounts", "", &acc)
	for _, a := range acc.Accounts {
		if a.Seconds == 0 {
			t.Errorf("%s: re-score lost the check's duration", a.User)
		}
	}

	req, _ := http.NewRequest("GET", srv.URL+"/api/export", nil)
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r.Body)
	r.Body.Close()
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
	}
	for _, want := range []string{"checks/bot.json.gz", "checks/human.json.gz", "labels.json", "summary.csv"} {
		if !names[want] {
			t.Errorf("export lacks %s (has %v)", want, names)
		}
	}

	// Clearing the label and note removes it.
	call(t, srv, "POST", "/api/labels/human", `{"label":""}`, nil)
	call(t, srv, "GET", "/api/accounts", "", &acc)
	for _, a := range acc.Accounts {
		if a.User == "human" && a.Label != "" {
			t.Errorf("human label not cleared: %q", a.Label)
		}
	}
	idx := call(t, srv, "GET", "/", "", nil)
	if !strings.HasPrefix(idx.Header.Get("Content-Type"), "text/html") {
		t.Error("index not served")
	}
}

func TestConsoleWithoutAPIKey(t *testing.T) {
	s, err := scanner.New(nil, t.TempDir(), scanner.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(s, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(c.Handler())
	defer srv.Close()
	var st map[string]any
	call(t, srv, "GET", "/api/status", "", &st)
	if st["api_key"] != false {
		t.Errorf("status %v", st)
	}
	res, err := http.Post(srv.URL+"/api/check", "application/json", strings.NewReader(`{"users":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("check without key: %d", res.StatusCode)
	}
	call(t, srv, "GET", "/api/accounts", "", nil)
	call(t, srv, "POST", "/api/recheck", "", nil)
}
