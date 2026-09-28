package render

import (
	"fmt"
	"html"
	"math"
	"strings"
	"time"

	"github.com/swiftexplorer567/lastfm-integrity-scanner/internal/analysis"
)

// Plays-per-day color scale shared by every chart and format.
var dayScale = []struct {
	Max   float64
	Color string
	Label string
}{
	{0, "#1c232d", "none"},
	{50, "#14532d", "1–50"},
	{150, "#15803d", "50–150"},
	{400, "#22c55e", "150–400"},
	{600, "#f59e0b", "400–600"},
	{1000, "#ef4444", "600–1,000"},
	{math.Inf(1), "#f472b6", "1,000+"},
}

func dayColor(perDay float64) string {
	for _, s := range dayScale {
		if perDay <= s.Max {
			return s.Color
		}
	}
	return dayScale[len(dayScale)-1].Color
}

const (
	colGrid  = "#2a3340"
	colMuted = "#8b98a9"
	colLimit = "#f87171"
)

func esc(s string) string { return html.EscapeString(s) }

// MonthChartSVG draws plays per day for each month, with the one-person and
// impossible lines dashed across.
func MonthChartSVG(r *analysis.Report) string {
	ms := r.Charts.Months
	const W, H, left, right, top, bottom = 1000.0, 230.0, 8.0, 44.0, 14.0, 26.0
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %.0f %.0f" width="100%%" role="img" aria-label="Plays a day, month by month" font-family="Inter,system-ui,sans-serif">`, W, H)
	if len(ms) == 0 {
		b.WriteString(`</svg>`)
		return b.String()
	}
	maxV := float64(r.Params.DayLimit) * 1.2
	for _, m := range ms {
		maxV = math.Max(maxV, m.PerDay)
	}
	maxV = niceCeil(maxV)
	plotW, plotH := W-left-right, H-top-bottom
	y := func(v float64) float64 { return top + plotH - v/maxV*plotH }
	fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" font-size="11" fill="%s" text-anchor="end">%s</text>`, W-2, top+4, colMuted, analysis.Num(int(maxV)))
	bars := barLayout(ms, plotW, 34)
	for i, m := range ms {
		x, bw := left+bars[i].x, bars[i].w
		gap := math.Min(2, bw*0.2)
		h := m.PerDay / maxV * plotH
		if m.Plays > 0 && h < 1.5 {
			h = 1.5
		}
		fmt.Fprintf(&b, `<rect x="%.2f" y="%.2f" width="%.2f" height="%.2f" rx="1" fill="%s"><title>%s: %s plays, %s a day</title></rect>`,
			x+gap/2, top+plotH-h, math.Max(bw-gap, 0.8), h, dayColor(m.PerDay), periodLabel(m), analysis.Num(m.Plays), analysis.Num(int(math.Round(m.PerDay))))
		if bars[i].label != "" {
			fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" font-size="11" fill="%s">%s</text>`, x, H-8, colMuted, bars[i].label)
		}
	}
	fmt.Fprintf(&b, `<line x1="%.1f" x2="%.1f" y1="%.1f" y2="%.1f" stroke="%s"/>`, left, left+plotW, top+plotH, top+plotH, colGrid)
	// Limit lines over the bars, labels at opposite ends so they never collide.
	for _, l := range []struct {
		v      int
		label  string
		col    string
		x      float64
		anchor string
	}{
		{400, "400/day: one person, all day", "#cbd5e1", left + 4, "start"},
		{r.Params.DayLimit, fmt.Sprintf("%d/day", r.Params.DayLimit), colLimit, left + plotW - 4, "end"},
	} {
		yy := y(float64(l.v))
		fmt.Fprintf(&b, `<line x1="%.1f" x2="%.1f" y1="%.1f" y2="%.1f" stroke="%s" stroke-dasharray="4 4" stroke-width="1.2"/>`, left, left+plotW, yy, yy, l.col)
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" font-size="11" font-weight="600" fill="%s" text-anchor="%s" paint-order="stroke" stroke="#131a22" stroke-width="3">%s</text>`, l.x, yy-4, l.col, l.anchor, esc(l.label))
	}
	b.WriteString(`</svg>`)
	return b.String()
}

// YearHeatmapsSVG returns one calendar per year, newest first, with days in
// 20+ different hours outlined.
func YearHeatmapsSVG(r *analysis.Report) []YearMap {
	if len(r.Charts.Days) == 0 {
		return nil
	}
	byDate := make(map[string]analysis.DayStat, len(r.Charts.Days))
	for _, d := range r.Charts.Days {
		byDate[d.Date] = d
	}
	first, _ := time.Parse("2006-01-02", r.Charts.Days[0].Date)
	last, _ := time.Parse("2006-01-02", r.Charts.Days[len(r.Charts.Days)-1].Date)
	var out []YearMap
	for year := last.Year(); year >= first.Year(); year-- {
		start := time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC)
		end := time.Date(year, 12, 31, 0, 0, 0, 0, time.UTC)
		if start.Before(first) {
			start = first
		}
		if end.After(last) {
			end = last
		}
		out = append(out, yearMap(year, start, end, byDate, r.Params.NoSleepHours))
	}
	return out
}

type YearMap struct {
	Year  int
	Plays int
	SVG   string
}

func yearMap(year int, start, end time.Time, byDate map[string]analysis.DayStat, noSleep int) YearMap {
	const cell, gapPx, left, top = 11.0, 2.0, 30.0, 16.0
	jan1 := time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC)
	weekday := func(t time.Time) int { return (int(t.Weekday()) + 6) % 7 }
	col := func(t time.Time) int { return (t.YearDay() - 1 + weekday(jan1)) / 7 }
	cols := col(time.Date(year, 12, 31, 0, 0, 0, 0, time.UTC)) + 1
	W := left + float64(cols)*(cell+gapPx) + 2
	H := top + 7*(cell+gapPx) + 2
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %.0f %.0f" width="%.0f" role="img" aria-label="Plays per day in %d" font-family="Inter,system-ui,sans-serif">`, W, H, W, year)
	for i, name := range []string{"Mon", "", "Wed", "", "Fri", "", "Sun"} {
		if name != "" {
			fmt.Fprintf(&b, `<text x="0" y="%.1f" font-size="9" fill="%s">%s</text>`, top+float64(i)*(cell+gapPx)+cell-2, colMuted, name)
		}
	}
	for m := time.January; m <= time.December; m++ {
		d := time.Date(year, m, 1, 0, 0, 0, 0, time.UTC)
		if d.After(end) || time.Date(year, m+1, 0, 0, 0, 0, 0, time.UTC).Before(start) {
			continue
		}
		fmt.Fprintf(&b, `<text x="%.1f" y="10" font-size="9" fill="%s">%s</text>`, left+float64(col(d))*(cell+gapPx), colMuted, d.Format("Jan"))
	}
	total := 0
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		ds := byDate[d.Format("2006-01-02")]
		total += ds.Plays
		x := left + float64(col(d))*(cell+gapPx)
		y := top + float64(weekday(d))*(cell+gapPx)
		stroke := ""
		if ds.Hours >= noSleep {
			stroke = ` stroke="#e5e7eb" stroke-width="1.4"`
		}
		fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.0f" height="%.0f" rx="2" fill="%s"%s><title>%s: %s plays in %d hours, busiest hour %d</title></rect>`,
			x, y, cell, cell, dayColor(float64(ds.Plays)), stroke, d.Format("Mon 2 Jan 2006"), analysis.Num(ds.Plays), ds.Hours, ds.PeakHour)
	}
	b.WriteString(`</svg>`)
	return YearMap{Year: year, Plays: total, SVG: b.String()}
}

// WeekHourSVG draws plays by weekday and UTC hour.
func WeekHourSVG(r *analysis.Report) string {
	const cell, gapPx, left, top = 22.0, 3.0, 36.0, 18.0
	W := left + 24*(cell+gapPx)
	H := top + 7*(cell+gapPx)
	maxV := 1
	for _, row := range r.Charts.WeekHour {
		for _, v := range row {
			maxV = max(maxV, v)
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %.0f %.0f" width="100%%" style="max-width:%.0fpx" role="img" aria-label="Plays by weekday and hour" font-family="Inter,system-ui,sans-serif">`, W, H, W)
	for h := 0; h < 24; h += 6 {
		fmt.Fprintf(&b, `<text x="%.1f" y="11" font-size="10" fill="%s">%02d</text>`, left+float64(h)*(cell+gapPx), colMuted, h)
	}
	fmt.Fprintf(&b, `<text x="%.1f" y="11" font-size="10" fill="%s">23</text>`, left+23*(cell+gapPx), colMuted)
	days := []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}
	for d, row := range r.Charts.WeekHour {
		y := top + float64(d)*(cell+gapPx)
		fmt.Fprintf(&b, `<text x="0" y="%.1f" font-size="10" fill="%s">%s</text>`, y+cell-7, colMuted, days[d])
		for h, v := range row {
			a := 0.08 + 0.92*math.Sqrt(float64(v)/float64(maxV))
			if v == 0 {
				a = 0.04
			}
			fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.0f" height="%.0f" rx="3" fill="#38bdf8" fill-opacity="%.2f"><title>%s %02d:00 UTC: %s plays</title></rect>`,
				left+float64(h)*(cell+gapPx), y, cell, cell, a, days[d], h, analysis.Num(v))
		}
	}
	b.WriteString(`</svg>`)
	return b.String()
}

func niceCeil(v float64) float64 {
	if v <= 0 {
		return 1
	}
	p := math.Pow(10, math.Floor(math.Log10(v)))
	for _, m := range []float64{1, 1.2, 1.5, 2, 2.5, 3, 4, 5, 6, 8, 10} {
		if m*p >= v {
			return m * p
		}
	}
	return 10 * p
}

// bar is one entry of the history chart laid out along the time axis.
type bar struct {
	x, w  float64 // left edge and width, in the caller's units
	label string  // year label to draw under this bar, or ""
}

// barLayout gives each entry a width proportional to the days it covers, so
// a chart that mixes long periods and single months keeps a true time axis.
// Year labels go under the first entry of each year, if there is room.
func barLayout(ms []analysis.MonthStat, width, minLabelGap float64) []bar {
	days := make([]float64, len(ms))
	var total float64
	for i, m := range ms {
		d := float64(m.Days)
		if d <= 0 {
			d = 30.4
		}
		days[i] = d
		total += d
	}
	out := make([]bar, len(ms))
	x, lastLabel, lastYear := 0.0, math.Inf(-1), ""
	for i, m := range ms {
		w := days[i] / total * width
		out[i] = bar{x: x, w: w}
		if y := m.Month[:4]; y != lastYear {
			if x-lastLabel >= minLabelGap {
				out[i].label = y
				lastLabel = x
			}
			lastYear = y
		}
		x += w
	}
	return out
}

// periodLabel names a chart entry: "2024-05", or "2023-01, 214 days".
func periodLabel(m analysis.MonthStat) string {
	if m.Days > 31 {
		return fmt.Sprintf("%s, %d days", m.Month, m.Days)
	}
	return m.Month
}
