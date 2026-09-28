// Package scanner runs a scan end to end: profile, history (cached and
// incremental), monthly totals, song lengths for the suspicious parts, then
// the analysis.
package scanner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/swiftexplorer567/lastfm-integrity-scanner/internal/analysis"
	"github.com/swiftexplorer567/lastfm-integrity-scanner/internal/lastfm"
	"github.com/swiftexplorer567/lastfm-integrity-scanner/internal/scrobble"
)

// Scan modes.
const (
	ModeAuto  = "auto"  // full when the account is small enough, else quick
	ModeFull  = "full"  // every scrobble since the account opened
	ModeQuick = "quick" // recent window plus Last.fm's monthly totals
)

type Options struct {
	// Workers is how many requests are in flight at once per scan.
	Workers int
	// AutoFullMaxScrobbles: in auto mode, accounts up to this size get a
	// full scan (at 5 requests/s, 300k scrobbles is about 5 minutes).
	AutoFullMaxScrobbles int64
	// QuickDays and QuickMaxPages bound the recent window of a quick scan.
	QuickDays     int
	QuickMaxPages int
	// DurationLookups caps track.getInfo calls per scan. Lengths make the
	// "needs a third player" test much sharper, and they are cached.
	DurationLookups int
	// RefetchHours is how far before the end of the cached history an
	// incremental scan re-downloads, to catch offline plays synced late.
	RefetchHours int
	Params       analysis.Params
}

func DefaultOptions() Options {
	return Options{
		Workers:              8,
		AutoFullMaxScrobbles: 300_000,
		QuickDays:            90,
		QuickMaxPages:        150,
		DurationLookups:      300,
		RefetchHours:         72,
		Params:               analysis.DefaultParams(),
	}
}

type Scanner struct {
	Client    *lastfm.Client
	Store     scrobble.Store
	Durations *DurationCache
	Opts      Options

	// One scan per user at a time; they share the on-disk cache.
	locks sync.Map
}

func New(client *lastfm.Client, dataDir string, opts Options) (*Scanner, error) {
	dc, err := OpenDurationCache(dataDir)
	if err != nil {
		return nil, err
	}
	return &Scanner{Client: client, Store: scrobble.Store{Dir: dataDir}, Durations: dc, Opts: opts}, nil
}

// Request is one scan.
type Request struct {
	User string `json:"user"`
	Mode string `json:"mode,omitempty"`
	// Refresh ignores the cache and downloads everything in the window again.
	Refresh bool `json:"refresh,omitempty"`
	// DurationLookups overrides Options.DurationLookups when not nil.
	DurationLookups *int `json:"duration_lookups,omitempty"`
}

// Progress reports the stage of a scan and how far it is.
type Progress = lastfm.Progress

func (s *Scanner) Scan(ctx context.Context, req Request, progress Progress) (*analysis.Report, error) {
	started := time.Now()
	mu, _ := s.locks.LoadOrStore(scrobble.SafeName(req.User), &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()

	report := func(stage string) {
		if progress != nil {
			progress(stage, 0, 0)
		}
	}
	report("profile")
	info, err := s.Client.UserInfo(ctx, req.User)
	if err != nil {
		return nil, fmt.Errorf("user %q: %w", req.User, err)
	}
	user := info.Name
	now := time.Now().Unix()
	requests := 1

	mode := req.Mode
	if mode == "" || mode == ModeAuto {
		mode = ModeQuick
		if info.Playcount <= s.Opts.AutoFullMaxScrobbles {
			mode = ModeFull
		}
	}
	if mode != ModeFull && mode != ModeQuick {
		return nil, fmt.Errorf("unknown scan mode %q", req.Mode)
	}

	// History: reuse what is cached, download what is not.
	var cached *scrobble.Cached
	if !req.Refresh {
		if cached, err = s.Store.Load(user); err != nil {
			cached = nil // unreadable cache: start over
		}
	}
	// A cached full history makes a quick scan a full one for free.
	if mode == ModeQuick && cached != nil && cached.CoveredFrom == 0 && cached.CoveredTo > now-30*86400 {
		mode = ModeFull
	}
	windowFrom := int64(0)
	maxPages := 0
	if mode == ModeQuick {
		windowFrom = now - int64(s.Opts.QuickDays)*86400
		maxPages = s.Opts.QuickMaxPages
	}

	fetchFrom := windowFrom
	incremental := cached != nil && cached.CoveredFrom <= windowFrom && cached.CoveredTo >= windowFrom
	if incremental {
		fetchFrom = cached.CoveredTo - int64(s.Opts.RefetchHours)*3600
		if fetchFrom < windowFrom {
			fetchFrom = windowFrom
		}
		if fetchFrom < 0 {
			fetchFrom = 0
		}
	}
	fresh := scrobble.New(user)
	fr, err := s.Client.FetchHistory(ctx, fresh, fetchFrom, now, lastfm.FetchOptions{Workers: s.Opts.Workers, MaxPages: maxPages}, progress)
	if err != nil {
		return nil, fmt.Errorf("history: %w", err)
	}
	requests += fr.Pages + len(fr.MissingPages)

	var hist *scrobble.Cached
	switch {
	case incremental && !fr.Truncated:
		hist = cached
		hist.Replace(fetchFrom, fresh)
		hist.CoveredTo = now
	default:
		hist = &scrobble.Cached{History: fresh, CoveredFrom: fetchFrom, CoveredTo: now}
		if fr.Truncated {
			hist.CoveredFrom = fr.OldestTS
		}
	}
	fromCache := len(hist.Plays) - len(fresh.Plays)
	if fromCache < 0 {
		fromCache = 0
	}
	if len(fr.MissingPages) == 0 {
		if err := s.Store.Save(hist); err != nil {
			return nil, fmt.Errorf("cache: %w", err)
		}
	}

	scope := "full"
	analyzeFrom := int64(0)
	if mode == ModeQuick || hist.CoveredFrom > 0 {
		scope = "sample"
		analyzeFrom = max(windowFrom, hist.CoveredFrom)
	}
	window := hist.Window(analyzeFrom, now)

	// Monthly totals: our own when the history is complete, Last.fm's when
	// we only have a sample.
	var months []analysis.MonthStat
	if scope == "sample" && info.Registered > 0 {
		var known []lastfm.MonthCount
		s.Store.LoadJSON(user, "months.json", &known)
		byMonth := map[string]lastfm.MonthCount{}
		for _, m := range known {
			byMonth[m.Month] = m
		}
		list := lastfm.Months(info.Registered, now)
		mc, err := s.Client.MonthTotals(ctx, user, list, byMonth, now, s.Opts.Workers, progress)
		if err != nil {
			return nil, fmt.Errorf("monthly totals: %w", err)
		}
		requests += len(list) - len(byMonth)
		s.Store.SaveJSON(user, "months.json", mc)
		for _, m := range mc {
			months = append(months, analysis.MonthStat{Month: m.Month, Plays: m.Plays})
		}
	}

	// Song lengths for the songs behind the tightest spacing.
	lookups := s.Opts.DurationLookups
	if req.DurationLookups != nil {
		lookups = *req.DurationLookups
	}
	durations, fetched := s.durations(ctx, window, lookups, progress)
	requests += fetched

	report("analysis")
	measurement := analysis.Measurement{
		Pages:        fr.Pages,
		TotalPages:   fr.TotalPages,
		MissingPages: fr.MissingPages,
		FromCache:    fromCache,
		Requests:     requests,
	}
	if fr.Truncated {
		measurement.Note = fmt.Sprintf("Sample cut short at %d pages; it reaches back only to %s.", maxPages, analysis.Date(hist.CoveredFrom))
	}
	if len(fr.MissingPages) > 0 {
		measurement.Note += fmt.Sprintf(" %d pages could not be downloaded; the history has gaps.", len(fr.MissingPages))
	}
	r := analysis.Analyze(analysis.Input{
		Profile: analysis.Profile{
			Name: info.Name, RealName: info.RealName, URL: info.URL, Country: info.Country,
			Image: info.Image, Playcount: info.Playcount, Registered: info.Registered,
		},
		History:     window,
		Scope:       scope,
		From:        max(analyzeFrom, window.First()),
		To:          now,
		Months:      months,
		Durations:   durations,
		Measurement: measurement,
		Params:      s.Opts.Params,
	})
	r.Measurement.ElapsedSec = time.Since(started).Seconds()
	if err := s.SaveReport(r); err != nil {
		return r, fmt.Errorf("saving report: %w", err)
	}
	return r, nil
}

// durations looks up lengths for the songs most often followed closely by
// another song (under two minutes), which is where lengths change the
// verdict. Copies of the same song are skipped: those are double scrobbles
// or loops and are judged without lengths. It returns lengths by track index
// and how many requests it made.
func (s *Scanner) durations(ctx context.Context, h *scrobble.History, budget int, progress Progress) (map[uint32]int, int) {
	out := map[uint32]int{}
	if s.Durations == nil {
		return out, 0
	}
	songs := make([]string, len(h.Tracks))
	for i, t := range h.Tracks {
		songs[i] = analysis.SongKey(t.Artist, t.Title)
	}
	tight := map[uint32]int{}
	for i := 0; i+1 < len(h.Plays); i++ {
		a, b := h.Plays[i], h.Plays[i+1]
		if b.TS-a.TS < 120 && songs[a.Track] != songs[b.Track] {
			tight[a.Track]++
		}
	}
	type cand struct {
		track uint32
		n     int
	}
	var cands []cand
	for t, n := range tight {
		cands = append(cands, cand{t, n})
	}
	sort.Slice(cands, func(a, b int) bool {
		if cands[a].n != cands[b].n {
			return cands[a].n > cands[b].n
		}
		return cands[a].track < cands[b].track
	})
	var missing []scrobble.Track
	var missingIdx []uint32
	for _, c := range cands {
		t := h.Tracks[c.track]
		if d, ok := s.Durations.Get(t); ok {
			if d > 0 {
				out[c.track] = d
			}
			continue
		}
		if len(missing) < budget {
			missing = append(missing, t)
			missingIdx = append(missingIdx, c.track)
		}
	}
	if len(missing) == 0 || s.Client == nil {
		return out, 0
	}
	got := s.Client.TrackDurations(ctx, missing, s.Opts.Workers, progress)
	for i, t := range missing {
		d, ok := got[t]
		if !ok {
			continue
		}
		s.Durations.Put(t, d)
		if d > 0 {
			out[missingIdx[i]] = d
		}
	}
	s.Durations.Save()
	return out, len(missing)
}

// SaveReport writes the report as the user's latest and into their history.
func (s *Scanner) SaveReport(r *analysis.Report) error {
	dir := filepath.Join(s.Store.Dir, "reports", scrobble.SafeName(r.User.Name))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	name := r.GeneratedAt.Format("20060102T150405Z") + ".json"
	if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
		return err
	}
	tmp := filepath.Join(dir, "latest.json.tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "latest.json"))
}

// LatestReport loads the most recent saved report for user.
func (s *Scanner) LatestReport(user string) (*analysis.Report, error) {
	b, err := os.ReadFile(filepath.Join(s.Store.Dir, "reports", scrobble.SafeName(user), "latest.json"))
	if err != nil {
		return nil, err
	}
	var r analysis.Report
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return &r, nil
}
