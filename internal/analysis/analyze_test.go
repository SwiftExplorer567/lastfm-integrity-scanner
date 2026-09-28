package analysis

import (
	"fmt"
	"testing"
	"time"

	"github.com/swiftexplorer567/lastfm-integrity-scanner/internal/scrobble"
	"github.com/swiftexplorer567/lastfm-integrity-scanner/internal/synth"
)

var now = time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)

func run(t *testing.T, prof synth.Profile, withDurations bool) *Report {
	t.Helper()
	g := synth.Generate("u", synth.Options{Profile: prof, Seed: 7, Start: now.AddDate(0, 0, -365), Days: 365})
	var durs map[uint32]int
	if withDurations {
		durs = map[uint32]int{}
		for i, tr := range g.History.Tracks {
			base := tr
			if d, ok := g.Durations[base]; ok {
				durs[uint32(i)] = d
			}
		}
	}
	r := Analyze(Input{
		Profile:   Profile{Name: "u", Playcount: int64(len(g.History.Plays)), Registered: now.AddDate(0, 0, -365).Unix()},
		History:   g.History,
		Scope:     "full",
		Durations: durs,
		Params:    DefaultParams(),
		Now:       now,
	})
	t.Logf("%-16s plays=%d score=%d %s action=%s dup=%d second=%d excess=%d (%.2f%%) loops=%.0f%% bursts=%d",
		prof, r.Integrity.PlaysAnalyzed, r.Score.Value, r.Score.Verdict, r.Leaderboard.Action,
		r.Integrity.Duplicates, r.Integrity.SecondDevicePlays, r.Integrity.ExcessPlays, 100*r.Integrity.ExcessShare,
		100*r.Integrity.LoopShare, r.Integrity.Bursts)
	for _, s := range r.Signals {
		t.Logf("    %+3d %-9s %s — %s", s.Points, s.Kind, s.Title, s.Detail)
	}
	return r
}

func TestHonestListenerIsClean(t *testing.T) {
	for _, d := range []bool{false, true} {
		r := run(t, synth.Honest, d)
		if r.Score.Verdict != "clean" || r.Leaderboard.Action != "keep" {
			t.Errorf("durations=%v: got %s/%s, want clean/keep", d, r.Score.Verdict, r.Leaderboard.Action)
		}
		if r.Integrity.ExcessPlays != 0 {
			t.Errorf("durations=%v: honest single-device listener has %d excess plays", d, r.Integrity.ExcessPlays)
		}
	}
}

func TestSecondDeviceIsForgiven(t *testing.T) {
	r := run(t, synth.TwoDevices, true)
	if r.Score.Verdict != "clean" {
		t.Errorf("got %s (score %d), want clean", r.Score.Verdict, r.Score.Value)
	}
	if r.Integrity.SecondDevicePlays == 0 {
		t.Error("expected plays explained by a second device")
	}
	if r.Integrity.ExcessShare > 0.005 {
		t.Errorf("excess share %.3f, want < 0.5%%", r.Integrity.ExcessShare)
	}
}

func TestDoubleScrobblesAreRemovedNotPunished(t *testing.T) {
	r := run(t, synth.DoubleScrobbler, false)
	if r.Score.Verdict != "clean" {
		t.Errorf("got %s (score %d), want clean", r.Score.Verdict, r.Score.Value)
	}
	if r.Integrity.Duplicates < r.Integrity.PlaysAnalyzed/4 {
		t.Errorf("duplicates %d of %d, want most copies caught", r.Integrity.Duplicates, r.Integrity.PlaysAnalyzed)
	}
	if r.Leaderboard.Action != "adjust" || r.Leaderboard.AdjustedScrobbles >= r.Leaderboard.RawScrobbles {
		t.Errorf("leaderboard %+v, want adjusted down", r.Leaderboard)
	}
	if r.Integrity.ExcessShare > 0.005 {
		t.Errorf("excess share %.3f, want < 0.5%%", r.Integrity.ExcessShare)
	}
}

func TestMismatchedEchoesAreRemovedNotPunished(t *testing.T) {
	r := run(t, synth.EchoScrobbler, false)
	if r.Score.Verdict != "clean" {
		t.Errorf("got %s (score %d), want clean", r.Score.Verdict, r.Score.Value)
	}
	if r.Integrity.EchoDuplicates < r.Integrity.PlaysAnalyzed/4 {
		t.Errorf("echo duplicates %d of %d, want most second copies caught", r.Integrity.EchoDuplicates, r.Integrity.PlaysAnalyzed)
	}
	if r.Integrity.ExcessShare > 0.005 || r.Integrity.UnderFifteenShare > 0.02 {
		t.Errorf("excess %.3f, under-15 %.3f; echoes should not count as either", r.Integrity.ExcessShare, r.Integrity.UnderFifteenShare)
	}
}

func TestFakerIsSuspect(t *testing.T) {
	r := run(t, synth.Faker, false)
	if r.Score.Verdict != "suspect" || r.Leaderboard.Action != "exclude" {
		t.Errorf("got %s/%s (score %d), want suspect/exclude", r.Score.Verdict, r.Leaderboard.Action, r.Score.Value)
	}
	if r.Integrity.ExcessShare < 0.3 {
		t.Errorf("excess share %.2f, want most bot plays flagged", r.Integrity.ExcessShare)
	}
}

func TestScriptedIsCaught(t *testing.T) {
	// Without song lengths only the fixed interval and the hourly rate give
	// it away; with them most scripted plays are also impossible.
	r := run(t, synth.Scripted, false)
	if r.Score.Verdict == "clean" {
		t.Errorf("without durations: got clean (score %d), want review or suspect", r.Score.Value)
	}
	r = run(t, synth.Scripted, true)
	if r.Score.Verdict != "suspect" {
		t.Errorf("with durations: got %s (score %d), want suspect", r.Score.Verdict, r.Score.Value)
	}
}

func TestSmallCases(t *testing.T) {
	h := scrobble.New("x")
	a := scrobble.Track{Artist: "BTS", Title: "Body to Body"}
	b := scrobble.Track{Artist: "BTS", Title: "SWIM"}
	c := scrobble.Track{Artist: "BTS", Title: "Like Animals"}
	t0 := int64(1_700_000_000)
	// A double scrobble with different metadata, 5 s apart.
	h.Add(t0, a)
	h.Add(t0+5, scrobble.Track{Artist: "BTS", Title: "Body to Body (Official MV)"})
	// Three different songs at one second: one fits on a second device, one
	// is excess.
	h.Add(t0+600, a)
	h.Add(t0+600, b)
	h.Add(t0+600, c)
	h.Sort()
	r := Analyze(Input{Profile: Profile{Name: "x"}, History: h, Scope: "full", Params: DefaultParams(), Now: now})
	ig := r.Integrity
	if ig.Duplicates != 1 || ig.SecondDevicePlays != 1 || ig.ExcessPlays != 1 {
		t.Errorf("dup=%d second=%d excess=%d, want 1/1/1", ig.Duplicates, ig.SecondDevicePlays, ig.ExcessPlays)
	}
	if ig.SameSecondInstants != 1 || ig.SameSecondTriples != 1 || ig.SameSecondPlays != 3 {
		t.Errorf("same second: %+v", ig)
	}
}

func TestSongKey(t *testing.T) {
	same := [][2][2]string{
		{{"BTS", "Body to Body"}, {"BTS", "Body To Body (Official MV)"}},
		{{"The Beatles", "Let It Be - Remastered 2009"}, {"the beatles", "Let It Be"}},
		{{"Drake feat. Rihanna", "Too Good"}, {"Drake", "Too Good (feat. Rihanna)"}},
		{{"Sezen Aksu", "Gülümse"}, {"SEZEN AKSU", "Gülümse"}},
	}
	for _, c := range same {
		if SongKey(c[0][0], c[0][1]) != SongKey(c[1][0], c[1][1]) {
			t.Errorf("%v and %v should match", c[0], c[1])
		}
	}
	if SongKey("BTS", "SWIM") == SongKey("BTS", "Body to Body") {
		t.Error("different songs matched")
	}
}

// analyzePlays builds a history of plays spaced by gaps, each (artist, title, album).
type play struct {
	gap                  int64
	artist, title, album string
}

func analyzePlays(t *testing.T, ps []play) *Report {
	t.Helper()
	h := scrobble.New("u")
	ts := now.AddDate(0, -1, 0).Unix()
	for _, p := range ps {
		ts += p.gap
		h.Add(ts, scrobble.Track{Artist: p.artist, Title: p.title, Album: p.album})
	}
	h.Sort()
	return Analyze(Input{Profile: Profile{Name: "u", Playcount: int64(len(ps))}, History: h, Scope: "quick", Params: DefaultParams(), Now: now})
}

func hasSignal(r *Report, id string) bool {
	for _, s := range r.Signals {
		if s.ID == id && s.Points > 0 {
			return true
		}
	}
	return false
}

// Scrobblers from 2009–2016 recorded one listen 5–30 times a second apart;
// the next song follows minutes later. That is one play, not a loop.
func TestStutterIsOnePlay(t *testing.T) {
	// Honest accounts in the calibration set carry up to 8.6% such copies.
	var ps []play
	copies := 0
	for i := 0; i < 1000; i++ {
		ps = append(ps, play{180 + int64(i*37%120), "A", fmt.Sprint("song ", i), "X"})
		if i%100 == 0 {
			for k := 0; k < 3+i%12; k++ {
				ps = append(ps, play{1, "A", fmt.Sprint("song ", i), "X"})
				copies++
			}
		}
	}
	r := analyzePlays(t, ps)
	ig := r.Integrity
	if ig.RapidLoopPlays != 0 || ig.ExcessPlays != 0 || ig.Bursts != 0 {
		t.Errorf("stutter scored as a loop: rapid %d, excess %d, bursts %d", ig.RapidLoopPlays, ig.ExcessPlays, ig.Bursts)
	}
	if ig.StutterCopies != copies || ig.Duplicates != copies {
		t.Errorf("stutter copies %d, duplicates %d, want %d", ig.StutterCopies, ig.Duplicates, copies)
	}
	if r.Score.Value != 0 {
		t.Errorf("score %d, want 0: %+v", r.Score.Value, r.Signals)
	}
	if want := float64(copies) / float64(len(ps)); r.Leaderboard.RemovedShare < want {
		t.Errorf("copies must still come off the leaderboard count; removed %.3f, want %.3f", r.Leaderboard.RemovedShare, want)
	}

	// A fifth of the history recorded twice is worth a look.
	for i := range ps {
		if i%3 == 0 {
			ps = append(ps, play{1, ps[len(ps)-1].artist, ps[len(ps)-1].title, "X"})
		}
	}
	if r := analyzePlays(t, ps); !hasSignal(r, "stutter") {
		t.Errorf("heavy stutter: no signal (%d copies of %d)", r.Integrity.StutterCopies, len(ps))
	}
}

// Different artists seconds apart: only scrobbling software does that.
func TestMixedSkipsAreFake(t *testing.T) {
	var ps []play
	for i := 0; i < 400; i++ {
		gap := int64(200)
		if i%10 != 0 {
			gap = 3
		}
		ps = append(ps, play{gap, fmt.Sprint("artist ", i), "t", fmt.Sprint("album ", i)})
	}
	r := analyzePlays(t, ps)
	if !hasSignal(r, "mixed_skips") {
		t.Errorf("no mixed_skips signal: %+v", r.Signals)
	}
}

// One album in order, seconds apart, is a manual album scrobble: a weak
// signal, never on its own a reason to hold the account.
func TestAlbumSkipsAreWeak(t *testing.T) {
	// djryan (labelled unsure) has 5.4% of plays like this.
	var ps []play
	for i := 0; i < 1000; i++ {
		ps = append(ps, play{180 + int64(i*37%120), fmt.Sprint("artist ", i%40), fmt.Sprint("song ", i), "x"})
		if i%200 == 0 {
			for k := 0; k < 10; k++ {
				ps = append(ps, play{2, "Band", fmt.Sprint("album ", i, " track ", k), fmt.Sprint("album ", i)})
			}
		}
	}
	r := analyzePlays(t, ps)
	if hasSignal(r, "mixed_skips") || r.Score.Verdict != "clean" {
		t.Errorf("album scrobbles: score %d %s, signals %+v", r.Score.Value, r.Score.Verdict, r.Signals)
	}
	if !hasSignal(r, "album_skips") {
		t.Errorf("no album_skips signal: %+v", r.Signals)
	}
}

// A short playlist cycled every few seconds for minutes on end.
func TestFastLoopsAreFake(t *testing.T) {
	var ps []play
	for i := 0; i < 600; i++ {
		gap := int64(8)
		if i%50 == 0 {
			gap = 3600
		}
		ps = append(ps, play{gap, "Band", fmt.Sprint("song ", i%12), "album"})
	}
	r := analyzePlays(t, ps)
	if !hasSignal(r, "fast_loops") || r.Score.Verdict == "clean" {
		t.Errorf("fast loops: score %d %s, signals %+v", r.Score.Value, r.Score.Verdict, r.Signals)
	}
}
