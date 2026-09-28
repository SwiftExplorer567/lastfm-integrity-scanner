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

func setup(t *testing.T, failEvery int64, profiles map[string]synth.Profile, days int) (*Scanner, *mockfm.Server, map[string]*synth.Result) {
	t.Helper()
	mock := mockfm.New()
	mock.FailEvery = failEvery
	start := time.Now().AddDate(0, 0, -days-2)
	gens := map[string]*synth.Result{}
	for name, p := range profiles {
		g := synth.Generate(name, synth.Options{Profile: p, Seed: 11, Start: start, Days: days})
		mock.AddUser(&mockfm.User{History: g.History, Registered: start.Unix(), Durations: g.Durations})
		gens[name] = g
	}
	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)
	client := lastfm.New(lastfm.Config{APIKey: "test", BaseURL: srv.URL, RPS: 2000, Burst: 50, MaxRetries: 8, RetryBase: time.Millisecond})
	opts := DefaultOptions()
	opts.AutoFullMaxScrobbles = 50_000
	s, err := New(client, t.TempDir(), opts)
	if err != nil {
		t.Fatal(err)
	}
	return s, mock, gens
}

func TestFullScanThroughFlakyAPI(t *testing.T) {
	s, mock, gens := setup(t, 7, map[string]synth.Profile{"alice": synth.DoubleScrobbler, "bot": synth.Faker}, 60)
	ctx := context.Background()

	r, err := s.Scan(ctx, Request{User: "alice"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := r.Integrity.PlaysAnalyzed, len(gens["alice"].History.Plays); got != want || r.Measurement.Scope != "full" {
		t.Fatalf("analyzed %d plays (%s), want %d (full)", got, r.Measurement.Scope, want)
	}
	if len(r.Measurement.MissingPages) != 0 {
		t.Fatalf("missing pages %v", r.Measurement.MissingPages)
	}
	if r.Score.Verdict != "clean" || r.Leaderboard.Action != "adjust" {
		t.Errorf("alice: %s/%s, want clean/adjust", r.Score.Verdict, r.Leaderboard.Action)
	}

	// Rescan: only the refetch window is downloaded.
	before := mock.Requests.Load()
	r2, err := s.Scan(ctx, Request{User: "alice"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r2.Integrity.PlaysAnalyzed != r.Integrity.PlaysAnalyzed || r2.Measurement.FromCache == 0 {
		t.Errorf("rescan analyzed %d (cache %d), want %d from cache", r2.Integrity.PlaysAnalyzed, r2.Measurement.FromCache, r.Integrity.PlaysAnalyzed)
	}
	if n := mock.Requests.Load() - before; n > int64(r.Measurement.Pages)/2 {
		t.Errorf("rescan made %d requests, first scan read %d pages", n, r.Measurement.Pages)
	}

	// The bot is big enough for auto mode to take a sample.
	rb, err := s.Scan(ctx, Request{User: "bot"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rb.Measurement.Scope != "sample" || rb.Score.Verdict != "suspect" || rb.Leaderboard.Action != "exclude" {
		t.Errorf("bot: scope=%s %s/%s, want sample suspect/exclude", rb.Measurement.Scope, rb.Score.Verdict, rb.Leaderboard.Action)
	}
	if len(rb.Charts.Months) == 0 || rb.Stats.MonthsOverLimit == 0 {
		t.Errorf("bot: months %d over limit %d, want Last.fm monthly totals", len(rb.Charts.Months), rb.Stats.MonthsOverLimit)
	}

	latest, err := s.LatestReport("BOT")
	if err != nil || latest.Score.Value != rb.Score.Value {
		t.Errorf("latest report: %v", err)
	}
}

func TestUnknownUser(t *testing.T) {
	s, _, _ := setup(t, 0, nil, 1)
	_, err := s.Scan(context.Background(), Request{User: "nobody"}, nil)
	if !lastfm.IsNotFound(err) {
		t.Fatalf("got %v, want not found", err)
	}
}
