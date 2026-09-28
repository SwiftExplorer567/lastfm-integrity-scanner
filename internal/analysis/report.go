package analysis

import "time"

// ReportVersion changes when the JSON shape changes incompatibly.
const ReportVersion = 1

// Report is everything a scan found. It is what the JSON export is, and
// every other format is rendered from it.
type Report struct {
	Version     int         `json:"version"`
	GeneratedAt time.Time   `json:"generated_at"`
	User        Profile     `json:"user"`
	Measurement Measurement `json:"measurement"`
	Score       Score       `json:"score"`
	Signals     []Signal    `json:"signals"`
	Leaderboard Leaderboard `json:"leaderboard"`
	Integrity   Integrity   `json:"integrity"`
	Stats       Stats       `json:"stats"`
	Charts      Charts      `json:"charts"`
	Params      Params      `json:"params"`
	// Gate is set by the pre-import check: the admission decision and the
	// evidence gathered from Last.fm's aggregate endpoints.
	Gate *Gate `json:"gate,omitempty"`

	// Classes has one entry per analysed play when Input.KeepClasses is set.
	Classes []PlayClass `json:"-"`
}

type Profile struct {
	Name       string `json:"name"`
	RealName   string `json:"realname,omitempty"`
	URL        string `json:"url,omitempty"`
	Country    string `json:"country,omitempty"`
	Image      string `json:"image,omitempty"`
	Playcount  int64  `json:"playcount"`
	Registered int64  `json:"registered"`
}

type Measurement struct {
	// Scope is "full" when every scrobble was read, "sample" when only a
	// recent window was (Last.fm's monthly totals still cover all of it).
	Scope         string  `json:"scope"`
	From          int64   `json:"from"`
	To            int64   `json:"to"`
	PlaysAnalyzed int     `json:"plays_analyzed"`
	HistoryMonths int     `json:"history_months"`
	Pages         int     `json:"pages"`
	TotalPages    int     `json:"total_pages"`
	MissingPages  []int   `json:"missing_pages,omitempty"`
	FromCache     int     `json:"plays_from_cache"`
	Requests      int     `json:"requests,omitempty"`
	ElapsedSec    float64 `json:"elapsed_seconds"`
	Note          string  `json:"note,omitempty"`
}

type Score struct {
	Value   int    `json:"value"`
	Verdict string `json:"verdict"` // clean | review | suspect
	Label   string `json:"label"`
}

// Signal is one reason the score is what it is. Benign signals carry no
// points; they explain what was forgiven (double scrobbles, a second device).
type Signal struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Detail string `json:"detail"`
	Points int    `json:"points"`
	Kind   string `json:"kind"` // fake | volume | pattern | benign
}

// Leaderboard is the recommendation for ranking systems.
type Leaderboard struct {
	Action            string  `json:"action"` // keep | adjust | review | exclude
	Reason            string  `json:"reason"`
	RawScrobbles      int64   `json:"raw_scrobbles"`
	AdjustedScrobbles int64   `json:"adjusted_scrobbles"`
	RemovedShare      float64 `json:"removed_share"`
	Estimated         bool    `json:"estimated"` // true when extrapolated from a sample
}

// Integrity holds the play-level findings behind the signals.
type Integrity struct {
	PlaysAnalyzed int `json:"plays_analyzed"`

	// Same play recorded twice (two scrobbler apps, or two devices on one
	// account) within DupWindow. Benign; removed from the adjusted count.
	Duplicates        int `json:"duplicates"`
	DuplicateClusters int `json:"duplicate_clusters"`
	// EchoDuplicates are the part of Duplicates found as lone pairs of
	// different songs seconds apart (two scrobblers, mismatched metadata).
	EchoDuplicates int `json:"echo_duplicates"`
	// StutterCopies are the part of Duplicates that repeat one play within
	// Params.StutterSeconds, however many times (a stuttering scrobbler).
	StutterCopies int `json:"stutter_copies"`

	// Plays that fit only because a second player was running at the same
	// time (a forgotten YouTube tab while Spotify plays on the phone). Benign.
	SecondDevicePlays int `json:"second_device_plays"`

	// Plays that need more simultaneous players than Params.Devices allows,
	// even when each is credited with only its minimum listening time.
	ExcessPlays int     `json:"excess_plays"`
	ExcessShare float64 `json:"excess_share"`

	// Same song more than MaxCopies times with each copy within DupWindow of
	// the previous one: a loop no player produces.
	RapidLoopPlays int `json:"rapid_loop_plays"`

	SameSecondPlays    int     `json:"same_second_plays"`
	SameSecondShare    float64 `json:"same_second_share"`
	SameSecondInstants int     `json:"same_second_instants"`
	SameSecondTriples  int     `json:"same_second_triples"`

	UnderFifteen      int     `json:"under_fifteen"`
	UnderFifteenShare float64 `json:"under_fifteen_share"`

	LoopPlays int     `json:"loop_plays"`
	LoopShare float64 `json:"loop_share"`
	// FastLoopPlays repeat a song heard minutes before, under 30 s after the
	// previous play: a playlist cycled faster than anyone can listen.
	FastLoopPlays int     `json:"fast_loop_plays"`
	FastLoopShare float64 `json:"fast_loop_share"`

	// SkipRunPlays are in runs of four or more different songs, each under
	// Params.SkipSeconds after the previous. A whole album in album order is
	// how manual "scrobble this album" tools and some old clients look;
	// SkipRunMixed counts the steps between different artists and albums,
	// which only scrobbling software produces.
	SkipRuns     int     `json:"skip_runs"`
	SkipRunPlays int     `json:"skip_run_plays"`
	SkipRunShare float64 `json:"skip_run_share"`
	SkipRunMixed int     `json:"skip_run_mixed"`

	Bursts       int     `json:"bursts"`
	BurstPlays   int     `json:"burst_plays"`
	BurstsPer10k float64 `json:"bursts_per_10k"`
	BurstMax     int     `json:"burst_max"`

	// Most common exact spacing between two different songs. Software that
	// fakes plays tends to use one fixed interval; people do not.
	RegularGapSeconds int     `json:"regular_gap_seconds"`
	RegularGapShare   float64 `json:"regular_gap_share"`
	RegularGapSample  int     `json:"regular_gap_sample"`

	// Share of plays in the quietest six consecutive hours of the day. Someone
	// who sleeps leaves a few percent there; plays spread evenly leave 25%.
	QuietShare     float64 `json:"quiet_share"`
	QuietStartHour int     `json:"quiet_start_hour"`

	DurationsKnown int `json:"plays_with_known_duration"`
}

type Stats struct {
	LastfmScrobbles int64     `json:"lastfm_scrobbles"`
	AccountDays     int       `json:"account_days"`
	AvgPerDay       float64   `json:"avg_per_day"`
	BusiestDay      DayStat   `json:"busiest_day"`
	BusiestHour     HourStat  `json:"busiest_hour"`
	DaysWithPlays   int       `json:"days_with_plays"`
	DaysOverLimit   int       `json:"days_over_limit"`
	HoursOverLimit  int       `json:"hours_over_limit"`
	NoSleepDays     int       `json:"no_sleep_days"`
	MonthsOverLimit int       `json:"months_over_limit"`
	MonthsTotal     int       `json:"months_total"`
	PeakMonth       MonthStat `json:"peak_month"`
}

type DayStat struct {
	Date     string `json:"date"`
	Plays    int    `json:"plays"`
	Hours    int    `json:"hours"`     // distinct clock hours with plays
	PeakHour int    `json:"peak_hour"` // plays in the busiest clock hour
}

type HourStat struct {
	Start int64 `json:"start"`
	Plays int   `json:"plays"`
}

// MonthStat is one bar of the history chart: a calendar month, or a longer
// period starting at Month when Days is set.
type MonthStat struct {
	Month  string  `json:"month"`
	Days   int     `json:"days,omitempty"`
	Plays  int64   `json:"plays"`
	PerDay float64 `json:"per_day"`
}

type GapBucket struct {
	Label string  `json:"label"`
	Count int     `json:"count"`
	Share float64 `json:"share"`
}

type BurstTrack struct {
	TS     int64  `json:"ts"`
	Artist string `json:"artist"`
	Title  string `json:"title"`
}

type Burst struct {
	Start  int64        `json:"start"`
	End    int64        `json:"end"`
	Plays  int          `json:"plays"`
	Tracks []BurstTrack `json:"tracks"`
}

type RepeatTrack struct {
	Artist     string `json:"artist"`
	Title      string `json:"title"`
	Plays      int    `json:"plays"`
	RapidPlays int    `json:"rapid_plays"`
}

type Charts struct {
	Months      []MonthStat   `json:"months"`
	Days        []DayStat     `json:"days"`
	WeekHour    [7][24]int    `json:"week_hour"` // Monday first, UTC
	Gaps        []GapBucket   `json:"gaps"`
	BusiestDays []DayStat     `json:"busiest_days"`
	Bursts      []Burst       `json:"bursts"`
	TopRepeats  []RepeatTrack `json:"top_repeats"`
}

// Gate is the outcome of the pre-import check.
type Gate struct {
	// Decision is pass (import automatically), review (a moderator decides),
	// block (do not import) or unknown (Last.fm did not answer enough of the
	// check in time; retry later).
	Decision string `json:"decision"`
	Reason   string `json:"reason"`

	Requests    int     `json:"requests"`
	ElapsedMS   int64   `json:"elapsed_ms"`
	Complete    bool    `json:"complete"`
	Periods     int     `json:"periods"`
	PeriodsRead int     `json:"periods_read"`
	SampledPlay int     `json:"sampled_plays"`
	Windows     int     `json:"windows"`
	RoundsMS    []int64 `json:"rounds_ms"` // wall time of each request round
	// DecidedEarly: rounds 1–2 already proved the account suspect, so the
	// refining round was skipped.
	DecidedEarly bool `json:"decided_early,omitempty"`
	// Retries counts requests Last.fm answered with an error and that were
	// asked again, by reason (rate_limited, http_500, lastfm_error_8, …).
	Retries map[string]int `json:"retries,omitempty"`

	// Lifetime per-song totals (user.getTopTracks, period=overall).
	DistinctTracks  int64           `json:"distinct_tracks"`
	PlaysPerTrack   float64         `json:"plays_per_track"`
	TopTrackShare   float64         `json:"top_track_share"`
	TopTracksMinHrs float64         `json:"top_tracks_min_hours"`
	AccountHours    float64         `json:"account_hours"`
	TopTracks       []TrackEvidence `json:"top_tracks"`

	// The densest stretches of the history, at month resolution.
	Hotspots []MonthStat `json:"hotspots"`
	// Sampled windows of consecutive plays, densest first.
	Evidence []Window `json:"evidence"`
	// Samples describes every window read, in time order: what each play
	// was classified as, and for the most suspicious windows the activity
	// over the window and a play-by-play look at its densest stretch.
	Samples []Sample `json:"samples,omitempty"`
}

// Sample is one window of consecutive plays as the analysis saw it.
type Sample struct {
	From    int64          `json:"from"`
	To      int64          `json:"to"`
	Plays   int            `json:"plays"`
	PerHour float64        `json:"per_hour"`
	Counts  map[string]int `json:"counts"` // by PlayClass.Kind
	Rapid   int            `json:"rapid"`
	// Suspicion orders windows: the share of plays that are impossible or
	// rapid loops.
	Suspicion float64 `json:"suspicion"`
	// Activity: plays per BucketSec across the window (top windows only).
	BucketSec int   `json:"bucket_sec,omitempty"`
	Activity  []int `json:"activity,omitempty"`
	// Zoom: every play of the window's densest stretch (top windows only).
	ZoomFrom int64        `json:"zoom_from,omitempty"`
	ZoomTo   int64        `json:"zoom_to,omitempty"`
	Zoom     []SamplePlay `json:"zoom,omitempty"`
}

// SamplePlay is one play in a zoomed stretch.
type SamplePlay struct {
	TS       int64  `json:"ts"`
	Artist   string `json:"artist"`
	Title    string `json:"title"`
	Kind     string `json:"kind"`
	Lane     int    `json:"lane"`
	Occupies int    `json:"occupies"`
	Rapid    bool   `json:"rapid,omitempty"`
}

// TrackEvidence is one song's lifetime total and the least listening time
// it implies.
type TrackEvidence struct {
	Artist   string  `json:"artist"`
	Title    string  `json:"title"`
	Plays    int64   `json:"plays"`
	Seconds  int     `json:"seconds,omitempty"` // song length when Last.fm knows it
	MinHours float64 `json:"min_hours"`         // plays × least time a scrobble needs
	PerDay   float64 `json:"per_day"`           // plays per day since the account opened
}

// Window is a run of consecutive plays read from one place in the history.
type Window struct {
	From       int64   `json:"from"`
	To         int64   `json:"to"`
	Plays      int     `json:"plays"`
	SpanSec    int64   `json:"span_seconds"`
	Duplicates int     `json:"duplicates"`
	Excess     int     `json:"excess"`
	PerHour    float64 `json:"per_hour"`
}
