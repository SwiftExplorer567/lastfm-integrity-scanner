package render

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"math"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/gomonobold"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
	"golang.org/x/image/vector"

	"github.com/swiftexplorer567/lastfm-integrity-scanner/internal/analysis"
)

// The summary card is laid out once as a list of primitives and then either
// written as SVG or rasterized to PNG, so both look the same.

const cardW, cardH = 1200.0, 675.0

type fontID int

const (
	fRegular fontID = iota
	fBold
	fMono
)

type prim struct {
	rect    bool
	x, y    float64 // text: baseline start (or end/middle per anchor)
	w, h    float64
	radius  float64
	fill    string
	opacity float64
	text    string
	size    float64
	font    fontID
	anchor  string // "", "end", "middle"
	dashed  bool   // horizontal dashed line (rect with h≈1)
}

func rect(x, y, w, h, radius float64, fill string) prim {
	return prim{rect: true, x: x, y: y, w: w, h: h, radius: radius, fill: fill, opacity: 1}
}

func text(x, y float64, s string, size float64, f fontID, fill string) prim {
	return prim{x: x, y: y, text: s, size: size, font: f, fill: fill, opacity: 1}
}

var verdictColor = map[string]string{"clean": "#22c55e", "review": "#f59e0b", "suspect": "#f87171"}
var actionStyle = map[string][2]string{
	"keep":    {"#14532d", "#86efac"},
	"adjust":  {"#1e3a5f", "#93c5fd"},
	"review":  {"#78350f", "#fcd34d"},
	"exclude": {"#7f1d1d", "#fecaca"},
}
var kindStyle = map[string][2]string{
	"fake":    {"#3b1219", "#f87171"},
	"volume":  {"#3b2a0f", "#f59e0b"},
	"pattern": {"#2e1f47", "#a78bfa"},
	"benign":  {"#0f2e22", "#22c55e"},
}

func cardLayout(r *analysis.Report) []prim {
	const (
		bg, panel, line = "#0d1117", "#131a22", "#243040"
		fg, muted       = "#e6edf3", "#8b98a9"
		pad             = 40.0
	)
	var ps []prim
	add := func(p ...prim) { ps = append(ps, p...) }
	add(rect(0, 0, cardW, cardH, 0, bg))

	// Header.
	add(text(pad, 58, "LAST.FM INTEGRITY REPORT", 13, fBold, muted))
	add(text(pad, 100, fit(r.User.Name, 34, fBold, 640), 34, fBold, fg))
	sub := fmt.Sprintf("%s scrobbles", analysis.Num(r.User.Playcount))
	if r.User.Registered > 0 {
		sub += " · joined " + analysis.Date(r.User.Registered)
	}
	sub += fmt.Sprintf(" · scanned %s (%s)", r.GeneratedAt.Format("2006-01-02"), r.Measurement.Scope)
	add(text(pad, 128, fit(sub, 15, fRegular, 660), 15, fRegular, muted))

	// Score box.
	vc := verdictColor[r.Score.Verdict]
	add(rect(cardW-pad-300, 36, 300, 104, 12, panel))
	add(rect(cardW-pad-300+16, 52, 96, 72, 10, "#0d1117"))
	sc := text(cardW-pad-300+64, 106, strconv.Itoa(r.Score.Value), 42, fMono, vc)
	sc.anchor = "middle"
	add(sc)
	add(text(cardW-pad-300+128, 80, r.Score.Label, 22, fBold, vc))
	as := actionStyle[r.Leaderboard.Action]
	label := "leaderboard: " + r.Leaderboard.Action
	lw := measure(label, 13, fBold) + 20
	add(rect(cardW-pad-300+128, 94, lw, 24, 12, as[0]))
	add(text(cardW-pad-300+138, 111, label, 13, fBold, as[1]))

	// Signals.
	top := 168.0
	add(text(pad, top, "SIGNALS", 12, fBold, muted))
	y := top + 14
	shown := 0
	for _, s := range r.Signals {
		if shown == 5 {
			break
		}
		st := kindStyle[s.Kind]
		pts := "ok"
		if s.Points > 0 {
			pts = "+" + strconv.Itoa(s.Points)
		}
		add(rect(pad, y, 620, 45, 8, panel))
		add(rect(pad+10, y+10, 44, 24, 6, st[0]))
		pt := text(pad+32, y+27, pts, 13, fMono, st[1])
		pt.anchor = "middle"
		add(pt)
		add(text(pad+66, y+19, fit(s.Title, 15, fBold, 540), 15, fBold, fg))
		add(text(pad+66, y+37, fit(s.Detail, 12.5, fRegular, 540), 12.5, fRegular, muted))
		y += 50
		shown++
	}
	if shown == 0 {
		add(text(pad, y+24, "Nothing unusual.", 15, fRegular, muted))
	}
	if more := len(r.Signals) - shown; more > 0 {
		add(text(pad, y+10, fmt.Sprintf("+ %d more in the full report", more), 12, fRegular, muted))
	}

	// Stats.
	type tile struct {
		h, v string
		warn bool
	}
	ig, st := r.Integrity, r.Stats
	adj := analysis.Num(r.Leaderboard.AdjustedScrobbles)
	if r.Leaderboard.Estimated {
		adj = "~" + adj
	}
	tiles := []tile{
		{"AVERAGE A DAY", analysis.Num(int64(math.Round(st.AvgPerDay))), st.AvgPerDay > float64(r.Params.LifetimeLimit)},
		{"BUSIEST DAY", analysis.Num(st.BusiestDay.Plays), st.BusiestDay.Plays > r.Params.DayLimit},
		{fmt.Sprintf("DAYS OVER %d", r.Params.DayLimit), analysis.Num(st.DaysOverLimit), st.DaysOverLimit > 0},
		{fmt.Sprintf("HOURS OVER %d", r.Params.HourLimit), analysis.Num(st.HoursOverLimit), st.HoursOverLimit > 0},
		{"NEEDS A 3RD PLAYER", analysis.Pct(ig.ExcessShare), ig.ExcessPlays > 0},
		{"DOUBLE SCROBBLES", analysis.Num(ig.Duplicates), false},
		{"MONTHS OVER LIMIT", fmt.Sprintf("%d / %d", st.MonthsOverLimit, st.MonthsTotal), st.MonthsOverLimit > 0},
		{"ADJUSTED COUNT", adj, false},
	}
	tx, ty := 700.0, top+14
	for i, t := range tiles {
		x := tx + float64(i%2)*234
		yy := ty + float64(i/2)*63
		add(rect(x, yy, 226, 57, 8, panel))
		add(text(x+12, yy+20, t.h, 10.5, fBold, muted))
		c := fg
		if t.warn {
			c = "#f87171"
		}
		add(text(x+12, yy+44, fit(t.v, 20, fMono, 200), 20, fMono, c))
	}

	// Month chart.
	chartTop, chartH := 480.0, 146.0
	add(text(pad, chartTop-8, "PLAYS A DAY, MONTH BY MONTH", 12, fBold, muted))
	add(rect(pad, chartTop, cardW-2*pad, chartH+14, 10, panel))
	ms := r.Charts.Months
	if len(ms) > 0 {
		maxV := float64(r.Params.DayLimit) * 1.2
		for _, m := range ms {
			maxV = math.Max(maxV, m.PerDay)
		}
		maxV = niceCeil(maxV)
		px, pw, py, ph := pad+12, cardW-2*pad-24, chartTop+10, chartH-14
		yOf := func(v float64) float64 { return py + ph - v/maxV*ph }
		bw := pw / float64(len(ms))
		g := math.Min(2, bw*0.2)
		for i, m := range ms {
			h := m.PerDay / maxV * ph
			if m.Plays > 0 && h < 1.5 {
				h = 1.5
			}
			add(rect(px+float64(i)*bw+g/2, py+ph-h, math.Max(bw-g, 0.8), h, 0, dayColor(m.PerDay)))
			if strings.HasSuffix(m.Month, "-01") || i == 0 {
				add(text(px+float64(i)*bw, chartTop+chartH+8, m.Month[:4], 10.5, fRegular, muted))
			}
		}
		// Limit lines on top of the bars so they stay visible.
		for _, l := range []struct {
			v   float64
			col string
		}{{400, "#cbd5e1"}, {float64(r.Params.DayLimit), "#f87171"}} {
			d := rect(px, yOf(l.v), pw, 1.2, 0, l.col)
			d.dashed = true
			d.opacity = 0.9
			add(d)
		}
		lbl := text(px+pw, yOf(float64(r.Params.DayLimit))-5, fmt.Sprintf("%d/day", r.Params.DayLimit), 11, fBold, "#f87171")
		lbl.anchor = "end"
		add(lbl)
	}
	add(text(pad, cardH-16, "Double scrobbles and a second player are forgiven. All times UTC. Full report: HTML / PDF / JSON.", 11.5, fRegular, muted))
	return ps
}

// CardSVG renders the summary card as SVG.
func CardSVG(r *analysis.Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %.0f %.0f" width="%.0f" height="%.0f">`, cardW, cardH, cardW, cardH)
	families := map[fontID]string{
		fRegular: `font-family="Inter,'Go',system-ui,sans-serif"`,
		fBold:    `font-family="Inter,'Go',system-ui,sans-serif" font-weight="700"`,
		fMono:    `font-family="ui-monospace,'Go Mono',Menlo,monospace" font-weight="700"`,
	}
	for _, p := range cardLayout(r) {
		if p.rect {
			if p.dashed {
				fmt.Fprintf(&b, `<line x1="%.1f" x2="%.1f" y1="%.1f" y2="%.1f" stroke="%s" stroke-opacity="%.2f" stroke-dasharray="5 5"/>`, p.x, p.x+p.w, p.y, p.y, p.fill, p.opacity)
				continue
			}
			fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" rx="%.0f" fill="%s"/>`, p.x, p.y, p.w, p.h, p.radius, p.fill)
			continue
		}
		anchor := ""
		if p.anchor != "" {
			anchor = fmt.Sprintf(` text-anchor="%s"`, p.anchor)
		}
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" font-size="%.1f" %s fill="%s"%s>%s</text>`, p.x, p.y, p.size, families[p.font], p.fill, anchor, esc(p.text))
	}
	b.WriteString(`</svg>`)
	return b.String()
}

// Raster output: 2x for sharp text on high-density screens.
const pngScale = 2.0

func renderPNG(w io.Writer, r *analysis.Report) error {
	img := image.NewRGBA(image.Rect(0, 0, int(cardW*pngScale), int(cardH*pngScale)))
	for _, p := range cardLayout(r) {
		c := parseHex(p.fill, p.opacity)
		if p.rect {
			if p.dashed {
				for x := p.x; x < p.x+p.w; x += 10 {
					fillRoundRect(img, x*pngScale, p.y*pngScale, math.Min(5, p.x+p.w-x)*pngScale, pngScale, 0, c)
				}
				continue
			}
			fillRoundRect(img, p.x*pngScale, p.y*pngScale, p.w*pngScale, p.h*pngScale, p.radius*pngScale, c)
			continue
		}
		face := faceFor(p.font, p.size*pngScale)
		d := &font.Drawer{Dst: img, Src: image.NewUniform(c), Face: face}
		x := p.x * pngScale
		switch p.anchor {
		case "end":
			x -= float64(d.MeasureString(p.text)) / 64
		case "middle":
			x -= float64(d.MeasureString(p.text)) / 128
		}
		d.Dot = fixed.Point26_6{X: fixed.Int26_6(x * 64), Y: fixed.Int26_6(p.y * pngScale * 64)}
		d.DrawString(p.text)
	}
	return (&png.Encoder{CompressionLevel: png.BestSpeed}).Encode(w, img)
}

func fillRoundRect(dst draw.Image, x, y, w, h, r float64, c color.Color) {
	if w <= 0 || h <= 0 {
		return
	}
	// Rasterize only the rectangle's own pixels.
	x0, y0 := int(math.Floor(x)), int(math.Floor(y))
	x1, y1 := int(math.Ceil(x+w)), int(math.Ceil(y+h))
	z := vector.NewRasterizer(x1-x0, y1-y0)
	r = math.Min(r, math.Min(w, h)/2)
	fx, fy, fw, fh, fr := float32(x-float64(x0)), float32(y-float64(y0)), float32(w), float32(h), float32(r)
	z.MoveTo(fx+fr, fy)
	z.LineTo(fx+fw-fr, fy)
	z.QuadTo(fx+fw, fy, fx+fw, fy+fr)
	z.LineTo(fx+fw, fy+fh-fr)
	z.QuadTo(fx+fw, fy+fh, fx+fw-fr, fy+fh)
	z.LineTo(fx+fr, fy+fh)
	z.QuadTo(fx, fy+fh, fx, fy+fh-fr)
	z.LineTo(fx, fy+fr)
	z.QuadTo(fx, fy, fx+fr, fy)
	z.ClosePath()
	z.Draw(dst, image.Rect(x0, y0, x1, y1), image.NewUniform(c), image.Point{})
}

func parseHex(s string, opacity float64) color.Color {
	s = strings.TrimPrefix(s, "#")
	v, _ := strconv.ParseUint(s, 16, 32)
	a := uint8(math.Round(255 * opacity))
	if opacity == 0 {
		a = 255
	}
	// Premultiplied alpha.
	pm := func(c uint64) uint8 { return uint8(uint64(c) * uint64(a) / 255) }
	return color.RGBA{pm(v >> 16 & 0xff), pm(v >> 8 & 0xff), pm(v & 0xff), a}
}

var (
	fontsOnce sync.Once
	fonts     map[fontID]*opentype.Font
	facesMu   sync.Mutex
	faces     = map[[2]float64]font.Face{}
)

func loadFonts() {
	fonts = map[fontID]*opentype.Font{}
	for id, data := range map[fontID][]byte{fRegular: goregular.TTF, fBold: gobold.TTF, fMono: gomonobold.TTF} {
		f, err := opentype.Parse(data)
		if err != nil {
			panic(err) // embedded fonts; cannot fail
		}
		fonts[id] = f
	}
}

func faceFor(id fontID, size float64) font.Face {
	fontsOnce.Do(loadFonts)
	facesMu.Lock()
	defer facesMu.Unlock()
	k := [2]float64{float64(id), size}
	if f, ok := faces[k]; ok {
		return f
	}
	f, err := opentype.NewFace(fonts[id], &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		panic(err)
	}
	faces[k] = f
	return f
}

func measure(s string, size float64, id fontID) float64 {
	return float64(font.MeasureString(faceFor(id, size), s)) / 64
}

// fit shortens s with an ellipsis until it is at most maxW wide.
func fit(s string, size float64, id fontID, maxW float64) string {
	if measure(s, size, id) <= maxW {
		return s
	}
	rs := []rune(s)
	lo, hi := 0, len(rs)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if measure(string(rs[:mid])+"…", size, id) <= maxW {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return strings.TrimSpace(string(rs[:lo])) + "…"
}
