package render

import (
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/go-pdf/fpdf"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/gomono"
	"golang.org/x/image/font/gofont/gomonobold"
	"golang.org/x/image/font/gofont/goregular"

	"github.com/swiftexplorer567/lastfm-integrity-scanner/internal/analysis"
)

// renderPDF writes a printable A4 report (light theme). Fonts are embedded
// Go fonts, which cover Latin (including Turkish), Greek and Cyrillic; songs
// in other scripts show as boxes, the HTML and JSON keep them intact.
func renderPDF(w io.Writer, r *analysis.Report) error {
	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.SetMargins(14, 14, 14)
	pdf.SetAutoPageBreak(true, 16)
	pdf.AddUTF8FontFromBytes("go", "", goregular.TTF)
	pdf.AddUTF8FontFromBytes("go", "B", gobold.TTF)
	pdf.AddUTF8FontFromBytes("mono", "", gomono.TTF)
	pdf.AddUTF8FontFromBytes("mono", "B", gomonobold.TTF)
	pdf.SetTitle("Integrity report: "+r.User.Name, true)
	pdf.SetCreator("lastfm-integrity-scanner", true)
	pdf.SetFooterFunc(func() {
		pdf.SetY(-11)
		pdf.SetFont("go", "", 7.5)
		pdf.SetTextColor(120, 128, 140)
		pdf.CellFormat(0, 4, fmt.Sprintf("Last.fm integrity report · %s · generated %s · page %d", r.User.Name, r.GeneratedAt.Format("2006-01-02 15:04 UTC"), pdf.PageNo()), "", 0, "C", false, 0, "")
	})
	pdf.AddPage()
	const pageW = 182.0 // 210 - margins

	rgb := func(hex string) (int, int, int) {
		v, _ := strconv.ParseUint(strings.TrimPrefix(hex, "#"), 16, 32)
		return int(v >> 16 & 0xff), int(v >> 8 & 0xff), int(v & 0xff)
	}
	fill := func(hex string) { pdf.SetFillColor(rgb(hex)) }
	ink := func(hex string) { pdf.SetTextColor(rgb(hex)) }
	const dark, muted, rule = "#111827", "#6b7280", "#e5e7eb"
	heading := func(s string) {
		if pdf.GetY() > 250 {
			pdf.AddPage()
		}
		pdf.Ln(4)
		pdf.SetFont("go", "B", 11)
		ink(dark)
		pdf.CellFormat(0, 6, s, "", 1, "L", false, 0, "")
		pdf.SetDrawColor(rgb(rule))
		pdf.Line(14, pdf.GetY(), 14+pageW, pdf.GetY())
		pdf.Ln(2)
	}

	// Header.
	pdf.SetFont("go", "B", 8)
	ink(muted)
	pdf.CellFormat(0, 4, "LAST.FM INTEGRITY REPORT", "", 1, "L", false, 0, "")
	pdf.SetFont("go", "B", 20)
	ink(dark)
	pdf.CellFormat(120, 10, r.User.Name, "", 1, "L", false, 0, "")
	pdf.SetFont("go", "", 9)
	ink(muted)
	sub := analysis.Num(r.User.Playcount) + " scrobbles"
	if r.User.Registered > 0 {
		sub += " · joined " + analysis.Date(r.User.Registered)
	}
	if r.User.Country != "" {
		sub += " · " + r.User.Country
	}
	pdf.CellFormat(120, 5, sub, "", 1, "L", false, 0, "")

	// Score box, top right.
	vc := map[string]string{"clean": "#16a34a", "review": "#d97706", "suspect": "#dc2626"}[r.Score.Verdict]
	fill("#f9fafb")
	pdf.SetDrawColor(rgb(rule))
	pdf.RoundedRect(136, 14, 60, 30, 3, "1234", "FD")
	pdf.SetXY(138, 17)
	pdf.SetFont("mono", "B", 26)
	ink(vc)
	pdf.CellFormat(22, 14, strconv.Itoa(r.Score.Value), "", 0, "C", false, 0, "")
	pdf.SetXY(161, 18)
	pdf.SetFont("go", "B", 12)
	pdf.CellFormat(34, 6, r.Score.Label, "", 2, "L", false, 0, "")
	pdf.SetFont("go", "", 8)
	ink(muted)
	pdf.CellFormat(34, 4, "score out of 100", "", 2, "L", false, 0, "")
	pdf.SetXY(138, 34)
	pdf.SetFont("go", "B", 8.5)
	ink(dark)
	pdf.CellFormat(56, 6, "Leaderboard: "+strings.ToUpper(r.Leaderboard.Action), "", 0, "C", false, 0, "")
	pdf.SetY(48)

	// Pre-import check.
	if g := r.Gate; g != nil {
		heading("Pre-import check: " + strings.ToUpper(g.Decision))
		pdf.SetFont("go", "", 9)
		ink(dark)
		pdf.MultiCell(0, 4.6, g.Reason, "", "L", false)
		pdf.Ln(1)
		complete := ""
		if !g.Complete {
			complete = " (partial)"
		}
		kv(pdf, [][2]string{
			{"Time and requests", fmt.Sprintf("%.1f s, %d requests", float64(g.ElapsedMS)/1000, g.Requests)},
			{"History periods read", fmt.Sprintf("%d / %d%s", g.PeriodsRead, g.Periods, complete)},
			{"Plays sampled", fmt.Sprintf("%s in %d windows", analysis.Num(g.SampledPlay), g.Windows)},
			{"Distinct songs", analysis.Num(g.DistinctTracks)},
			{"Top song's share of all plays", analysis.Pct(g.TopTrackShare)},
			{"Least hours the top songs need / hours since sign-up", fmt.Sprintf("%s / %s", analysis.Num(int64(g.TopTracksMinHrs)), analysis.Num(int64(g.AccountHours)))},
		})
		if len(g.TopTracks) > 0 {
			pdf.Ln(2)
			table(pdf, []string{"Lifetime top songs", "Plays", "A day", "Least hours"}, []float64{110, 24, 22, 26}, func(add func(...string)) {
				for _, t := range g.TopTracks {
					add(t.Artist+" — "+t.Title, analysis.Num(t.Plays), fmt.Sprintf("%.1f", t.PerDay), analysis.Num(int64(t.MinHours)))
				}
			})
		}
		if len(g.Hotspots) > 0 {
			pdf.Ln(2)
			table(pdf, []string{"Densest stretch from", "Days", "Plays", "A day"}, []float64{80, 30, 36, 36}, func(add func(...string)) {
				for _, h := range g.Hotspots {
					add(h.Month, strconv.Itoa(h.Days), analysis.Num(h.Plays), analysis.Num(int64(math.Round(h.PerDay))))
				}
			})
		}
		if len(g.Evidence) > 0 {
			pdf.Ln(2)
			table(pdf, []string{"Densest sampled window (UTC)", "Plays", "Span (s)", "A hour", "3rd player"}, []float64{70, 26, 28, 28, 30}, func(add func(...string)) {
				for _, w := range g.Evidence {
					add(fmtTime(w.From), analysis.Num(w.Plays), analysis.Num(w.SpanSec), analysis.Num(int64(math.Round(w.PerHour))), analysis.Num(w.Excess))
				}
			})
		}
	}

	// Leaderboard.
	heading("Leaderboard recommendation")
	adj := analysis.Num(r.Leaderboard.AdjustedScrobbles)
	if r.Leaderboard.Estimated {
		adj += " (estimated from the sample)"
	}
	kv(pdf, [][2]string{
		{"Action", strings.ToUpper(r.Leaderboard.Action)},
		{"Last.fm count", analysis.Num(r.Leaderboard.RawScrobbles)},
		{"Adjusted count", adj},
		{"Removed (double scrobbles + impossible plays)", analysis.Pct(r.Leaderboard.RemovedShare)},
	})
	pdf.SetFont("go", "", 8.5)
	ink(muted)
	pdf.MultiCell(0, 4.2, r.Leaderboard.Reason, "", "L", false)

	// Signals.
	heading("Signals")
	if len(r.Signals) == 0 {
		pdf.SetFont("go", "", 9)
		ink(muted)
		pdf.CellFormat(0, 5, "Nothing unusual.", "", 1, "L", false, 0, "")
	}
	kindCol := map[string]string{"fake": "#dc2626", "volume": "#d97706", "pattern": "#7c3aed", "benign": "#16a34a"}
	for _, s := range r.Signals {
		if pdf.GetY() > 265 {
			pdf.AddPage()
		}
		y := pdf.GetY()
		pts := "ok"
		if s.Points > 0 {
			pts = "+" + strconv.Itoa(s.Points)
		}
		pdf.SetFont("mono", "B", 9)
		ink(kindCol[s.Kind])
		pdf.SetXY(14, y)
		pdf.CellFormat(12, 5, pts, "", 0, "L", false, 0, "")
		pdf.SetFont("go", "B", 9.5)
		ink(dark)
		pdf.CellFormat(0, 5, s.Title, "", 1, "L", false, 0, "")
		pdf.SetX(26)
		pdf.SetFont("go", "", 8.5)
		ink(muted)
		pdf.MultiCell(pageW-12, 4.2, s.Detail, "", "L", false)
		pdf.Ln(1.5)
	}

	// Key numbers.
	heading("Key numbers")
	st, ig := r.Stats, r.Integrity
	kv(pdf, [][2]string{
		{"Average a day, since the account opened", analysis.Num(int64(math.Round(st.AvgPerDay)))},
		{"Busiest day", fmt.Sprintf("%s plays on %s", analysis.Num(st.BusiestDay.Plays), st.BusiestDay.Date)},
		{"Busiest hour", fmt.Sprintf("%s plays at %s", analysis.Num(st.BusiestHour.Plays), fmtTime(st.BusiestHour.Start))},
		{fmt.Sprintf("Days over %d plays", r.Params.DayLimit), fmt.Sprintf("%s of %s", analysis.Num(st.DaysOverLimit), analysis.Num(st.DaysWithPlays))},
		{fmt.Sprintf("Hours over %d plays", r.Params.HourLimit), analysis.Num(st.HoursOverLimit)},
		{fmt.Sprintf("Days with plays in %d+ hours", r.Params.NoSleepHours), analysis.Num(st.NoSleepDays)},
		{"Months averaging over the daily limit", fmt.Sprintf("%d of %d", st.MonthsOverLimit, st.MonthsTotal)},
		{"Plays that needed a third player", fmt.Sprintf("%s (%s)", analysis.Num(ig.ExcessPlays), analysis.Pct(ig.ExcessShare))},
		{"Double scrobbles (forgiven)", analysis.Num(ig.Duplicates)},
		{"Plays on a second player (forgiven)", analysis.Num(ig.SecondDevicePlays)},
		{"Same song again within 10 minutes", analysis.Pct(ig.LoopShare)},
		{"Plays under 15 s apart (after duplicates)", analysis.Pct(ig.UnderFifteenShare)},
		{"Different songs at the same second", fmt.Sprintf("%s (%s)", analysis.Num(ig.SameSecondPlays), analysis.Pct(ig.SameSecondShare))},
		{"Bursts of 5+ plays in a minute", fmt.Sprintf("%s, most %d", analysis.Num(ig.Bursts), ig.BurstMax)},
		{"Share of plays in the quietest 6 hours", analysis.Pct(ig.QuietShare)},
	})

	// Month chart.
	if ms := r.Charts.Months; len(ms) > 0 {
		if pdf.GetY() > 215 {
			pdf.AddPage() // keep the heading with its chart
		}
		heading("Plays a day, month by month")
		x0, y0, cw, ch := 14.0, pdf.GetY()+2, pageW, 45.0
		maxV := float64(r.Params.DayLimit) * 1.2
		for _, m := range ms {
			maxV = math.Max(maxV, m.PerDay)
		}
		maxV = niceCeil(maxV)
		yOf := func(v float64) float64 { return y0 + ch - v/maxV*ch }
		pdf.SetLineWidth(0.2)
		for _, l := range []struct {
			v   float64
			col string
			s   string
		}{{400, "#9ca3af", "400/day: one person, all day"}, {float64(r.Params.DayLimit), "#dc2626", fmt.Sprintf("%d/day", r.Params.DayLimit)}} {
			pdf.SetDrawColor(rgb(l.col))
			pdf.SetDashPattern([]float64{1, 1}, 0)
			pdf.Line(x0, yOf(l.v), x0+cw, yOf(l.v))
			pdf.SetFont("go", "", 6.5)
			ink(l.col)
			pdf.Text(x0+1, yOf(l.v)-1, l.s)
		}
		pdf.SetDashPattern(nil, 0)
		bars := barLayout(ms, cw, 8)
		for i, m := range ms {
			bw := bars[i].w
			h := m.PerDay / maxV * ch
			if m.Plays > 0 && h < 0.4 {
				h = 0.4
			}
			fill(dayColor(m.PerDay))
			pdf.Rect(x0+bars[i].x+bw*0.1, y0+ch-h, bw*0.8, h, "F")
			if bars[i].label != "" {
				pdf.SetFont("go", "", 6.5)
				ink(muted)
				pdf.Text(x0+bars[i].x, y0+ch+4, bars[i].label)
			}
		}
		pdf.SetY(y0 + ch + 7)
	}

	// Measurement.
	heading("Measurement")
	m := r.Measurement
	rows := [][2]string{
		{"Scope", m.Scope},
		{"Range", analysis.Date(m.From) + " → " + analysis.Date(m.To)},
		{"Plays analyzed", analysis.Num(m.PlaysAnalyzed)},
		{"History map", fmt.Sprintf("%d months", m.HistoryMonths)},
		{"Pages read / from cache", fmt.Sprintf("%s / %s plays", analysis.Num(m.Pages), analysis.Num(m.FromCache))},
		{"Plays with a known song length", analysis.Num(ig.DurationsKnown)},
	}
	if m.Note != "" {
		rows = append(rows, [2]string{"Note", m.Note})
	}
	kv(pdf, rows)

	// Gaps.
	heading("Time between plays")
	for i, g := range r.Charts.Gaps {
		pdf.SetFont("go", "", 8.5)
		ink(dark)
		y := pdf.GetY()
		pdf.CellFormat(22, 5, g.Label, "", 0, "L", false, 0, "")
		fill("#f3f4f6")
		pdf.Rect(38, y+1.5, 110, 2.2, "F")
		if i < 3 {
			fill("#dc2626")
		} else {
			fill("#6b7280")
		}
		if g.Share > 0 {
			pdf.Rect(38, y+1.5, math.Max(0.6, 110*g.Share), 2.2, "F")
		}
		pdf.SetX(150)
		ink(muted)
		pdf.CellFormat(46, 5, fmt.Sprintf("%s · %s", analysis.Num(g.Count), analysis.Pct(g.Share)), "", 1, "R", false, 0, "")
	}

	// Busiest days.
	if len(r.Charts.BusiestDays) > 0 {
		heading("Busiest days")
		table(pdf, []string{"Day (UTC)", "Plays", "Hours", "Busiest hour"}, []float64{70, 36, 36, 40}, func(add func(...string)) {
			for _, d := range r.Charts.BusiestDays {
				add(d.Date, analysis.Num(d.Plays), strconv.Itoa(d.Hours), strconv.Itoa(d.PeakHour))
			}
		})
	}

	// Bursts.
	if len(r.Charts.Bursts) > 0 {
		heading("Densest bursts")
		for _, b := range r.Charts.Bursts {
			if pdf.GetY() > 250 {
				pdf.AddPage()
			}
			pdf.SetFont("mono", "B", 8.5)
			ink(dark)
			pdf.CellFormat(100, 5, fmtTime(b.Start), "", 0, "L", false, 0, "")
			ink("#dc2626")
			pdf.CellFormat(0, 5, fmt.Sprintf("%d plays in %ds", b.Plays, b.End-b.Start), "", 1, "R", false, 0, "")
			pdf.SetFont("go", "", 8)
			ink(muted)
			for _, t := range b.Tracks {
				pdf.CellFormat(0, 4, time.Unix(t.TS, 0).UTC().Format("15:04:05")+"  "+t.Artist+" — "+t.Title, "", 1, "L", false, 0, "")
			}
			pdf.Ln(2)
		}
	}

	// Repeats.
	if len(r.Charts.TopRepeats) > 0 {
		heading("Most repeated songs")
		table(pdf, []string{"Song", "Plays", "Impossible"}, []float64{130, 26, 26}, func(add func(...string)) {
			for _, t := range r.Charts.TopRepeats {
				add(t.Artist+" — "+t.Title, analysis.Num(t.Plays), analysis.Num(t.RapidPlays))
			}
		})
	}

	heading("Method")
	pdf.SetFont("go", "", 8)
	ink(muted)
	p := r.Params
	pdf.MultiCell(0, 4, fmt.Sprintf("Copies of one song within %ds (up to %d) are double scrobbles and are removed, never scored. "+
		"Every other play is credited with the least time a player needs before Last.fm accepts it (half the song, at most 4 minutes; %ds when the length is unknown) "+
		"and packed onto at most %d players at once, so a phone plus a forgotten browser tab is fine; plays that still do not fit needed a third player. "+
		"Days over %d plays and hours over %d are beyond one person. All times UTC.",
		p.DupWindowSeconds, p.MaxCopies, p.MinPlaySeconds, p.Devices, p.DayLimit, p.HourLimit), "", "L", false)

	if err := pdf.Error(); err != nil {
		return err
	}
	return pdf.Output(w)
}

func kv(pdf *fpdf.Fpdf, rows [][2]string) {
	for _, row := range rows {
		if pdf.GetY() > 270 {
			pdf.AddPage()
		}
		pdf.SetFont("go", "", 8.5)
		pdf.SetTextColor(107, 114, 128)
		pdf.CellFormat(95, 5, row[0], "", 0, "L", false, 0, "")
		pdf.SetFont("mono", "", 8.5)
		pdf.SetTextColor(17, 24, 39)
		pdf.CellFormat(0, 5, row[1], "", 1, "R", false, 0, "")
	}
}

func table(pdf *fpdf.Fpdf, head []string, widths []float64, rows func(add func(...string))) {
	pdf.SetFont("go", "B", 8)
	pdf.SetTextColor(107, 114, 128)
	for i, h := range head {
		align := "R"
		if i == 0 {
			align = "L"
		}
		pdf.CellFormat(widths[i], 5, h, "B", 0, align, false, 0, "")
	}
	pdf.Ln(-1)
	rows(func(cells ...string) {
		if pdf.GetY() > 270 {
			pdf.AddPage()
		}
		pdf.SetFont("mono", "", 8)
		pdf.SetTextColor(17, 24, 39)
		for i, c := range cells {
			align := "R"
			if i == 0 {
				align = "L"
				c = clip(pdf, c, widths[i]-2)
			}
			pdf.CellFormat(widths[i], 5, c, "", 0, align, false, 0, "")
		}
		pdf.Ln(-1)
	})
}

func clip(pdf *fpdf.Fpdf, s string, w float64) string {
	if pdf.GetStringWidth(s) <= w {
		return s
	}
	rs := []rune(s)
	for len(rs) > 0 && pdf.GetStringWidth(string(rs)+"…") > w {
		rs = rs[:len(rs)-1]
	}
	return string(rs) + "…"
}

func fmtTime(ts int64) string {
	if ts <= 0 {
		return "–"
	}
	return time.Unix(ts, 0).UTC().Format("2006-01-02 15:04 UTC")
}
