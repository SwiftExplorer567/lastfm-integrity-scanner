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
	return fmt.Sprintf("integrity-%s-%s.%s", strings.ToLower(r.User.Name), r.GeneratedAt.Format("20060102"), f)
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
