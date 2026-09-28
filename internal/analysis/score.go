package analysis

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// Verdict thresholds on the 0–100 score.
const (
	ReviewAt  = 35
	SuspectAt = 65
)

// score turns the findings into signals, a score and a leaderboard action.
//
// Points are weighted by intent. Things honest setups produce (a second
// scrobbler, a second device, a player left running overnight) carry little
// or nothing; things only software produces (plays that need a third player,
// the same song ten times a minute, one fixed interval between songs) carry
// the most. Volume that no single person can reach sits in between.
func score(r *Report, extra []Signal) {
	ig, st, p := &r.Integrity, &r.Stats, r.Params
	n := ig.PlaysAnalyzed
	// A quick check reads scattered windows of plays, not whole days, so
	// signals that need complete days or a full clock are left out.
	windowed := r.Measurement.Scope == "quick"
	sig := append([]Signal(nil), extra...)
	add := func(id, kind, title, detail string, pts int) {
		if pts > 0 || kind == "benign" {
			sig = append(sig, Signal{ID: id, Kind: kind, Title: title, Detail: detail, Points: pts})
		}
	}

	// Physically impossible plays.
	if ig.ExcessPlays >= 20 {
		add("excess_plays", "fake", "Plays no two players could produce",
			fmt.Sprintf("%s plays (%s) needed a third player running at once, even crediting each with only its minimum listening time",
				num(ig.ExcessPlays), pct(ig.ExcessShare)),
			capInt(round(150*ig.ExcessShare), 35))
	}
	if ig.RapidLoopPlays >= 10 {
		share := ratio(ig.RapidLoopPlays, n)
		add("rapid_loops", "fake", "The same song, seconds apart, over and over",
			fmt.Sprintf("%s plays (%s) repeated a song within %ds of the previous copy, more than %d times in a row",
				num(ig.RapidLoopPlays), pct(share), p.DupWindowSeconds, p.MaxCopies),
			capInt(round(200*share), 20))
	}
	if ig.Bursts >= 3 && ig.BurstsPer10k >= 5 {
		add("bursts", "fake", "Bursts of plays seconds apart",
			fmt.Sprintf("%s times %d or more plays landed inside a minute (%.0f per 10,000 plays); the most was %d",
				num(ig.Bursts), p.BurstMinPlays, ig.BurstsPer10k, ig.BurstMax),
			capInt(4+round(ig.BurstsPer10k/10), 18))
	}
	if ig.UnderFifteenShare >= 0.02 && ig.UnderFifteen >= 50 {
		add("under_fifteen", "fake", "Plays under fifteen seconds apart",
			fmt.Sprintf("%s of plays came less than fifteen seconds after the one before, after removing double scrobbles", pct(ig.UnderFifteenShare)),
			capInt(round(50*ig.UnderFifteenShare), 15))
	}
	// Calibrated on 28 labelled real accounts. Runs of different songs from
	// different artists seconds apart: two fakes 3.2% and 3.8% of plays,
	// sixteen honest accounts at most 0.05%.
	if mixed := ratio(ig.SkipRunMixed, n); mixed >= 0.003 && ig.SkipRunMixed >= 20 {
		add("mixed_skips", "fake", "Songs from different artists seconds apart",
			fmt.Sprintf("%s times a play came under %ds after a song by another artist from another album, in runs of four or more; no player scrobbles songs that fast",
				num(ig.SkipRunMixed), p.SkipSeconds),
			capInt(round(1000*mixed), 35))
	}
	// One album in order, seconds apart: a manual "scrobble this album" or an
	// old client scrobbling skipped tracks. Honest accounts reach 1%.
	if album := ratio(ig.SkipRunPlays-ig.SkipRunMixed, n); album >= 0.02 {
		add("album_skips", "pattern", "Whole albums scrobbled in seconds",
			fmt.Sprintf("%s of plays came in runs of four or more songs under %ds apart, mostly one album in order", pct(album), p.SkipSeconds),
			capInt(round(150*(album-0.02)), 8))
	}
	// Two fakes 2.2% and 12%, honest accounts at most 0.4%.
	if ig.FastLoopShare >= 0.005 && ig.FastLoopPlays >= 20 {
		add("fast_loops", "fake", "A playlist cycled faster than it plays",
			fmt.Sprintf("%s plays (%s) repeated a song heard minutes earlier, under 30 s after the play before",
				num(ig.FastLoopPlays), pct(ig.FastLoopShare)),
			capInt(round(1500*(ig.FastLoopShare-0.005)), 35))
	}
	// Stutter copies are removed and forgiven, but honest accounts stay
	// under 9%; from 15% (a labelled fake had 19%) a person should look.
	if stutter := ratio(ig.StutterCopies, n); stutter > 0.12 {
		pts := capInt(round(800*(stutter-0.12)), 25)
		if stutter >= 0.15 {
			pts = 35
		}
		add("stutter", "volume", "One play recorded many times",
			fmt.Sprintf("%s plays (%s) were copies of the play a second before; they are removed from the adjusted count, but honest accounts stay under 9%%",
				num(ig.StutterCopies), pct(stutter)),
			pts)
	}
	// Honest accounts reach about 1% here too (offline caches flushed in one
	// batch: HasanJWS measured 0.96% and 1.0% on two runs), so points rise
	// gradually from 1% instead of jumping in at a threshold.
	if ig.SameSecondShare > 0.01 && ig.SameSecondPlays >= 30 {
		add("same_second", "fake", "Different songs at the same second",
			fmt.Sprintf("%s of plays share their second with another song (%s instants, %s with three or more)",
				pct(ig.SameSecondShare), num(ig.SameSecondInstants), num(ig.SameSecondTriples)),
			capInt(round(400*(ig.SameSecondShare-0.01)), 12))
	}
	// Real listening puts under 2% of gaps between different songs on any one
	// exact second (HasanJWS 0.9–1.0%, even ChAelitaNicole 1.7%); a script
	// with a fixed interval puts most of them there.
	if ig.RegularGapSample >= 200 && ig.RegularGapShare >= 0.15 {
		// 90 real accounts: median 0.9%, p90 1.5%, max 6%; a radio station
		// account 25%, a 31-second script 67%.
		pts := capInt(round(100*(ig.RegularGapShare-0.10)), 20)
		if ig.RegularGapShare >= 0.2 && ig.RegularGapSample >= 500 {
			pts = 35
		}
		add("fixed_interval", "fake", "Songs spaced by one fixed interval",
			fmt.Sprintf("%s of gaps between different songs were exactly %ds; real song lengths vary", pct(ig.RegularGapShare), ig.RegularGapSeconds),
			pts)
	}

	// Volume no single person reaches.
	if st.AvgPerDay > float64(p.LifetimeLimit) {
		pts := 20
		if st.AvgPerDay > float64(p.DayLimit) {
			pts = 35
		}
		add("lifetime_average", "volume", "Impossible lifetime average",
			fmt.Sprintf("%s scrobbles a day, every day, since the account opened", num(round(st.AvgPerDay))), pts)
	}
	if st.DaysOverLimit > 0 {
		share := ratio(st.DaysOverLimit, st.DaysWithPlays)
		add("days_over_limit", "volume", "Days no one could listen to",
			fmt.Sprintf("%s days over %d plays; the busiest had %s", num(st.DaysOverLimit), p.DayLimit, num(st.BusiestDay.Plays)),
			capInt(6+round(60*share), 30))
	}
	if st.MonthsOverLimit > 0 {
		unit := "months"
		for _, m := range r.Charts.Months {
			if m.Days > 31 {
				unit = "periods"
				break
			}
		}
		add("months_over_limit", "volume", "Months no one could listen to",
			fmt.Sprintf("%d of %d %s averaged over %d plays a day; %s averaged %s",
				st.MonthsOverLimit, st.MonthsTotal, unit, p.DayLimit, st.PeakMonth.Month, num(int(st.PeakMonth.PerDay))),
			capInt(10+5*(st.MonthsOverLimit-1), 25))
	}
	if st.HoursOverLimit >= 3 {
		add("hours_over_limit", "volume", "Hours faster than one song a minute",
			fmt.Sprintf("%s hours over %d plays; the busiest had %d", num(st.HoursOverLimit), p.HourLimit, st.BusiestHour.Plays),
			capInt(4+st.HoursOverLimit/5, 20))
	}

	// Patterns that fit software better than people.
	if ig.LoopShare >= 0.4 && n >= 500 {
		add("loop_share", "pattern", "The same songs on a loop",
			fmt.Sprintf("%s of plays repeated a song heard in the %d minutes before", pct(ig.LoopShare), p.LoopWindowSeconds/60),
			capInt(6+round(40*(ig.LoopShare-0.4)), 18))
	}
	if st.NoSleepDays >= 3 && !windowed {
		share := ratio(st.NoSleepDays, st.DaysWithPlays)
		add("no_sleep_days", "pattern", "Days with no time to sleep",
			fmt.Sprintf("%s days with plays in %d or more different hours (%s of active days)", num(st.NoSleepDays), p.NoSleepHours, pct(share)),
			capInt(3+round(100*share), 15))
	}
	if ig.QuietShare >= 0.15 && n >= 2000 && !windowed {
		add("round_the_clock", "pattern", "No quiet hours",
			fmt.Sprintf("The quietest six hours of the day (from %02d:00 UTC) still hold %s of plays; people who sleep leave a few percent there",
				ig.QuietStartHour, pct(ig.QuietShare)),
			capInt(round(100*(ig.QuietShare-0.12)), 12))
	}

	// What was forgiven.
	if ig.Duplicates > 0 {
		add("double_scrobbles", "benign", "Double scrobbles removed",
			fmt.Sprintf("%s plays were a copy of a play seconds earlier (two scrobbler apps or devices%s); they are dropped from the adjusted count but do not raise the score",
				num(ig.Duplicates), stutterNote(ig.StutterCopies)), 0)
	}
	if ig.SecondDevicePlays > 0 {
		add("second_device", "benign", "A second player at the same time",
			fmt.Sprintf("%s plays overlap another play and fit only with a second player running (a browser tab left open, a phone and a PC); they are accepted",
				num(ig.SecondDevicePlays)), 0)
	}

	sort.SliceStable(sig, func(a, b int) bool { return sig[a].Points > sig[b].Points })
	total := 0
	for _, s := range sig {
		total += s.Points
	}
	if total > 100 {
		total = 100
	}
	r.Signals = sig
	r.Score.Value = total
	switch {
	case total >= SuspectAt:
		r.Score.Verdict, r.Score.Label = "suspect", "Suspect"
	case total >= ReviewAt:
		r.Score.Verdict, r.Score.Label = "review", "Needs review"
	default:
		r.Score.Verdict, r.Score.Label = "clean", "Clean"
	}
	leaderboard(r)
}

func leaderboard(r *Report) {
	ig := &r.Integrity
	lb := &r.Leaderboard
	lb.RawScrobbles = r.Stats.LastfmScrobbles
	removed := ig.Duplicates + ig.ExcessPlays
	lb.RemovedShare = ratio(removed, ig.PlaysAnalyzed)
	if r.Measurement.Scope == "full" && int64(ig.PlaysAnalyzed) >= lb.RawScrobbles*98/100 {
		lb.AdjustedScrobbles = lb.RawScrobbles - int64(removed)
	} else {
		lb.AdjustedScrobbles = int64(math.Round(float64(lb.RawScrobbles) * (1 - lb.RemovedShare)))
		lb.Estimated = true
	}
	if lb.AdjustedScrobbles < 0 {
		lb.AdjustedScrobbles = 0
	}
	switch {
	case r.Score.Verdict == "suspect":
		lb.Action = "exclude"
		lb.Reason = "Score is in the suspect range; the history cannot be explained by one person listening."
	case r.Score.Verdict == "review":
		lb.Action = "review"
		lb.Reason = "Some signals point to faked plays; a moderator should look before ranking this account."
	case lb.RemovedShare >= 0.005:
		lb.Action = "adjust"
		lb.Reason = fmt.Sprintf("Clean, but %s of plays are double scrobbles or overlap; rank by the adjusted count.", pct(lb.RemovedShare))
	default:
		lb.Action = "keep"
		lb.Reason = "Clean; rank as is."
	}
}

func stutterNote(n int) string {
	if n == 0 {
		return ""
	}
	return ", or " + num(n) + " copies of one play a second apart from a stuttering scrobbler"
}

func round(f float64) int { return int(math.Round(f)) }

func capInt(v, hi int) int {
	if v > hi {
		return hi
	}
	if v < 0 {
		return 0
	}
	return v
}

func pct(f float64) string {
	switch {
	case f == 0:
		return "0%"
	case f < 0.001:
		return "<0.1%"
	case f < 0.1:
		return fmt.Sprintf("%.1f%%", f*100)
	}
	return fmt.Sprintf("%.0f%%", f*100)
}

// num formats an integer with thousands separators.
func num[T ~int | ~int64](v T) string {
	s := fmt.Sprint(int64(v))
	neg := false
	if s[0] == '-' {
		neg, s = true, s[1:]
	}
	var out []byte
	for i := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, s[i])
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}

// Num, Pct and Date are exported for the renderers so every format prints
// numbers the same way.
func Num[T ~int | ~int64](v T) string { return num(v) }
func Pct(f float64) string            { return pct(f) }
func Date(ts int64) string            { return time.Unix(ts, 0).UTC().Format("2006-01-02") }
