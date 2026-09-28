package render

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/swiftexplorer567/lastfm-integrity-scanner/internal/analysis"
)

// Evidence charts for the pre-import check. They show what was actually
// read (windows of consecutive plays) and what the analysis made of each
// play, rather than calendars a sampled check cannot fill.

// Colors per play class, shared by every chart and legend.
var classStyle = []struct {
	Kind, Color, Label, Help string
}{
	{analysis.ClassNormal, "#475569", "One player", "fits on a single player"},
	{analysis.ClassSecondPlayer, "#38bdf8", "Second player", "overlaps and fits only with a second player running (forgiven)"},
	{analysis.ClassDuplicate, "#a78bfa", "Duplicate", "a copy of a play seconds earlier (removed, forgiven)"},
	{analysis.ClassEcho, "#c4b5fd", "Echo", "the same play recorded under other metadata (removed, forgiven)"},
	{analysis.ClassExcess, "#f87171", "Impossible", "needed a third player at the same moment"},
}

func classColor(kind string) string {
	for _, c := range classStyle {
		if c.Kind == kind {
			return c.Color
		}
	}
	return "#475569"
}

func classLabel(kind string) string {
	for _, c := range classStyle {
		if c.Kind == kind {
			return c.Label
		}
	}
	return kind
}

const colRapid = "#fbbf24"

// SamplesOverviewSVG draws one row per window read: when it was, how its
// plays split across classes, and how fast they came.
func SamplesOverviewSVG(g *analysis.Gate) string {
	ss := g.Samples
	const W, left, right, top = 1000.0, 150.0, 120.0, 22.0
	rowH := 16.0
	if len(ss) > 24 {
		rowH = 12
	}
	H := top + float64(len(ss))*rowH + 6
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %.0f %.0f" width="100%%" role="img" aria-label="Every sampled window" font-family="Inter,system-ui,sans-serif">`, W, H)
	barW := W - left - right
	fmt.Fprintf(&b, `<text x="%.0f" y="13" font-size="11" fill="%s">window (UTC)</text>`, 0.0, colMuted)
	fmt.Fprintf(&b, `<text x="%.0f" y="13" font-size="11" fill="%s">share of its plays</text>`, left, colMuted)
	fmt.Fprintf(&b, `<text x="%.0f" y="13" font-size="11" fill="%s" text-anchor="end">plays / hour</text>`, W-2, colMuted)
	for _, f := range []float64{0.25, 0.5, 0.75} {
		x := left + f*barW
		fmt.Fprintf(&b, `<line x1="%.1f" x2="%.1f" y1="%.1f" y2="%.1f" stroke="%s" stroke-dasharray="2 3"/>`, x, x, top-4, H-4, colGrid)
	}
	for i, s := range ss {
		y := top + float64(i)*rowH
		fmt.Fprintf(&b, `<text x="0" y="%.1f" font-size="11" fill="%s" font-family="ui-monospace,Menlo,monospace">%s</text>`,
			y+11, colMuted, time.Unix(s.From, 0).UTC().Format("2006-01-02 15:04"))
		x := left
		for _, c := range classStyle {
			n := s.Counts[c.Kind]
			if n == 0 || s.Plays == 0 {
				continue
			}
			w := float64(n) / float64(s.Plays) * barW
			fmt.Fprintf(&b, `<rect x="%.2f" y="%.1f" width="%.2f" height="%.1f" fill="%s"><title>%s: %s of %s plays (%s)</title></rect>`,
				x, y+2, w, rowH-5, c.Color, c.Label, analysis.Num(n), analysis.Num(s.Plays), analysis.Pct(float64(n)/float64(s.Plays)))
			x += w
		}
		if s.Rapid > 0 {
			w := float64(s.Rapid) / float64(max(s.Plays, 1)) * barW
			fmt.Fprintf(&b, `<rect x="%.2f" y="%.1f" width="%.2f" height="2" fill="%s"><title>%s plays repeat a song seconds apart</title></rect>`,
				left, y+rowH-3, w, colRapid, analysis.Num(s.Rapid))
		}
		col := colMuted
		if s.PerHour > 60 {
			col = colLimit
		}
		fmt.Fprintf(&b, `<text x="%.0f" y="%.1f" font-size="11" fill="%s" text-anchor="end" font-family="ui-monospace,Menlo,monospace">%s</text>`,
			W-2, y+11, col, analysis.Num(int64(math.Round(s.PerHour))))
	}
	b.WriteString(`</svg>`)
	return b.String()
}

// LaneSVG lays the plays of a zoomed stretch onto players: each play is a
// block as long as the time it was credited with. Plays that found no free
// player sit on their own row; that row is the evidence.
func LaneSVG(s analysis.Sample) string {
	const W, left, top, rowH = 1000.0, 118.0, 18.0, 26.0
	rows := []string{"Player 1", "Player 2", "No player free", "Duplicates"}
	H := top + float64(len(rows))*rowH + 22
	span := float64(max(s.ZoomTo-s.ZoomFrom, 60))
	plotW := W - left - 8
	xOf := func(ts int64) float64 { return left + float64(ts-s.ZoomFrom)/span*plotW }
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %.0f %.0f" width="100%%" role="img" aria-label="Plays laid onto players" font-family="Inter,system-ui,sans-serif">`, W, H)
	for i, name := range rows {
		y := top + float64(i)*rowH
		fill := "#10161d"
		if i == 2 {
			fill = "#2a1216"
		}
		fmt.Fprintf(&b, `<rect x="%.0f" y="%.1f" width="%.1f" height="%.1f" fill="%s"/>`, left, y, plotW, rowH-4, fill)
		col := colMuted
		if i == 2 {
			col = colLimit
		}
		fmt.Fprintf(&b, `<text x="0" y="%.1f" font-size="11.5" fill="%s">%s</text>`, y+15, col, name)
	}
	// Minute ticks.
	for m := int64(0); m*60 <= int64(span); m += 1 {
		x := left + float64(m*60)/span*plotW
		if m%5 == 0 {
			// Keep the labels at the edges inside the chart.
			anchor := "middle"
			if x+16 > W {
				anchor = "end"
			}
			fmt.Fprintf(&b, `<text x="%.1f" y="12" font-size="10" fill="%s" text-anchor="%s">%s</text>`, x, colMuted, anchor,
				time.Unix(s.ZoomFrom+m*60, 0).UTC().Format("15:04"))
		}
		fmt.Fprintf(&b, `<line x1="%.1f" x2="%.1f" y1="%.1f" y2="%.1f" stroke="%s" stroke-opacity="0.5"/>`, x, x, top, H-22, colGrid)
	}
	// Stack the no-lane and duplicate rows so overlapping blocks stay visible.
	stackEnd := map[int][]float64{}
	for _, p := range s.Zoom {
		row := p.Lane
		switch {
		case p.Kind == analysis.ClassExcess:
			row = 2
		case p.Kind == analysis.ClassDuplicate || p.Kind == analysis.ClassEcho:
			row = 3
		case row < 0:
			row = 0
		}
		x := xOf(p.TS)
		w := math.Max(float64(max(p.Occupies, 8))/span*plotW, 2)
		if row >= 2 {
			w = math.Max(plotW/span*10, 2.5)
		}
		// Sub-row inside the band for overlaps.
		sub := 0
		ends := stackEnd[row]
		for sub < len(ends) && ends[sub] > x {
			sub++
		}
		if sub == len(ends) {
			ends = append(ends, 0)
		}
		ends[sub] = x + w
		stackEnd[row] = ends
		levels := 4.0
		hh := (rowH - 6) / levels
		yy := top + float64(row)*rowH + 1 + float64(sub%4)*hh
		if row < 2 {
			hh, yy = rowH-6, top+float64(row)*rowH+1
		}
		stroke := ""
		if p.Rapid {
			stroke = fmt.Sprintf(` stroke="%s" stroke-width="1.2"`, colRapid)
		}
		fmt.Fprintf(&b, `<rect x="%.2f" y="%.2f" width="%.2f" height="%.2f" rx="1.5" fill="%s" fill-opacity="0.9"%s><title>%s  %s — %s (%s)</title></rect>`,
			x, yy, w, hh, classColor(p.Kind), stroke, time.Unix(p.TS, 0).UTC().Format("15:04:05"), esc(p.Artist), esc(p.Title), classLabel(p.Kind))
	}
	fmt.Fprintf(&b, `<text x="%.0f" y="%.1f" font-size="10.5" fill="%s">Each block starts when the play started and lasts the least time Last.fm needs before it accepts the scrobble.</text>`, left, H-6, colMuted)
	b.WriteString(`</svg>`)
	return b.String()
}

// ActivitySVG draws plays per bucket across a window, with the rate of one
// song a minute dashed across.
func ActivitySVG(s analysis.Sample, hourLimit int) string {
	const W, H, left, bottom = 1000.0, 90.0, 4.0, 16.0
	if len(s.Activity) == 0 {
		return ""
	}
	maxV := 1
	for _, v := range s.Activity {
		maxV = max(maxV, v)
	}
	limit := float64(hourLimit) * float64(s.BucketSec) / 3600
	top := math.Max(float64(maxV), limit*1.3)
	plotH := H - bottom - 4
	bw := (W - left) / float64(len(s.Activity))
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %.0f %.0f" width="100%%" role="img" aria-label="Plays across the window" font-family="Inter,system-ui,sans-serif">`, W, H)
	for i, v := range s.Activity {
		h := float64(v) / top * plotH
		col := "#64748b"
		if float64(v) > limit {
			col = colLimit
		}
		start := s.From + int64(i*s.BucketSec)
		fmt.Fprintf(&b, `<rect x="%.2f" y="%.2f" width="%.2f" height="%.2f" fill="%s"><title>%s: %d plays in %d min</title></rect>`,
			left+float64(i)*bw+0.5, 4+plotH-h, math.Max(bw-1, 0.8), h, col, time.Unix(start, 0).UTC().Format("Jan 2 15:04"), v, s.BucketSec/60)
	}
	y := 4 + plotH - limit/top*plotH
	fmt.Fprintf(&b, `<line x1="%.0f" x2="%.0f" y1="%.1f" y2="%.1f" stroke="%s" stroke-dasharray="4 4"/>`, left, W, y, y, colLimit)
	fmt.Fprintf(&b, `<text x="%.0f" y="%.1f" font-size="10.5" fill="%s" text-anchor="end">%d plays an hour</text>`, W-2, y-3, colLimit, hourLimit)
	fmt.Fprintf(&b, `<text x="%.0f" y="%.0f" font-size="10.5" fill="%s">%s</text>`, left, H-3, colMuted, time.Unix(s.From, 0).UTC().Format("2006-01-02 15:04"))
	fmt.Fprintf(&b, `<text x="%.0f" y="%.0f" font-size="10.5" fill="%s" text-anchor="end">%s UTC</text>`, W-2, H-3, colMuted, time.Unix(s.To, 0).UTC().Format("2006-01-02 15:04"))
	b.WriteString(`</svg>`)
	return b.String()
}

// TimeBudgetSVG compares the hours the account has existed with the least
// hours its top songs alone need.
func TimeBudgetSVG(g *analysis.Gate) string {
	if g.AccountHours <= 0 {
		return ""
	}
	const W, H, left = 520.0, 70.0, 190.0
	top := math.Max(g.AccountHours, g.TopTracksMinHrs)
	plotW := W - left - 80
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %.0f %.0f" width="100%%" role="img" aria-label="Listening time budget" font-family="Inter,system-ui,sans-serif">`, W, H)
	rows := []struct {
		label string
		v     float64
		col   string
	}{
		{"Hours since sign-up", g.AccountHours, "#475569"},
		{"Least hours the top songs need", g.TopTracksMinHrs, "#38bdf8"},
	}
	if g.TopTracksMinHrs > g.AccountHours*0.25 {
		rows[1].col = colLimit
	}
	for i, r := range rows {
		y := 8 + float64(i)*30
		fmt.Fprintf(&b, `<text x="0" y="%.1f" font-size="12" fill="%s">%s</text>`, y+14, colMuted, r.label)
		w := math.Max(r.v/top*plotW, 1)
		fmt.Fprintf(&b, `<rect x="%.0f" y="%.1f" width="%.1f" height="18" rx="3" fill="%s"/>`, left, y, w, r.col)
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" font-size="12" fill="#e6edf3" font-family="ui-monospace,Menlo,monospace">%s h</text>`, left+w+8, y+14, analysis.Num(int64(r.v)))
	}
	b.WriteString(`</svg>`)
	return b.String()
}
