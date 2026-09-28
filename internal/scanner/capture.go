package scanner

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/swiftexplorer567/lastfm-integrity-scanner/internal/analysis"
	"github.com/swiftexplorer567/lastfm-integrity-scanner/internal/lastfm"
	"github.com/swiftexplorer567/lastfm-integrity-scanner/internal/scrobble"
)

// A Capture is everything a pre-import check read from Last.fm. It is saved
// next to the report (data/checks/<user>.json.gz, the latest per user) so
// the decision can be audited, and so new scoring can be tried on real
// accounts with `lfscan recheck` without asking Last.fm again.
type Capture struct {
	Version  int               `json:"version"`
	Info     lastfm.UserInfo   `json:"info"`
	Top      []lastfm.TopTrack `json:"top"`
	TopErr   string            `json:"top_error,omitempty"`
	Distinct int64             `json:"distinct"`
	Since    int64             `json:"since"`
	Now      int64             `json:"now"`
	Requests int               `json:"requests"`
	RoundsMS []int64           `json:"rounds_ms"`
	Decided  bool              `json:"decided_early"`
	Retries  map[string]int    `json:"retries,omitempty"`
	Periods  []captureUnit     `json:"periods"`
	Windows  []captureWindow   `json:"windows"`
}

type captureUnit struct {
	Label   string              `json:"label"`
	From    int64               `json:"from"`
	To      int64               `json:"to"`
	Days    int                 `json:"days"`
	Months  []lastfm.MonthCount `json:"months"`
	Total   int64               `json:"total"`
	OK      bool                `json:"ok"`
	Windows int                 `json:"windows"`
	Refined []captureUnit       `json:"refined,omitempty"`
}

type captureWindow struct {
	Period int                  `json:"period"`
	Month  int                  `json:"month"` // index into Refined, -1 for the period itself
	Plays  []lastfm.RawScrobble `json:"plays"`
}

const captureVersion = 1

func newCapture(info *lastfm.UserInfo, top []lastfm.TopTrack, topErr error, distinct int64, units []*unit,
	windows []*window, since, now int64, requests int, roundsMS []int64, decided bool) *Capture {
	cp := &Capture{
		Version: captureVersion, Info: *info, Top: top, Distinct: distinct, Since: since, Now: now,
		Requests: requests, RoundsMS: roundsMS, Decided: decided,
	}
	if topErr != nil {
		cp.TopErr = topErr.Error()
	}
	where := map[*unit][2]int{}
	flat := func(u *unit) captureUnit {
		return captureUnit{Label: u.label, From: u.from, To: u.to, Days: u.days, Months: u.months, Total: u.total, OK: u.ok, Windows: u.windows}
	}
	for i, u := range units {
		cu := flat(u)
		where[u] = [2]int{i, -1}
		for j, m := range u.refined {
			cu.Refined = append(cu.Refined, flat(m))
			where[m] = [2]int{i, j}
		}
		cp.Periods = append(cp.Periods, cu)
	}
	for _, w := range windows {
		at, ok := where[w.unit]
		if !ok {
			continue
		}
		cp.Windows = append(cp.Windows, captureWindow{Period: at[0], Month: at[1], Plays: w.plays})
	}
	return cp
}

// recheck analyses a capture as if it had just been read.
func (s *Scanner) recheck(cp *Capture, o CheckOptions) (*analysis.Report, error) {
	var units []*unit
	for _, cu := range cp.Periods {
		u := &unit{label: cu.Label, from: cu.From, to: cu.To, days: cu.Days, months: cu.Months, total: cu.Total, ok: cu.OK, windows: cu.Windows, isPeriod: true}
		for _, cm := range cu.Refined {
			u.refined = append(u.refined, &unit{label: cm.Label, from: cm.From, to: cm.To, days: cm.Days, months: cm.Months, total: cm.Total, ok: cm.OK, windows: cm.Windows})
		}
		units = append(units, u)
	}
	var windows []*window
	for _, cw := range cp.Windows {
		if cw.Period < 0 || cw.Period >= len(units) {
			continue
		}
		u := units[cw.Period]
		if cw.Month >= 0 {
			if cw.Month >= len(u.refined) {
				continue
			}
			u = u.refined[cw.Month]
		}
		windows = append(windows, &window{unit: u, plays: cw.Plays})
	}
	var leaves []*unit
	for _, u := range units {
		if len(u.refined) > 0 {
			leaves = append(leaves, u.refined...)
		} else {
			leaves = append(leaves, u)
		}
	}
	var topErr error
	if cp.TopErr != "" {
		topErr = errors.New(cp.TopErr)
	}
	info := cp.Info
	r, err := s.assemble(&info, cp.Top, topErr, cp.Distinct, units, leaves, windows, cp.Since, cp.Now, cp.Requests, o)
	if r != nil {
		r.Gate.RoundsMS = cp.RoundsMS
		r.Gate.DecidedEarly = cp.Decided
		r.Gate.Retries = cp.Retries
		// A re-score describes the check that read the data: its time and
		// how long its rounds took, not when the rules were run again.
		r.GeneratedAt = time.Unix(cp.Now, 0).UTC()
		r.Gate.ElapsedMS = 0
		for _, ms := range cp.RoundsMS {
			r.Gate.ElapsedMS += ms
		}
	}
	return r, err
}

// Recheck re-scores a saved capture with the current analysis. No request
// is made.
func (s *Scanner) Recheck(cp *Capture) (*analysis.Report, error) {
	return s.recheck(cp, s.Opts.Check)
}

func (s *Scanner) capturePath(user string) string {
	return filepath.Join(s.Store.Dir, "checks", scrobble.SafeName(user)+".json.gz")
}

// saveCapture writes the capture; a failure only costs the audit trail, so
// it is not reported.
func (s *Scanner) saveCapture(cp *Capture) {
	if s.Store.Dir == "" || os.Getenv("LFSCAN_CHECK_CAPTURE") == "0" {
		return
	}
	path := s.capturePath(cp.Info.Name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	f, err := os.CreateTemp(filepath.Dir(path), "capture-*.tmp")
	if err != nil {
		return
	}
	defer os.Remove(f.Name())
	zw := gzip.NewWriter(f)
	if json.NewEncoder(zw).Encode(cp) != nil || zw.Close() != nil || f.Close() != nil {
		return
	}
	os.Rename(f.Name(), path)
}

// LoadCapture reads a saved capture file.
func LoadCapture(path string) (*Capture, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var cp Capture
	if err := json.NewDecoder(zr).Decode(&cp); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if cp.Version != captureVersion {
		return nil, fmt.Errorf("%s: capture version %d, want %d", path, cp.Version, captureVersion)
	}
	return &cp, nil
}
