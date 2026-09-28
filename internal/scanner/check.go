package scanner

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/swiftexplorer567/lastfm-integrity-scanner/internal/analysis"
	"github.com/swiftexplorer567/lastfm-integrity-scanner/internal/lastfm"
	"github.com/swiftexplorer567/lastfm-integrity-scanner/internal/scrobble"
)

// The pre-import check answers "may this account into the system?" in a few
// seconds, however many scrobbles it has, without downloading its history.
//
// It leans on one property of user.getRecentTracks: a request bounded by
// from/to returns both Last.fm's exact count of scrobbles in that range
// (@attr.total) and the newest plays in it. So:
//
//  1. user.getInfo and user.getTopTracks(overall) in parallel. Lifetime
//     per-song totals are strong evidence on their own: 400,000 plays of six
//     songs need more hours than the account has existed.
//  2. The account's life is cut into ~24 periods; one request per period
//     gives its exact play count (the whole-history chart) and a window of
//     consecutive plays from it (evidence; 500 by default).
//  3. The densest periods are split into months, one request each, which
//     pins down where the volume is and reads more windows there; in the
//     same round, a window from the middle of the densest periods.
//
// Rounds 3 and 4 of the first design were merged into one round after the
// first real runs showed each round costs one full Last.fm round trip
// (2–3 s for a 1,000-play page), whatever the number of requests in it.
//
// Every window goes through the same analysis as a full scan (double
// scrobbles forgiven, a second player accepted, plays needing a third player
// counted). About 20–40 requests, sent in 3 parallel rounds, well inside
// Last.fm's allowance of short bursts.

// CheckOptions tunes the pre-import check.
type CheckOptions struct {
	// Deadline bounds the whole check. What has not arrived by then is left
	// out and the gate says so.
	Deadline time.Duration
	// Periods is how many slices the account's life is cut into.
	Periods int
	// Refine is how many of the densest periods get split into months.
	Refine int
	// Window is how many consecutive plays each request reads (max 1000).
	// Last.fm answers smaller pages faster; 500 keeps each round well under
	// the deadline while still showing hours of consecutive plays.
	Window int
	// TopTracks is how many lifetime top tracks to read.
	TopTracks int
	// MaxRequests caps the requests one check may make.
	MaxRequests int
	// Workers is how many requests of one check run at once. It should cover
	// a whole round: a round takes as long as its slowest wave, so 22
	// requests on 16 workers cost two round trips instead of one.
	Workers int
	// Concurrent checks run at once; more wait their turn, so the first in
	// line finish fast instead of all of them finishing slowly.
	Concurrent int
	// CacheTTL: a result this recent is returned without asking Last.fm.
	CacheTTL time.Duration
	// ExtraProbes: when rounds 1–2 found some evidence but no verdict, this
	// many more periods get a window from their middle in round 3.
	ExtraProbes int
	// Patient makes a check wait for the shared request budget to refill
	// instead of reading fewer periods (see check1). Batch runs want the
	// full sample on every account; live sign-ups want an answer now.
	Patient bool
	// HedgeAfter: a request with no answer by then gets a twin, and the
	// first answer wins. In the first 100-account run most requests took
	// 1–3 s but a few hung for 7–8 s and pushed checks past the deadline.
	HedgeAfter time.Duration
}

func DefaultCheckOptions() CheckOptions {
	return CheckOptions{
		Deadline:    9 * time.Second,
		Periods:     24,
		Refine:      2,
		Window:      500,
		TopTracks:   200,
		MaxRequests: 64,
		Workers:     32,
		Concurrent:  3,
		CacheTTL:    6 * time.Hour,
		HedgeAfter:  2500 * time.Millisecond,
		ExtraProbes: 8,
	}
}

// Gate decisions.
const (
	GatePass    = "pass"
	GateReview  = "review"
	GateBlock   = "block"
	GateUnknown = "unknown"
)

type checkState struct {
	once  sync.Once
	sem   chan struct{}
	mu    sync.Mutex
	cache map[string]cachedCheck
}

type cachedCheck struct {
	r  *analysis.Report
	at time.Time
}

// unit is a stretch of time read with one request: a period or a month.
type unit struct {
	label    string
	from, to int64
	days     int
	months   []lastfm.MonthCount
	total    int64
	ok       bool
	windows  int
	refined  []*unit
	isPeriod bool
	// parent is set for a probe into the middle of a period: its window
	// stands for the parent's plays.
	parent *unit
}

// Check runs the pre-import check for user. force skips the cache.
func (s *Scanner) Check(ctx context.Context, user string, force bool) (*analysis.Report, error) {
	o := s.Opts.Check
	cs := &s.check
	cs.once.Do(func() {
		cs.sem = make(chan struct{}, max(1, o.Concurrent))
		cs.cache = map[string]cachedCheck{}
	})
	key := scrobble.SafeName(user)
	if !force {
		cs.mu.Lock()
		c, ok := cs.cache[key]
		cs.mu.Unlock()
		if ok && time.Since(c.at) < o.CacheTTL {
			return c.r, nil
		}
	}

	queued := time.Now()
	select {
	case cs.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-cs.sem }()
	if o.Patient && s.Client != nil {
		// Wait, before the deadline starts, until a full check's requests
		// can go out at once.
		// Never more than the bucket can hold: after rate-limit answers the
		// burst shrinks, and waiting for more would never end.
		want := min(float64(o.MaxRequests)*0.75, float64(s.Client.Burst())*0.9)
		for s.Client.Tokens() < want {
			if err := sleepCtx(ctx, 200*time.Millisecond); err != nil {
				return nil, err
			}
		}
	}
	started := time.Now()

	r, err := s.check1(ctx, user, o)
	if err != nil {
		return nil, err
	}
	r.Gate.ElapsedMS = time.Since(started).Milliseconds()
	r.Measurement.ElapsedSec = time.Since(queued).Seconds()
	if r.Gate.Decision != GateUnknown {
		cs.mu.Lock()
		cs.cache[key] = cachedCheck{r: r, at: time.Now()}
		cs.mu.Unlock()
		s.saveReportAs(r, "check")
	}
	return r, nil
}

func (s *Scanner) check1(parent context.Context, user string, o CheckOptions) (*analysis.Report, error) {
	ctx, cancel := context.WithTimeout(parent, o.Deadline)
	defer cancel()
	ctx, retries := lastfm.WithRetryStats(ctx)
	c := s.Client
	var requests atomic.Int32
	// Wall time of each round, reported so slow Last.fm answers are visible.
	started := time.Now()
	var roundsMS []int64
	lastLap := started
	lap := func() {
		roundsMS = append(roundsMS, time.Since(lastLap).Milliseconds())
		lastLap = time.Now()
	}

	// Spend what the shared budget can give right now. With a queue of
	// sign-ups the bucket runs low, and each check reads fewer periods (never
	// fewer than 8) so the queue keeps moving; lifetime totals and the
	// densest windows still carry the evidence.
	periods, refine := o.Periods, o.Refine
	if avail := int(c.Tokens()); avail < o.MaxRequests/2 {
		periods = min(periods, max(8, avail-6))
		refine = 0
		if avail >= 16 {
			refine = 1
		}
	}

	// Round 1: profile and lifetime top tracks.
	var (
		info     *lastfm.UserInfo
		top      []lastfm.TopTrack
		distinct int64
		infoErr  error
		topErr   error
		wg       sync.WaitGroup
	)
	wg.Add(2)
	type topResult struct {
		tracks   []lastfm.TopTrack
		distinct int64
	}
	more := func() { requests.Add(1) }
	go func() {
		defer wg.Done()
		info, infoErr = hedged(ctx, o.HedgeAfter, func(ctx context.Context) (*lastfm.UserInfo, error) { return c.UserInfo(ctx, user) }, more)
	}()
	go func() {
		defer wg.Done()
		var tr topResult
		tr, topErr = hedged(ctx, o.HedgeAfter, func(ctx context.Context) (topResult, error) {
			t, d, err := c.TopTracks(ctx, user, "overall", o.TopTracks)
			return topResult{t, d}, err
		}, more)
		top, distinct = tr.tracks, tr.distinct
	}()
	wg.Wait()
	requests.Add(2)
	lap()
	if infoErr != nil {
		return nil, fmt.Errorf("user %q: %w", user, infoErr)
	}
	user = info.Name
	now := time.Now().Unix()
	since := info.Registered
	if since <= 0 || since > now {
		since = time.Date(2002, 1, 1, 0, 0, 0, 0, time.UTC).Unix() // Last.fm's first year
	}

	// Periods of whole months.
	months := lastfm.Months(since, now)
	group := int(math.Ceil(float64(len(months)) / float64(periods)))
	var units []*unit
	for i := 0; i < len(months); i += group {
		j := min(i+group, len(months)) - 1
		units = append(units, newUnit(months[i:j+1], since, now, true))
	}

	var (
		mu       sync.Mutex
		windows  []*window
		private  bool
		budgetOK = func() bool { return int(requests.Load()) < o.MaxRequests }
	)
	read := func(ctx context.Context, u *unit, page int) {
		if !budgetOK() {
			return
		}
		requests.Add(1)
		p, err := hedged(ctx, o.HedgeAfter, func(ctx context.Context) (*lastfm.RecentPage, error) {
			return c.RecentTracks(ctx, user, page, o.Window, u.from, u.to)
		}, func() { requests.Add(1) })
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			if lastfm.IsPrivate(err) {
				private = true
			}
			return
		}
		if page == 1 {
			u.total, u.ok = p.Total, true
		}
		if len(p.Scrobbles) > 0 {
			owner := u
			if u.parent != nil {
				owner = u.parent
			}
			owner.windows++
			windows = append(windows, &window{unit: owner, plays: p.Scrobbles})
		}
	}
	type job struct {
		u    *unit
		page int
	}
	round := func(ctx context.Context, jobs []job) {
		ch := make(chan job)
		var wg sync.WaitGroup
		for w := 0; w < min(o.Workers, len(jobs)); w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := range ch {
					read(ctx, j.u, j.page)
				}
			}()
		}
		for _, j := range jobs {
			ch <- j
		}
		close(ch)
		wg.Wait()
	}

	// Round 2: every period.
	var jobs []job
	for _, u := range units {
		jobs = append(jobs, job{u, 1})
	}
	round(ctx, jobs)
	lap()
	if private {
		return nil, fmt.Errorf("user %q: %w", user, &lastfm.APIError{Code: lastfm.ErrLoginRequired, Message: "recent listening is hidden"})
	}

	// Round 3, in one go: split the densest multi-month periods into months,
	// and read a window from the middle of the densest periods (their newest
	// plays alone might not show the rest). Skipped when round 2 was slow, so
	// the check still answers inside the deadline with what it has, and when
	// rounds 1–2 already prove the account suspect: on the big fake accounts
	// each request takes ~3 s and more evidence cannot change the decision.
	decided, uncertain := false, false
	if pre, err := s.assemble(info, top, topErr, distinct, units, append([]*unit(nil), units...), windows, since, now, int(requests.Load()), o); err == nil {
		decided = pre.Score.Verdict == "suspect" && pre.Gate.Complete
		// Some evidence but no verdict yet. Faking that comes in episodes
		// is caught or missed depending on which windows were read (the
		// second 100-account run flipped renatoakamur from review 56 to
		// pass 18 on a smaller sample), so read more before deciding.
		uncertain = !decided && pre.Score.Value >= 10
	}
	jobs = nil
	if !decided && time.Since(started) < o.Deadline*55/100 {
		hot := append([]*unit(nil), units...)
		sort.SliceStable(hot, func(a, b int) bool { return hot[a].perDay() > hot[b].perDay() })
		split := 0
		for _, u := range hot {
			if split >= refine || !u.ok || len(u.months) < 2 || u.total == 0 {
				continue
			}
			split++
			for _, m := range u.months {
				sub := newUnit([]lastfm.MonthCount{m}, since, now, false)
				u.refined = append(u.refined, sub)
				jobs = append(jobs, job{sub, 1})
			}
		}
		maxMiddles := 2
		if uncertain {
			maxMiddles += o.ExtraProbes
		}
		middles := 0
		for _, u := range hot {
			if middles == maxMiddles {
				break
			}
			// The newest plays before the period's midpoint: a page-1 query
			// bounded by time, because deep pages (page 600 of a busy
			// period) are slow on Last.fm.
			if u.ok && u.total > int64(2*o.Window) {
				mid := u.from + (u.to-u.from)/2
				jobs = append(jobs, job{&unit{label: u.label, from: u.from, to: mid, days: 1, parent: u}, 1})
				middles++
			}
		}
	}
	// Round 3 is optional: it gets until 80% of the deadline, and whatever
	// has not answered by then is left out rather than holding the decision.
	ctx3, cancel3 := context.WithDeadline(ctx, started.Add(o.Deadline*80/100))
	round(ctx3, jobs)
	cancel3()
	lap()

	var leaves []*unit
	for _, u := range units {
		if len(u.refined) > 0 {
			leaves = append(leaves, u.refined...)
		} else {
			leaves = append(leaves, u)
		}
	}

	cp := newCapture(info, top, topErr, distinct, units, windows, since, now, int(requests.Load()), roundsMS, decided)
	cp.Retries = retries.Counts()
	s.saveCapture(cp)
	return s.recheck(cp, o)
}

type window struct {
	unit  *unit
	plays []lastfm.RawScrobble
	stat  analysis.Window
}

func newUnit(ms []lastfm.MonthCount, since, now int64, isPeriod bool) *unit {
	from := max(ms[0].Start, since)
	to := min(ms[len(ms)-1].End, now)
	days := int(math.Ceil(float64(to-from+1) / 86400))
	return &unit{label: ms[0].Month, from: from, to: to, days: max(days, 1), months: ms, isPeriod: isPeriod}
}

func (u *unit) perDay() float64 {
	if !u.ok {
		return -1
	}
	return float64(u.total) / float64(u.days)
}

func (s *Scanner) assemble(info *lastfm.UserInfo, top []lastfm.TopTrack, topErr error, distinct int64,
	units, leaves []*unit, windows []*window, since, now int64, requests int, o CheckOptions) (*analysis.Report, error) {
	params := s.Opts.Params
	if params.Devices == 0 {
		params = analysis.DefaultParams()
	}

	// Song lengths from the top-tracks answer, matched by song.
	lengths := map[string]int{}
	for _, t := range top {
		if t.Seconds > 0 {
			lengths[analysis.SongKey(t.Artist, t.Title)] = t.Seconds
		}
	}
	durationsFor := func(h *scrobble.History) map[uint32]int {
		d := map[uint32]int{}
		for i, t := range h.Tracks {
			if sec, ok := lengths[analysis.SongKey(t.Artist, t.Title)]; ok {
				d[uint32(i)] = sec
			}
		}
		return d
	}

	// Each window on its own, for the evidence list and the weighted
	// estimate of how much of the account is double or impossible.
	all := scrobble.New(info.Name)
	var covered [][2]int64
	var samples []sampledWindow
	var wSum, wRemoved float64
	sampled := 0
	for _, w := range windows {
		h := scrobble.New(info.Name)
		for _, p := range w.plays {
			h.Add(p.TS, scrobble.Track{Artist: p.Artist, Title: p.Title, Album: p.Album})
		}
		h.Sort()
		wr := analysis.Analyze(analysis.Input{History: h, Scope: "quick", Durations: durationsFor(h), Params: params, KeepClasses: true})
		span := h.Last() - h.First()
		w.stat = analysis.Window{
			From: h.First(), To: h.Last(), Plays: len(h.Plays), SpanSec: span,
			Duplicates: wr.Integrity.Duplicates, Excess: wr.Integrity.ExcessPlays,
			PerHour: float64(len(h.Plays)) / (math.Max(float64(span), 60) / 3600),
		}
		// A split period's own window repeats its last month's; its months
		// stand for it.
		weight := float64(w.unit.total) / float64(max(w.unit.windows, 1))
		if len(w.unit.refined) > 0 {
			weight = 0
		}
		wSum += weight
		wRemoved += weight * float64(w.stat.Duplicates+w.stat.Excess) / float64(max(w.stat.Plays, 1))

		// Windows can overlap (a month's newest plays are also its period's
		// newest); add each play once.
		lo, hi := h.First(), h.Last()
		overlaps := false
		for _, c := range covered {
			if lo <= c[1] && hi >= c[0] {
				overlaps = true
				break
			}
		}
		if overlaps {
			continue
		}
		covered = append(covered, [2]int64{lo, hi})
		samples = append(samples, sampledWindow{h: h, classes: wr.Classes, s: summarize(h, wr.Classes)})
		for _, p := range h.Plays {
			all.Add(p.TS, h.Tracks[p.Track])
		}
		sampled += len(h.Plays)
	}
	all.Sort()

	// The history chart: periods, with months where a period was split.
	var chart []analysis.MonthStat
	periodsRead := 0
	for _, u := range units {
		if u.ok {
			periodsRead++
		}
		// Show the period's months only if every one of them came back;
		// otherwise the period's own exact count stands.
		parts := []*unit{u}
		if len(u.refined) > 0 {
			all := true
			for _, m := range u.refined {
				all = all && m.ok
			}
			if all {
				parts = u.refined
			}
		}
		for _, p := range parts {
			if !p.ok {
				continue
			}
			chart = append(chart, analysis.MonthStat{Month: p.label, Days: p.days, Plays: p.total})
		}
	}

	// Evidence from lifetime per-song totals.
	accountHours := float64(now-since) / 3600
	accountDays := math.Max(1, accountHours/24)
	gate := &analysis.Gate{
		Requests:       requests,
		Periods:        len(units),
		PeriodsRead:    periodsRead,
		SampledPlay:    sampled,
		Windows:        len(windows),
		DistinctTracks: distinct,
		AccountHours:   math.Round(accountHours),
	}
	var extra []analysis.Signal
	if topErr == nil && len(top) > 0 {
		var minHrs float64
		for i, t := range top {
			least := float64(params.MinPlaySeconds)
			if t.Seconds > 0 {
				least = math.Min(math.Max(float64(t.Seconds)/2, 15), 240)
			}
			h := float64(t.Plays) * least / 3600
			minHrs += h
			if i < 10 {
				gate.TopTracks = append(gate.TopTracks, analysis.TrackEvidence{
					Artist: t.Artist, Title: t.Title, Plays: t.Plays, Seconds: t.Seconds,
					MinHours: math.Round(h*10) / 10, PerDay: math.Round(float64(t.Plays)/accountDays*10) / 10,
				})
			}
		}
		gate.TopTracksMinHrs = math.Round(minHrs)
		if info.Playcount > 0 {
			gate.TopTrackShare = float64(top[0].Plays) / float64(info.Playcount)
		}
		if distinct > 0 {
			gate.PlaysPerTrack = float64(info.Playcount) / float64(distinct)
		}
		extra = append(extra, topTrackSignals(gate, top, len(top), accountHours, info.Playcount)...)
	}

	measurement := analysis.Measurement{
		Pages:    len(windows),
		Requests: requests,
		Note: fmt.Sprintf("Pre-import check: %d of %d periods read, %d windows of up to %d consecutive plays sampled across the history.",
			periodsRead, len(units), len(windows), o.Window),
	}
	r := analysis.Analyze(analysis.Input{
		Profile: analysis.Profile{
			Name: info.Name, RealName: info.RealName, URL: info.URL, Country: info.Country,
			Image: info.Image, Playcount: info.Playcount, Registered: info.Registered,
		},
		History:     all,
		Scope:       "quick",
		From:        since,
		To:          now,
		Months:      chart,
		Durations:   durationsFor(all),
		Measurement: measurement,
		Params:      params,
		Extra:       extra,
	})

	// The sample over-represents quiet periods (one window each); weigh each
	// window by the plays it stands for instead.
	lb := &r.Leaderboard
	if wSum > 0 {
		lb.RemovedShare = wRemoved / wSum
		lb.AdjustedScrobbles = int64(math.Round(float64(lb.RawScrobbles) * (1 - lb.RemovedShare)))
		lb.Estimated = true
		if lb.Action == "keep" || lb.Action == "adjust" {
			lb.Action = "keep"
			lb.Reason = "Clean; rank as is."
			if lb.RemovedShare >= 0.005 {
				lb.Action = "adjust"
				lb.Reason = fmt.Sprintf("Clean, but about %s of plays are double scrobbles or overlap; rank by the adjusted count.", analysis.Pct(lb.RemovedShare))
			}
		}
	}

	// Hotspots and the densest windows.
	sort.SliceStable(leaves, func(a, b int) bool { return leaves[a].perDay() > leaves[b].perDay() })
	for _, u := range leaves {
		if len(gate.Hotspots) == 5 || !u.ok || u.total == 0 {
			break
		}
		gate.Hotspots = append(gate.Hotspots, analysis.MonthStat{Month: u.label, Days: u.days, Plays: u.total, PerDay: u.perDay()})
	}
	sort.SliceStable(windows, func(a, b int) bool { return windows[a].stat.PerHour > windows[b].stat.PerHour })
	for _, w := range windows {
		if len(gate.Evidence) == 6 {
			break
		}
		gate.Evidence = append(gate.Evidence, w.stat)
	}

	gate.Samples = detailSamples(samples, 6)

	gate.Complete = periodsRead == len(units) && topErr == nil
	switch {
	case periodsRead*2 < len(units):
		gate.Decision = GateUnknown
		gate.Reason = fmt.Sprintf("Last.fm answered only %d of %d periods in time; try again shortly.", periodsRead, len(units))
	case r.Score.Verdict == "suspect":
		gate.Decision = GateBlock
		gate.Reason = "The history cannot be explained by one person listening: " + topReasons(r.Signals)
	case r.Score.Verdict == "review":
		gate.Decision = GateReview
		gate.Reason = "Some signals point to faked plays: " + topReasons(r.Signals)
	case r.Stats.MonthsOverLimit >= 2:
		// Nothing scripted, but a volume one person on one or two players
		// does not reach for months on end (xEspiix in the first 100-account
		// run: 9 periods over 600 a day, peak 824). A person decides.
		gate.Decision = GateReview
		gate.Reason = fmt.Sprintf("Nothing looks scripted, but %d periods averaged over %d plays a day (peak %s a day in %s): more than one person listens, or players ran for months.",
			r.Stats.MonthsOverLimit, params.DayLimit, analysis.Num(int64(math.Round(r.Stats.PeakMonth.PerDay))), r.Stats.PeakMonth.Month)
	case !gate.Complete && !missingFits(info.Playcount, units, topErr, params.DayLimit):
		gate.Decision = GateReview
		gate.Reason = fmt.Sprintf("Nothing suspicious in what was read, but only %d of %d periods answered in time.", periodsRead, len(units))
	case !gate.Complete:
		// The lifetime playcount minus the periods read is what the missing
		// ones hold; if that is a volume one person reaches, a period or two
		// that timed out is no reason to hold the account.
		gate.Decision = GatePass
		gate.Reason = fmt.Sprintf("Nothing suspicious in what was read. %d of %d periods did not answer in time, but the lifetime total leaves them a volume one person reaches.",
			len(units)-periodsRead, len(units))
	default:
		gate.Decision = GatePass
		gate.Reason = "Nothing in the lifetime totals or the sampled plays needs more than one person with up to two players."
	}
	r.Gate = gate
	return r, nil
}

// missingFits reports whether the periods that did not answer can hold
// only a human volume: the lifetime playcount minus the periods read, over
// the missing days, stays under dayLimit. It allows at most two missing
// periods, or a tenth of them, and needs the top tracks' evidence.
func missingFits(playcount int64, units []*unit, topErr error, dayLimit int) bool {
	if topErr != nil || playcount <= 0 {
		return false
	}
	var read int64
	missing, missingDays := 0, 0
	for _, u := range units {
		if u.ok {
			read += u.total
		} else {
			missing++
			missingDays += u.days
		}
	}
	if missing > max(2, len(units)/10) {
		return false
	}
	rest := max(playcount-read, 0)
	return float64(rest) <= float64(dayLimit)*float64(max(missingDays, 1))
}

// topTrackSignals turns lifetime per-song totals into evidence. Every
// scrobble needs some minimum listening (half the song, or 30 s when its
// length is unknown), so the top songs' totals put a floor under how many
// hours the account must have spent on them.
func topTrackSignals(g *analysis.Gate, top []lastfm.TopTrack, n int, accountHours float64, playcount int64) []analysis.Signal {
	var out []analysis.Signal
	if accountHours > 0 {
		share := g.TopTracksMinHrs / accountHours
		pts := 0
		switch {
		case share >= 1:
			pts = 35
		case share >= 0.5:
			pts = 25
		case share >= 0.25:
			pts = 12
		}
		if pts > 0 {
			out = append(out, analysis.Signal{
				ID: "top_tracks_time", Kind: "fake", Points: pts,
				Title: "More listening than the account has had time for",
				Detail: fmt.Sprintf("The top %d songs alone need at least %s hours of listening (%s of every hour since the account opened), crediting each play with only its minimum time",
					n, analysis.Num(int64(g.TopTracksMinHrs)), analysis.Pct(share)),
			})
		}
	}
	if playcount >= 20000 && g.TopTrackShare >= 0.05 {
		out = append(out, analysis.Signal{
			ID: "top_track_share", Kind: "pattern",
			Points: min(15, 5+int(math.Round(100*(g.TopTrackShare-0.05)))),
			Title:  "One song dominates the account",
			Detail: fmt.Sprintf("%s — %s is %s of all scrobbles (%s plays)", top[0].Artist, top[0].Title, analysis.Pct(g.TopTrackShare), analysis.Num(top[0].Plays)),
		})
	}
	return out
}

func topReasons(sig []analysis.Signal) string {
	var s string
	k := 0
	for _, x := range sig {
		if x.Points == 0 || k == 3 {
			continue
		}
		if k > 0 {
			s += "; "
		}
		s += x.Title
		k++
	}
	return s + "."
}

// hedged runs call and, each time delay passes with no answer, another copy
// of it (at most three in flight); the first success wins and the others are
// cancelled. extra is called when a copy is sent. A failure before the delay is returned as is: the
// client has already retried it.
func hedged[T any](ctx context.Context, delay time.Duration, call func(context.Context) (T, error), extra func()) (T, error) {
	if delay <= 0 {
		return call(ctx)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		v   T
		err error
	}
	ch := make(chan result, 2)
	run := func() {
		v, err := call(ctx)
		ch <- result{v, err}
	}
	go run()
	timer := time.NewTimer(delay)
	defer timer.Stop()
	launched, pending := 1, 1
	for {
		select {
		case r := <-ch:
			pending--
			if r.err == nil || pending == 0 {
				return r.v, r.err
			}
		case <-timer.C:
			if launched < 3 {
				launched, pending = launched+1, pending+1
				extra()
				go run()
				timer.Reset(delay)
			}
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
