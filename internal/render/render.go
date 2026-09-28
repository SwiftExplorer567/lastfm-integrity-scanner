// Package render turns a report into the formats admins send around: JSON
// for machines, HTML for reading, PDF for attaching, and a PNG or SVG summary
// card for chat.
package render

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/swiftexplorer567/lastfm-integrity-scanner/internal/analysis"
)

type Format string

const (
	JSON Format = "json"
	HTML Format = "html"
	PDF  Format = "pdf"
	PNG  Format = "png"
	SVG  Format = "svg"
)

var Formats = []Format{JSON, HTML, PDF, PNG, SVG}

func ParseFormat(s string) (Format, error) {
	f := Format(strings.ToLower(strings.TrimPrefix(s, ".")))
	for _, known := range Formats {
		if f == known {
			return f, nil
		}
	}
	return "", fmt.Errorf("unknown format %q (want one of json, html, pdf, png, svg)", s)
}

func (f Format) ContentType() string {
	switch f {
	case JSON:
		return "application/json"
	case HTML:
		return "text/html; charset=utf-8"
	case PDF:
		return "application/pdf"
	case PNG:
		return "image/png"
	case SVG:
		return "image/svg+xml"
	}
	return "application/octet-stream"
}

// Render writes r in format f.
func Render(w io.Writer, r *analysis.Report, f Format) error {
	switch f {
	case JSON:
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(r)
	case HTML:
		return renderHTML(w, r)
	case PDF:
		return renderPDF(w, r)
	case PNG:
		return renderPNG(w, r)
	case SVG:
		_, err := io.WriteString(w, CardSVG(r))
		return err
	}
	return fmt.Errorf("unknown format %q", f)
}

// Filename is a sensible download name for a report.
func Filename(r *analysis.Report, f Format) string {
	kind := "integrity"
	if r.Gate != nil {
		kind = "check"
	}
	return fmt.Sprintf("%s-%s-%s.%s", kind, strings.ToLower(r.User.Name), r.GeneratedAt.Format("20060102"), f)
}

//go:embed report.html.tmpl
var reportTmpl string

var funcs = template.FuncMap{
	"num":  func(v any) string { return numAny(v) },
	"num0": func(f float64) string { return analysis.Num(int64(math.Round(f))) },
	"pct":  analysis.Pct,
	"date": func(ts int64) string {
		if ts <= 0 {
			return "–"
		}
		return analysis.Date(ts)
	},
	"datetime": func(ts int64) string {
		if ts <= 0 {
			return "–"
		}
		return time.Unix(ts, 0).UTC().Format("2006-01-02 15:04 UTC")
	},
	"upper": strings.ToUpper,
	"msec":  func(ms int64) float64 { return float64(ms) / 1000 },
	"dur": func(sec int64) string {
		switch {
		case sec < 120:
			return fmt.Sprintf("%ds", sec)
		case sec < 7200:
			return fmt.Sprintf("%d min", sec/60)
		}
		return fmt.Sprintf("%.1f h", float64(sec)/3600)
	},
	"clock": func(ts int64) string { return time.Unix(ts, 0).UTC().Format("15:04:05") },
	"sub":   func(a, b int) int { return a - b },
	"sub64": func(a, b int64) int64 { return a - b },
	"div":   func(a, b int) int { return a / b },
	"barw": func(share float64) string {
		w := share * 100
		if w > 0 && w < 0.8 {
			w = 0.8
		}
		return fmt.Sprintf("%.1f", w)
	},
}

var htmlTmpl = template.Must(template.New("report").Funcs(funcs).Parse(reportTmpl))

//go:embed check.html.tmpl
var checkTmplSrc string

var checkTmpl = template.Must(template.New("check").Funcs(funcs).Parse(checkTmplSrc))

type checkView struct {
	R           *analysis.Report
	MonthSVG    template.HTML
	OverviewSVG template.HTML
	BudgetSVG   template.HTML
	Classes     []struct{ Kind, Color, Label, Help string }
	Focus       []focusView
	FocusClean  bool
	ReviewAt    int
	SuspectAt   int
}

type focusView struct {
	S           analysis.Sample
	Excess      int
	Dups        int
	ActivitySVG template.HTML
	LaneSVG     template.HTML
	Rows        []zoomRow
	Open        bool
}

type zoomRow struct {
	Clock, Gap, Artist, Title, Label, Color string
	Tight, Rapid                            bool
}

// newCheckView picks what the check report shows: up to three windows with
// impossible plays or loops, or, for a clean account, its busiest window
// for comparison.
func newCheckView(r *analysis.Report) checkView {
	g := r.Gate
	v := checkView{
		R:         r,
		MonthSVG:  template.HTML(MonthChartSVG(r)),
		ReviewAt:  analysis.ReviewAt,
		SuspectAt: analysis.SuspectAt,
		BudgetSVG: template.HTML(TimeBudgetSVG(g)),
	}
	for _, c := range classStyle {
		v.Classes = append(v.Classes, struct{ Kind, Color, Label, Help string }{c.Kind, c.Color, c.Label, c.Help})
	}
	if len(g.Samples) > 0 {
		v.OverviewSVG = template.HTML(SamplesOverviewSVG(g))
	}
	var detailed []analysis.Sample
	for _, s := range g.Samples {
		if len(s.Zoom) > 0 {
			detailed = append(detailed, s)
		}
	}
	sort.SliceStable(detailed, func(a, b int) bool {
		if detailed[a].Suspicion != detailed[b].Suspicion {
			return detailed[a].Suspicion > detailed[b].Suspicion
		}
		return detailed[a].PerHour > detailed[b].PerHour
	})
	var pick []analysis.Sample
	for _, s := range detailed {
		if s.Suspicion > 0.01 && len(pick) < 3 {
			pick = append(pick, s)
		}
	}
	if len(pick) == 0 && len(detailed) > 0 {
		pick = append(pick, detailed[0])
		v.FocusClean = true
	}
	for i, s := range pick {
		f := focusView{
			S: s, Excess: s.Counts[analysis.ClassExcess],
			Dups:        s.Counts[analysis.ClassDuplicate] + s.Counts[analysis.ClassEcho],
			ActivitySVG: template.HTML(ActivitySVG(s, r.Params.HourLimit)),
			LaneSVG:     template.HTML(LaneSVG(s)),
			Open:        i == 0 && !v.FocusClean,
		}
		var prev int64
		for j, p := range s.Zoom {
			gap := "–"
			if j > 0 {
				gap = fmt.Sprintf("%ds", p.TS-prev)
			}
			f.Rows = append(f.Rows, zoomRow{
				Clock: time.Unix(p.TS, 0).UTC().Format("15:04:05"), Gap: gap, Artist: p.Artist, Title: p.Title,
				Label: classLabel(p.Kind), Color: classColor(p.Kind), Rapid: p.Rapid, Tight: j > 0 && p.TS-prev < 15,
			})
			prev = p.TS
		}
		v.Focus = append(v.Focus, f)
	}
	return v
}

type htmlView struct {
	R         *analysis.Report
	MonthSVG  template.HTML
	WeekSVG   template.HTML
	Years     []htmlYear
	Scale     []scaleEntry
	ReviewAt  int
	SuspectAt int
}

type htmlYear struct {
	Year  int
	Plays int
	SVG   template.HTML
}

type scaleEntry struct{ Color, Label string }

func renderHTML(w io.Writer, r *analysis.Report) error {
	if r.Gate != nil {
		return checkTmpl.Execute(w, newCheckView(r))
	}
	v := htmlView{
		R:         r,
		MonthSVG:  template.HTML(MonthChartSVG(r)),
		WeekSVG:   template.HTML(WeekHourSVG(r)),
		ReviewAt:  analysis.ReviewAt,
		SuspectAt: analysis.SuspectAt,
	}
	for _, y := range YearHeatmapsSVG(r) {
		v.Years = append(v.Years, htmlYear{Year: y.Year, Plays: y.Plays, SVG: template.HTML(y.SVG)})
	}
	for _, s := range dayScale {
		v.Scale = append(v.Scale, scaleEntry{s.Color, s.Label})
	}
	return htmlTmpl.Execute(w, v)
}

func numAny(v any) string {
	switch n := v.(type) {
	case int:
		return analysis.Num(n)
	case int64:
		return analysis.Num(n)
	case float64:
		return analysis.Num(int64(math.Round(n)))
	}
	return fmt.Sprint(v)
}
