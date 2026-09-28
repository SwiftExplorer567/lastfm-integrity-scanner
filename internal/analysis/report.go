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

type MonthStat struct {
	Month  string  `json:"month"`
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
