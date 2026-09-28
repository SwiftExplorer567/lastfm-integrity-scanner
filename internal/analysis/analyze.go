// Package analysis turns a scrobble history into an integrity report.
//
// The central question is not "are there overlapping plays" (honest people
// produce those: two scrobbler apps, a YouTube tab left running while the
// phone plays Spotify) but "can the plays be explained by a person with a
// normal setup". Each play is credited with the least time a player must have
// spent on it before Last.fm would accept it, and the plays are packed onto at
// most Params.Devices players. Whatever does not fit is excess: plays that
// needed a third, fourth, tenth player at once. Exact double scrobbles are
// removed first and never count against anyone.
package analysis

import (
	"math"
	"sort"
	"time"

	"github.com/swiftexplorer567/lastfm-integrity-scanner/internal/scrobble"
)

// Params are the physical assumptions behind the analysis.
type Params struct {
	// Devices is how many players may run at once on one account without
	// suspicion. 2 covers "phone plus a forgotten browser tab".
	Devices int `json:"devices"`
	// MinPlaySeconds is the least time a play occupies a player when its
	// duration is unknown. Last.fm only accepts tracks over 30 s played for
	// half their length (or 4 minutes), so real plays occupy at least ~15 s
	// and nearly always 30 s or more.
	MinPlaySeconds int `json:"min_play_seconds"`
	// DupWindowSeconds: copies of the same song this close together are one
	// play recorded twice.
	DupWindowSeconds int `json:"dup_window_seconds"`
	// MaxCopies is the most copies of one play that can come from honest
	// double scrobbling (one per scrobbler app). More is a loop.
	MaxCopies int `json:"max_copies"`
	// PairSeconds: two different songs this close, alone, are one play
	// recorded twice with mismatched metadata. 0 disables.
	PairSeconds int `json:"pair_seconds"`
	// LoopWindowSeconds: a song counts as repeated if it was played this
	// recently.
	LoopWindowSeconds int `json:"loop_window_seconds"`
	BurstWindowSecond int `json:"burst_window_seconds"`
	BurstMinPlays     int `json:"burst_min_plays"`
	// DayLimit is the most plays in one day a person can manage (600 is 24 h
	// of 2.4-minute songs with no sleep). HourLimit is the same per hour.
	DayLimit      int `json:"day_limit"`
	HourLimit     int `json:"hour_limit"`
	NoSleepHours  int `json:"no_sleep_hours"`
	LifetimeLimit int `json:"lifetime_limit"`
}

func DefaultParams() Params {
	return Params{
		Devices:           2,
		MinPlaySeconds:    30,
		DupWindowSeconds:  60,
		MaxCopies:         3,
		PairSeconds:       10,
		LoopWindowSeconds: 600,
		BurstWindowSecond: 60,
		BurstMinPlays:     5,
		DayLimit:          600,
		HourLimit:         60,
		NoSleepHours:      20,
		LifetimeLimit:     400,
	}
}

// Input is what Analyze needs.
type Input struct {
	Profile Profile
	History *scrobble.History
	// Scope is "full" or "sample"; see Measurement.Scope.
	Scope    string
	From, To int64
	// Months are Last.fm's own monthly totals. If nil they are derived from
	// History, which is only complete for a full scan.
	Months []MonthStat
	// Durations maps a track index in History.Tracks to its length in seconds.
	Durations   map[uint32]int
	Measurement Measurement
	Params      Params
	Now         time.Time
	// Extra signals found outside the play history (for example from
	// lifetime per-song totals). They count toward the score like any other.
	Extra []Signal
}

// Analyze computes the full report. It runs in O(n log n) for n plays and
// allocates a few bytes per play; millions of plays take well under a second.
func Analyze(in Input) *Report {
	p := in.Params
	if p.Devices == 0 {
		p = DefaultParams()
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	h := in.History
	plays := h.Plays
	n := len(plays)

	r := &Report{
		Version:     ReportVersion,
		GeneratedAt: now.UTC(),
		User:        in.Profile,
		Measurement: in.Measurement,
		Params:      p,
	}
	r.Measurement.Scope = in.Scope
	r.Measurement.From = in.From
	r.Measurement.To = in.To
	r.Measurement.PlaysAnalyzed = n
	r.Integrity.PlaysAnalyzed = n

	// Song identity per track, so metadata variants of one song match.
	keyIDs := make(map[string]uint32, len(h.Tracks))
	songOf := make([]uint32, len(h.Tracks))
	for i, t := range h.Tracks {
		k := SongKey(t.Artist, t.Title)
		id, ok := keyIDs[k]
		if !ok {
			id = uint32(len(keyIDs))
			keyIDs[k] = id
		}
		songOf[i] = id
	}
	nSongs := len(keyIDs)
	song := func(i int) uint32 { return songOf[plays[i].Track] }

	ig := &r.Integrity

	// 1. Double scrobbles and rapid loops. Chain copies of a song that are
	// each within DupWindow of the previous copy into clusters.
	const (
		stKept = iota
		stDuplicate
		stExcess
	)
	state := make([]uint8, n)
	rapid := make([]bool, n)
	{
		lastTS := make([]int64, nSongs)
		lastCluster := make([]int32, nSongs)
		for i := range lastCluster {
			lastCluster[i] = -1
		}
		cluster := make([]int32, n)
		var sizes []int32
		for i := 0; i < n; i++ {
			s := song(i)
			if c := lastCluster[s]; c >= 0 && plays[i].TS-lastTS[s] <= int64(p.DupWindowSeconds) {
				cluster[i] = c
				sizes[c]++
			} else {
				cluster[i] = int32(len(sizes))
				sizes = append(sizes, 1)
				lastCluster[s] = cluster[i]
			}
			lastTS[s] = plays[i].TS
		}
		seen := make([]int32, len(sizes))
		for i := 0; i < n; i++ {
			c := cluster[i]
			seen[c]++
			if seen[c] == 1 {
				continue
			}
			if int(sizes[c]) <= p.MaxCopies {
				state[i] = stDuplicate
				ig.Duplicates++
				if seen[c] == 2 {
					ig.DuplicateClusters++
				}
			} else {
				rapid[i] = true
				ig.RapidLoopPlays++
			}
		}
	}

	// 1b. Echo pairs: two plays within PairSeconds of each other with nothing
	// else that close on either side. One player cannot scrobble two songs
	// that close (Last.fm needs at least ~15 s of listening), so this is one
	// play recorded twice by two scrobblers whose metadata differs too much
	// to match ("アーティスト" vs "Artist", a different title format). Real runs
	// showed honest accounts with thousands of these. A bot's rapid plays come
	// in chains of three or more and are left alone.
	if p.PairSeconds > 0 {
		var kept []int
		for i := 0; i < n; i++ {
			if state[i] == stKept {
				kept = append(kept, i)
			}
		}
		tight := func(a, b int) bool {
			return a >= 0 && b < len(kept) && plays[kept[b]].TS-plays[kept[a]].TS <= int64(p.PairSeconds)
		}
		for k := 1; k < len(kept); k++ {
			if tight(k-1, k) && !tight(k-2, k-1) && !tight(k, k+1) {
				state[kept[k]] = stDuplicate
				ig.Duplicates++
				ig.EchoDuplicates++
				k++ // the pair is consumed; the next play starts fresh
			}
		}
	}

	// 2. Pack the remaining plays onto Devices players. A play occupies its
	// player for half its duration (capped at 4 minutes, Last.fm's own rule)
	// or MinPlaySeconds when the duration is unknown. Plays with no free
	// player are excess.
	{
		busy := make([]int64, p.Devices)
		for i := 0; i < n; i++ {
			if state[i] != stKept {
				continue
			}
			occ := int64(p.MinPlaySeconds)
			if d, ok := in.Durations[plays[i].Track]; ok && d > 0 {
				ig.DurationsKnown++
				occ = int64(clamp(d/2, 15, 240))
			}
			t := plays[i].TS
			lane := -1
			for l, b := range busy {
				if b <= t {
					lane = l
					break
				}
			}
			if lane < 0 {
				state[i] = stExcess
				ig.ExcessPlays++
				continue
			}
			busy[lane] = t + occ
			if lane > 0 {
				ig.SecondDevicePlays++
			}
		}
		ig.ExcessShare = ratio(ig.ExcessPlays, n)
	}

	// 3. Spacing, loops and fixed-interval patterns on deduplicated plays.
	{
		lastTS := make([]int64, nSongs)
		for i := range lastTS {
			lastTS[i] = math.MinInt64 / 2
		}
		gapHist := map[int64]int{}
		prev := -1
		kept := 0
		for i := 0; i < n; i++ {
			if state[i] == stDuplicate {
				continue
			}
			kept++
			s := song(i)
			t := plays[i].TS
			if t-lastTS[s] <= int64(p.LoopWindowSeconds) {
				ig.LoopPlays++
			}
			lastTS[s] = t
			if prev >= 0 {
				gap := t - plays[prev].TS
				if gap < 15 {
					ig.UnderFifteen++
				}
				if song(prev) != s && gap >= 15 && gap <= 600 {
					gapHist[gap]++
					ig.RegularGapSample++
				}
			}
			prev = i
		}
		ig.LoopShare = ratio(ig.LoopPlays, kept)
		ig.UnderFifteenShare = ratio(ig.UnderFifteen, kept-1)
		var mode int64
		best := 0
		for g, c := range gapHist {
			if c > best || (c == best && g < mode) {
				mode, best = g, c
			}
		}
		ig.RegularGapSeconds = int(mode)
		ig.RegularGapShare = ratio(best, ig.RegularGapSample)
	}

	// 4. Different songs at the same second, among plays that are not
	// duplicates of another.
	for i := 0; i < n; {
		j := i
		for j < n && plays[j].TS == plays[i].TS {
			j++
		}
		if j-i > 1 {
			distinct := map[uint32]struct{}{}
			count := 0
			for k := i; k < j; k++ {
				if state[k] != stDuplicate {
					distinct[song(k)] = struct{}{}
					count++
				}
			}
			if len(distinct) > 1 {
				ig.SameSecondInstants++
				ig.SameSecondPlays += count
				if len(distinct) > 2 {
					ig.SameSecondTriples++
				}
			}
		}
		i = j
	}
	ig.SameSecondShare = ratio(ig.SameSecondPlays, n)

	// 5. Bursts: BurstMinPlays or more plays inside one BurstWindow, taken
	// greedily so they do not overlap.
	var bursts []Burst
	for i := 0; i < n; {
		j := i
		for j+1 < n && plays[j+1].TS-plays[i].TS < int64(p.BurstWindowSecond) {
			j++
		}
		if cnt := j - i + 1; cnt >= p.BurstMinPlays {
			bursts = append(bursts, Burst{Start: plays[i].TS, End: plays[j].TS, Plays: cnt})
			ig.BurstPlays += cnt
			if cnt > ig.BurstMax {
				ig.BurstMax = cnt
			}
			i = j + 1
			continue
		}
		i++
	}
	ig.Bursts = len(bursts)
	if n > 0 {
		ig.BurstsPer10k = float64(ig.Bursts) * 10000 / float64(n)
	}
	sort.SliceStable(bursts, func(a, b int) bool {
		if bursts[a].Plays != bursts[b].Plays {
			return bursts[a].Plays > bursts[b].Plays
		}
		return bursts[a].End-bursts[a].Start < bursts[b].End-bursts[b].Start
	})
	if len(bursts) > 5 {
		bursts = bursts[:5]
	}
	for bi := range bursts {
		b := &bursts[bi]
		lo := sort.Search(n, func(i int) bool { return plays[i].TS >= b.Start })
		hi := lo + b.Plays - 1
		for k := hi; k >= lo && len(b.Tracks) < 8; k-- {
			t := h.Tracks[plays[k].Track]
			b.Tracks = append(b.Tracks, BurstTrack{TS: plays[k].TS, Artist: t.Artist, Title: t.Title})
		}
	}
	r.Charts.Bursts = bursts

	// 6. Calendar: days, clock hours, week × hour, time of day.
	st := &r.Stats
	var hourOfDay [24]int
	{
		var (
			day      = int64(math.MinInt64)
			cur      DayStat
			hourSet  uint32
			hourKey  = int64(math.MinInt64)
			hourCnt  int
			dayPeak  int
			flushDay = func() {
				if cur.Plays == 0 {
					return
				}
				cur.Hours = popcount(hourSet)
				cur.PeakHour = dayPeak
				r.Charts.Days = append(r.Charts.Days, cur)
			}
		)
		flushHour := func() {
			if hourCnt == 0 {
				return
			}
			if hourCnt > p.HourLimit {
				st.HoursOverLimit++
			}
			if hourCnt > st.BusiestHour.Plays {
				st.BusiestHour = HourStat{Start: hourKey * 3600, Plays: hourCnt}
			}
			if hourCnt > dayPeak {
				dayPeak = hourCnt
			}
		}
		for i := 0; i < n; i++ {
			t := plays[i].TS
			hk := floorDiv(t, 3600)
			if hk != hourKey {
				flushHour()
				hourKey, hourCnt = hk, 0
			}
			d := floorDiv(t, 86400)
			if d != day {
				flushDay()
				day = d
				cur = DayStat{Date: time.Unix(d*86400, 0).UTC().Format("2006-01-02")}
				hourSet, dayPeak = 0, 0
			}
			hod := int(hk - d*24)
			hourSet |= 1 << hod
			hourCnt++
			cur.Plays++
			hourOfDay[hod]++
			wd := (int(time.Unix(t, 0).UTC().Weekday()) + 6) % 7
			r.Charts.WeekHour[wd][hod]++
		}
		flushHour()
		flushDay()
	}
	for _, d := range r.Charts.Days {
		st.DaysWithPlays++
		if d.Plays > p.DayLimit {
			st.DaysOverLimit++
		}
		if d.Hours >= p.NoSleepHours {
			st.NoSleepDays++
		}
		if d.Plays > st.BusiestDay.Plays {
			st.BusiestDay = d
		}
	}
	busiest := append([]DayStat(nil), r.Charts.Days...)
	sort.SliceStable(busiest, func(a, b int) bool { return busiest[a].Plays > busiest[b].Plays })
	if len(busiest) > 10 {
		busiest = busiest[:10]
	}
	r.Charts.BusiestDays = busiest

	// Quietest six consecutive hours of the day.
	{
		best, start := math.MaxInt, 0
		for s := 0; s < 24; s++ {
			sum := 0
			for k := 0; k < 6; k++ {
				sum += hourOfDay[(s+k)%24]
			}
			if sum < best {
				best, start = sum, s
			}
		}
		ig.QuietShare = ratio(best, n)
		ig.QuietStartHour = start
	}

	// 7. Gaps between consecutive plays as recorded.
	{
		edges := []struct {
			label string
			max   int64
		}{{"0s", 0}, {"1–4s", 4}, {"5–14s", 14}, {"15–29s", 29}, {"30–59s", 59}, {"1–2 min", 119}, {"2 min+", math.MaxInt64}}
		counts := make([]int, len(edges))
		for i := 1; i < n; i++ {
			g := plays[i].TS - plays[i-1].TS
			for k, e := range edges {
				if g <= e.max {
					counts[k]++
					break
				}
			}
		}
		for k, e := range edges {
			r.Charts.Gaps = append(r.Charts.Gaps, GapBucket{Label: e.label, Count: counts[k], Share: ratio(counts[k], n-1)})
		}
	}

	// 8. Most repeated songs.
	{
		type agg struct {
			track       uint32
			plays, fast int
		}
		bySong := make([]agg, nSongs)
		for i := 0; i < n; i++ {
			a := &bySong[song(i)]
			if a.plays == 0 {
				a.track = plays[i].Track
			}
			a.plays++
			if rapid[i] || state[i] == stExcess {
				a.fast++
			}
		}
		sort.Slice(bySong, func(a, b int) bool {
			if bySong[a].fast != bySong[b].fast {
				return bySong[a].fast > bySong[b].fast
			}
			return bySong[a].plays > bySong[b].plays
		})
		for _, a := range bySong {
			if len(r.Charts.TopRepeats) == 5 || a.plays == 0 {
				break
			}
			t := h.Tracks[a.track]
			r.Charts.TopRepeats = append(r.Charts.TopRepeats, RepeatTrack{Artist: t.Artist, Title: t.Title, Plays: a.plays, RapidPlays: a.fast})
		}
	}

	// 9. Months: Last.fm's totals when we have them, else our own.
	months := in.Months
	if months == nil {
		months = monthsFromDays(r.Charts.Days)
	}
	for i := range months {
		m := &months[i]
		days := m.Days
		if days <= 0 {
			days = daysInMonth(m.Month, now)
		}
		if days > 0 {
			m.PerDay = float64(m.Plays) / float64(days)
		}
		if m.PerDay > float64(p.DayLimit) {
			st.MonthsOverLimit++
		}
		if m.PerDay > st.PeakMonth.PerDay {
			st.PeakMonth = *m
		}
	}
	st.MonthsTotal = len(months)
	r.Charts.Months = months
	r.Measurement.HistoryMonths = len(months)

	// 10. Lifetime.
	st.LastfmScrobbles = in.Profile.Playcount
	if st.LastfmScrobbles == 0 {
		st.LastfmScrobbles = int64(n)
	}
	since := in.Profile.Registered
	if since <= 0 && n > 0 {
		since = plays[0].TS
	}
	if since > 0 {
		st.AccountDays = int(math.Ceil(float64(now.Unix()-since) / 86400))
		if st.AccountDays < 1 {
			st.AccountDays = 1
		}
		st.AvgPerDay = float64(st.LastfmScrobbles) / float64(st.AccountDays)
	}

	score(r, in.Extra)
	return r
}

func monthsFromDays(days []DayStat) []MonthStat {
	var out []MonthStat
	for _, d := range days {
		m := d.Date[:7]
		if len(out) == 0 || out[len(out)-1].Month != m {
			out = append(out, MonthStat{Month: m})
		}
		out[len(out)-1].Plays += int64(d.Plays)
	}
	return out
}

// daysInMonth counts the days of month "2006-01", only up to now for the
// current month.
func daysInMonth(month string, now time.Time) int {
	t, err := time.Parse("2006-01", month)
	if err != nil {
		return 0
	}
	end := t.AddDate(0, 1, 0)
	if now.Before(end) {
		d := int(math.Ceil(now.Sub(t).Hours() / 24))
		if d < 1 {
			d = 1
		}
		return d
	}
	return int(end.Sub(t).Hours() / 24)
}

func ratio(a, b int) float64 {
	if b <= 0 {
		return 0
	}
	return float64(a) / float64(b)
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

func popcount(x uint32) int {
	c := 0
	for x != 0 {
		x &= x - 1
		c++
	}
	return c
}
