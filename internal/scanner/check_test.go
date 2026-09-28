package scanner

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/swiftexplorer567/lastfm-integrity-scanner/internal/lastfm"
	"github.com/swiftexplorer567/lastfm-integrity-scanner/internal/mockfm"
	"github.com/swiftexplorer567/lastfm-integrity-scanner/internal/synth"
)

// checkEnv serves generated accounts through a mock with real-world latency
// and the production rate limit, so elapsed times mean something.
func checkEnv(t *testing.T, latency time.Duration, users map[string]struct {
	p    synth.Profile
	days int
}) (*Scanner, *mockfm.Server) {
	t.Helper()
	mock := mockfm.New()
	mock.Latency = latency
	for name, u := range users {
		start := time.Now().AddDate(0, 0, -u.days-1)
		g := synth.Generate(name, synth.Options{Profile: u.p, Seed: 5, Start: start, Days: u.days})
		mock.AddUser(&mockfm.User{History: g.History, Registered: start.Unix(), Durations: g.Durations})
	}
	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)
	client := lastfm.New(lastfm.Config{APIKey: "k", BaseURL: srv.URL, RetryBase: 50 * time.Millisecond, FixedRate: true}) // default 4.5/s, burst 60
	s, err := New(client, t.TempDir(), DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	return s, mock
}

func TestCheckDecisions(t *testing.T) {
	if testing.Short() {
		t.Skip("generates millions of plays")
	}
	type u = struct {
		p    synth.Profile
		days int
	}
	cases := []struct {
		user string
		u    u
		want string
	}{
		{"bigbot", u{synth.Faker, 5 * 365}, GateBlock},         // ~4.6M scrobbles
		{"veteran", u{synth.Honest, 10 * 365}, GatePass},       // 10-year account
		{"twodevices", u{synth.TwoDevices, 3 * 365}, GatePass}, // phone + forgotten tab
		{"doubler", u{synth.DoubleScrobbler, 2 * 365}, GatePass},
		{"script", u{synth.Scripted, 365}, ""}, // review or block
	}
	users := map[string]u{}
	for _, c := range cases {
		users[c.user] = c.u
	}
	s, mock := checkEnv(t, 120*time.Millisecond, users)
	for _, c := range cases {
		// Let the shared bucket refill between accounts, as it would between
		// sign-ups arriving a few seconds apart.
		time.Sleep(3 * time.Second)
		before := mock.Requests.Load()
		start := time.Now()
		r, err := s.Check(context.Background(), c.user, true)
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("%s: %v", c.user, err)
		}
		g := r.Gate
		t.Logf("%-10s %9s scrobbles  %-7s score %3d  %5.2fs  %2d requests  %d/%d periods  %d windows  %s plays sampled  adjusted %s",
			c.user, fmtN(r.User.Playcount), g.Decision, r.Score.Value, elapsed.Seconds(), mock.Requests.Load()-before,
			g.PeriodsRead, g.Periods, g.Windows, fmtN(int64(g.SampledPlay)), fmtN(r.Leaderboard.AdjustedScrobbles))
		for _, sg := range r.Signals {
			t.Logf("      %+3d %s — %s", sg.Points, sg.Title, sg.Detail)
		}
		if elapsed > 10*time.Second {
			t.Errorf("%s: took %v, want under 10s", c.user, elapsed)
		}
		if !g.Complete {
			t.Errorf("%s: incomplete check", c.user)
		}
		switch {
		case c.want != "" && g.Decision != c.want:
			t.Errorf("%s: decision %s, want %s", c.user, g.Decision, c.want)
		case c.want == "" && g.Decision != GateReview && g.Decision != GateBlock:
			t.Errorf("%s: decision %s, want review or block", c.user, g.Decision)
		}
		if n := mock.Requests.Load() - before; n > int64(s.Opts.Check.MaxRequests) {
			t.Errorf("%s: %d requests, budget %d", c.user, n, s.Opts.Check.MaxRequests)
		}
	}

	// A second check within the TTL is served from cache.
	before := mock.Requests.Load()
	if _, err := s.Check(context.Background(), "bigbot", false); err != nil {
		t.Fatal(err)
	}
	if mock.Requests.Load() != before {
		t.Error("cached check went to Last.fm")
	}
}

func TestCheckIgnoresOtherUsersData(t *testing.T) {
	s, mock := checkEnv(t, 0, map[string]struct {
		p    synth.Profile
		days int
	}{"alice": {synth.Honest, 400}})
	mock.WrongUserEvery = 5
	r, err := s.Check(context.Background(), "alice", true)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Gate.Complete || r.Gate.Decision != GatePass {
		t.Fatalf("got %s (complete=%v), want pass despite wrong-user answers", r.Gate.Decision, r.Gate.Complete)
	}
}

// TestCheckBurstOfSignups: many accounts arrive at once. The shared rate
// limit is the hard ceiling; checks queue, the first ones finish quickly and
// later ones read less so the queue keeps moving.
func TestCheckBurstOfSignups(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	type u = struct {
		p    synth.Profile
		days int
	}
	users := map[string]u{}
	names := []string{}
	for i := 0; i < 8; i++ {
		name := string(rune('a'+i)) + "-user"
		p := synth.Honest
		if i%3 == 0 {
			p = synth.Faker
		}
		users[name] = u{p, 2 * 365}
		names = append(names, name)
	}
	s, _ := checkEnv(t, 120*time.Millisecond, users)
	type res struct {
		name     string
		decision string
		elapsed  time.Duration
		reqs     int
	}
	out := make(chan res)
	start := time.Now()
	for _, n := range names {
		go func(n string) {
			r, err := s.Check(context.Background(), n, true)
			if err != nil {
				t.Error(err)
				out <- res{name: n}
				return
			}
			out <- res{n, r.Gate.Decision, time.Since(start), r.Gate.Requests}
		}(n)
	}
	for range names {
		r := <-out
		t.Logf("%-8s %-7s after %5.2fs  %d requests", r.name, r.decision, r.elapsed.Seconds(), r.reqs)
		want := GatePass
		if r.name[0] == 'a' || r.name[0] == 'd' || r.name[0] == 'g' {
			want = GateBlock
		}
		if r.decision != want {
			t.Errorf("%s: %s, want %s", r.name, r.decision, want)
		}
	}
}

// TestCheckSurvivesHungRequests: one request in 12, at random, hangs for 8 s,
// as a few did in the first real 100-account run. Hedged copies must keep
// the check complete and inside the deadline.
func TestCheckSurvivesHungRequests(t *testing.T) {
	s, mock := checkEnv(t, 50*time.Millisecond, map[string]struct {
		p    synth.Profile
		days int
	}{"slowpoke": {synth.Honest, 6 * 365}})
	mock.SlowEvery, mock.SlowFor = 12, 8*time.Second
	start := time.Now()
	r, err := s.Check(context.Background(), "slowpoke", true)
	if err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el > 8*time.Second || !r.Gate.Complete || r.Gate.Decision != GatePass {
		t.Fatalf("took %v, complete=%v, decision=%s, rounds %v, requests %d; want a complete pass well under the deadline", el, r.Gate.Complete, r.Gate.Decision, r.Gate.RoundsMS, r.Gate.Requests)
	}
}

// TestCheckReportsRetries: Last.fm errors that were retried show up in the
// gate by reason, so slow checks can be explained.
func TestCheckReportsRetries(t *testing.T) {
	s, mock := checkEnv(t, 0, map[string]struct {
		p    synth.Profile
		days int
	}{"alice": {synth.Honest, 400}})
	mock.FailEvery = 5 // alternates HTTP 500 and error 29
	r, err := s.Check(context.Background(), "alice", true)
	if err != nil {
		t.Fatal(err)
	}
	if r.Gate.Retries["rate_limited"] == 0 || r.Gate.Retries["http_500"] == 0 || r.Gate.Decision != GatePass {
		t.Errorf("retries %v, decision %s; want both reasons counted and a pass", r.Gate.Retries, r.Gate.Decision)
	}
}

// TestPatientChecksReadFullSamples: a batch that drains the rate budget
// waits instead of reading fewer periods, so every account gets the same
// depth of reading.
func TestPatientChecksReadFullSamples(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the rate budget")
	}
	users := map[string]struct {
		p    synth.Profile
		days int
	}{}
	names := []string{"a", "b", "c", "d"}
	for _, n := range names {
		users[n] = struct {
			p    synth.Profile
			days int
		}{synth.Honest, 4 * 365}
	}
	s, _ := checkEnv(t, 0, users)
	s.Opts.Check.Patient = true
	first := 0
	for _, n := range names {
		r, err := s.Check(context.Background(), n, true)
		if err != nil {
			t.Fatal(err)
		}
		if first == 0 {
			first = r.Gate.Periods
		} else if r.Gate.Periods != first {
			t.Errorf("%s: %d periods, want the same %d as the first check despite the drained budget", n, r.Gate.Periods, first)
		}
	}
}

// TestRecheckReproducesCheck: a saved capture re-scored offline gives the
// same answer as the live check.
func TestRecheckReproducesCheck(t *testing.T) {
	s, _ := checkEnv(t, 0, map[string]struct {
		p    synth.Profile
		days int
	}{"bot": {synth.Faker, 200}, "fine": {synth.TwoDevices, 400}})
	for _, u := range []string{"bot", "fine"} {
		live, err := s.Check(context.Background(), u, true)
		if err != nil {
			t.Fatal(err)
		}
		cp, err := LoadCapture(s.capturePath(u))
		if err != nil {
			t.Fatal(err)
		}
		again, err := s.Recheck(cp)
		if err != nil {
			t.Fatal(err)
		}
		if again.Score != live.Score || again.Gate.Decision != live.Gate.Decision || again.Integrity.PlaysAnalyzed != live.Integrity.PlaysAnalyzed {
			t.Errorf("%s: recheck %+v %s, live %+v %s", u, again.Score, again.Gate.Decision, live.Score, live.Gate.Decision)
		}
	}
}

func fmtN(v int64) string {
	s := ""
	for v >= 1000 {
		s = "," + pad3(v%1000) + s
		v /= 1000
	}
	return itoa(v) + s
}

func pad3(v int64) string {
	return string([]byte{byte('0' + v/100), byte('0' + v/10%10), byte('0' + v%10)})
}
func itoa(v int64) string {
	if v < 10 {
		return string(byte('0' + v))
	}
	return itoa(v/10) + string(byte('0'+v%10))
}
