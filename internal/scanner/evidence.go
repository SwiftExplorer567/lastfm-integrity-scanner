package scanner

import (
	"sort"

	"github.com/swiftexplorer567/lastfm-integrity-scanner/internal/analysis"
	"github.com/swiftexplorer567/lastfm-integrity-scanner/internal/scrobble"
)

// Evidence for the pre-import check: every window read is summarised by
// what its plays were classified as, and the most suspicious windows get an
// activity trace and a play-by-play look at their densest stretch. This is
// what the report draws instead of calendars, which a check that reads
// scattered windows cannot fill.

type sampledWindow struct {
	h       *scrobble.History
	classes []analysis.PlayClass
	s       analysis.Sample
}

const (
	zoomSeconds = 15 * 60 // the stretch shown play by play
	zoomMax     = 120     // plays listed at most
)

func summarize(h *scrobble.History, classes []analysis.PlayClass) analysis.Sample {
	s := analysis.Sample{From: h.First(), To: h.Last(), Plays: len(h.Plays), Counts: map[string]int{}}
	span := float64(max(h.Last()-h.First(), 60))
	s.PerHour = float64(len(h.Plays)) / (span / 3600)
	bad := 0
	for _, c := range classes {
		s.Counts[c.Kind]++
		if c.Rapid {
			s.Rapid++
		}
		if c.Kind == analysis.ClassExcess || c.Rapid {
			bad++
		}
	}
	if len(h.Plays) > 0 {
		s.Suspicion = float64(bad) / float64(len(h.Plays))
	}
	return s
}

// detailSamples returns every window's summary in time order, with the top
// most suspicious ones given activity and zoom detail.
func detailSamples(ws []sampledWindow, top int) []analysis.Sample {
	order := make([]int, len(ws))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		sa, sb := ws[order[a]].s, ws[order[b]].s
		if sa.Suspicion != sb.Suspicion {
			return sa.Suspicion > sb.Suspicion
		}
		return sa.PerHour > sb.PerHour
	})
	for k, i := range order {
		if k == top {
			break
		}
		addDetail(&ws[i])
	}
	out := make([]analysis.Sample, len(ws))
	for i, w := range ws {
		out[i] = w.s
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].From < out[b].From })
	return out
}

func addDetail(w *sampledWindow) {
	plays := w.h.Plays
	if len(plays) == 0 {
		return
	}
	// Activity: at most 96 buckets, at least a minute each.
	span := max(w.h.Last()-w.h.First(), 60)
	bucket := max(int64(60), (span+95)/96)
	bucket = (bucket + 59) / 60 * 60
	n := int(span/bucket) + 1
	w.s.BucketSec = int(bucket)
	w.s.Activity = make([]int, n)
	for _, p := range plays {
		w.s.Activity[int((p.TS-w.h.First())/bucket)]++
	}

	// Densest stretch: the zoomSeconds holding the most plays, impossible
	// ones counting extra so the stretch lands where the evidence is.
	weight := func(i int) int {
		if c := w.classes[i]; c.Kind == analysis.ClassExcess || c.Rapid {
			return 4
		}
		return 1
	}
	bestLo, bestScore, score, lo := 0, -1, 0, 0
	for hi := range plays {
		score += weight(hi)
		for plays[hi].TS-plays[lo].TS >= zoomSeconds {
			score -= weight(lo)
			lo++
		}
		if score > bestScore {
			bestScore, bestLo = score, lo
		}
	}
	from := plays[bestLo].TS
	w.s.ZoomFrom, w.s.ZoomTo = from, from+zoomSeconds
	for i := bestLo; i < len(plays) && plays[i].TS < from+zoomSeconds && len(w.s.Zoom) < zoomMax; i++ {
		t := w.h.Tracks[plays[i].Track]
		c := w.classes[i]
		w.s.Zoom = append(w.s.Zoom, analysis.SamplePlay{
			TS: plays[i].TS, Artist: t.Artist, Title: t.Title,
			Kind: c.Kind, Lane: c.Lane, Occupies: c.Occupies, Rapid: c.Rapid,
		})
	}
}
