// Package synth generates realistic listening histories for tests, the demo
// command and the mock Last.fm server: honest listeners, honest listeners with
// messy setups, and accounts that fake plays.
package synth

import (
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/swiftexplorer567/lastfm-integrity-scanner/internal/scrobble"
)

// Profile names a kind of listener.
type Profile string

const (
	// Honest: one device, sleeps at night, songs play to the end or get
	// skipped now and then.
	Honest Profile = "honest"
	// TwoDevices: honest, but most evenings a second player (a YouTube tab on
	// the PC) runs while the phone plays Spotify, and some nights the tab is
	// left on autoplay until morning.
	TwoDevices Profile = "two-devices"
	// DoubleScrobbler: honest, but two scrobbler apps record most plays, so
	// many plays appear twice a few seconds apart with slightly different
	// metadata.
	DoubleScrobbler Profile = "double-scrobbler"
	// Faker: honest-looking history padded by a bot that loops a handful of
	// songs seconds apart for hours every day.
	Faker Profile = "faker"
	// Scripted: honest history plus a script that submits varied songs at a
	// fixed 31-second interval for a couple of hours a day, just above the
	// 30-second floor so no two plays overlap without song lengths.
	Scripted Profile = "scripted"
)

var Profiles = []Profile{Honest, TwoDevices, DoubleScrobbler, Scripted, Faker}

// Song is a catalogue entry with its length.
type Song struct {
	scrobble.Track
	Seconds int
}

// Catalogue returns n songs by m artists with lengths between 2 and 6 minutes.
func Catalogue(rng *rand.Rand, n, m int) []Song {
	out := make([]Song, n)
	for i := range out {
		a := rng.IntN(m)
		out[i] = Song{
			Track: scrobble.Track{
				Artist: fmt.Sprintf("Artist %d", a+1),
				Title:  fmt.Sprintf("Song %d", i+1),
				Album:  fmt.Sprintf("Album %d-%d", a+1, i%7),
			},
			Seconds: 120 + rng.IntN(240),
		}
	}
	return out
}

// Options shape a generated history.
type Options struct {
	Profile Profile
	Seed    uint64
	Start   time.Time
	Days    int
}

// Result is a generated history plus the song lengths behind it.
type Result struct {
	History   *scrobble.History
	Durations map[scrobble.Track]int
	Songs     []Song
}

// Generate builds a history for user.
func Generate(user string, o Options) *Result {
	rng := rand.New(rand.NewPCG(o.Seed, o.Seed^0x9e3779b97f4a7c15))
	songs := Catalogue(rng, 1500, 180)
	h := scrobble.New(user)
	res := &Result{History: h, Durations: map[scrobble.Track]int{}, Songs: songs}
	for _, s := range songs {
		res.Durations[s.Track] = s.Seconds
	}
	day0 := o.Start.UTC().Truncate(24 * time.Hour)

	// A listener's taste: a few hundred songs, a few favourites.
	fav := make([]Song, 300)
	for i := range fav {
		fav[i] = songs[rng.IntN(len(songs))]
	}
	pick := func() Song {
		if rng.Float64() < 0.3 {
			return fav[rng.IntN(20)]
		}
		return fav[rng.IntN(len(fav))]
	}

	// session plays songs back to back on one player from t for d seconds.
	session := func(t, d int64, skipRate float64, rec func(ts int64, s Song)) int64 {
		end := t + d
		for t < end {
			s := pick()
			rec(t, s)
			listen := int64(s.Seconds)
			if rng.Float64() < skipRate {
				// Skipped after the scrobble point.
				listen = int64(s.Seconds/2 + rng.IntN(20))
			}
			t += listen + int64(rng.IntN(4))
		}
		return t
	}
	plain := func(ts int64, s Song) { h.Add(ts, s.Track) }

	for d := 0; d < o.Days; d++ {
		base := day0.Add(time.Duration(d) * 24 * time.Hour).Unix()
		// Wakes around 08:00 UTC, sleeps around midnight. Two to four sessions,
		// one after another on one player.
		cursor := base + int64(8*3600+rng.IntN(2*3600))
		for k := 0; k < 2+rng.IntN(3); k++ {
			start := cursor
			dur := int64(20*60 + rng.IntN(150*60))
			rec := plain
			if o.Profile == DoubleScrobbler && rng.Float64() < 0.7 {
				rec = func(ts int64, s Song) {
					h.Add(ts, s.Track)
					alt := s.Track
					alt.Title += " - Remastered 2011"
					alt.Album = ""
					h.Add(ts+int64(rng.IntN(20)), alt)
				}
			}
			cursor = session(start, dur, 0.15, rec) + int64(30*60+rng.IntN(150*60))
		}
		switch o.Profile {
		case TwoDevices:
			if rng.Float64() < 0.6 {
				// Evening YouTube tab on the PC alongside the phone.
				start := base + int64(18*3600+rng.IntN(3*3600))
				session(start, int64(60*60+rng.IntN(120*60)), 0.05, plain)
			}
			if rng.Float64() < 0.08 {
				// Forgotten tab on autoplay all night.
				start := base + int64(22*3600+rng.IntN(2*3600))
				session(start, int64(8*3600+rng.IntN(3*3600)), 0.0, plain)
			}
		case Scripted:
			start := base + int64(rng.IntN(6*3600))
			for t, end := start, start+int64(3600+rng.IntN(2*3600)); t < end; t += 31 {
				h.Add(t, songs[rng.IntN(len(songs))].Track)
			}
		case Faker:
			// A bot loops a few songs 2–8 s apart for hours, every day.
			loop := songs[:6]
			start := base + int64(rng.IntN(6*3600))
			end := start + int64(3*3600+rng.IntN(5*3600))
			for t := start; t < end; {
				s := loop[rng.IntN(len(loop))]
				h.Add(t, s.Track)
				if rng.Float64() < 0.05 {
					h.Add(t, loop[rng.IntN(len(loop))].Track) // same-second pair
				}
				t += int64(2 + rng.IntN(7))
				if rng.Float64() < 0.01 {
					t += int64(60 + rng.IntN(600)) // pause
				}
			}
		}
	}
	h.Sort()
	return res
}
