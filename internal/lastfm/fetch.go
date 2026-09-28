package lastfm

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/swiftexplorer567/lastfm-integrity-scanner/internal/scrobble"
)

// PageSize is the largest page user.getRecentTracks serves.
const PageSize = 200

// Progress is called as work completes. It may be called from several
// goroutines at once.
type Progress func(stage string, done, total int)

func (p Progress) report(stage string, done, total int) {
	if p != nil {
		p(stage, done, total)
	}
}

// FetchOptions controls a bulk history download.
type FetchOptions struct {
	// Workers is how many pages are requested in parallel. The client's rate
	// limiter still caps the overall request rate; parallelism hides latency.
	Workers int
	// MaxPages stops after this many pages, newest first. 0 means no limit.
	MaxPages int
}

// FetchResult describes what a download actually covered.
type FetchResult struct {
	Pages        int     `json:"pages"`
	TotalPages   int     `json:"total_pages"`
	Total        int64   `json:"total"`
	MissingPages []int   `json:"missing_pages,omitempty"`
	Truncated    bool    `json:"truncated"`
	OldestTS     int64   `json:"oldest_ts"`
	Elapsed      float64 `json:"elapsed_seconds"`
}

// FetchHistory downloads every scrobble of user with from <= TS <= to into h.
// Pinning "to" keeps page boundaries stable while new scrobbles arrive during
// a long download. Pages that still fail after the client's retries get one
// more sequential pass at the end; any left are reported, not fatal.
func (c *Client) FetchHistory(ctx context.Context, h *scrobble.History, from, to int64, opt FetchOptions, progress Progress) (*FetchResult, error) {
	start := time.Now()
	if opt.Workers <= 0 {
		opt.Workers = 8
	}
	first, err := c.RecentTracks(ctx, h.User, 1, PageSize, from, to)
	if err != nil {
		return nil, err
	}
	res := &FetchResult{TotalPages: first.TotalPages, Total: first.Total}
	want := first.TotalPages
	if opt.MaxPages > 0 && want > opt.MaxPages {
		want = opt.MaxPages
		res.Truncated = true
	}

	pages := make(chan *RecentPage, opt.Workers*2)
	var (
		mu     sync.Mutex
		failed []int
		done   = 1
	)
	progress.report("history", done, want)

	// A single goroutine owns h, so interning needs no lock.
	oldest := int64(0)
	collected := make(chan struct{})
	go func() {
		defer close(collected)
		for p := range pages {
			for _, s := range p.Scrobbles {
				h.Add(s.TS, scrobble.Track{Artist: s.Artist, Title: s.Title, Album: s.Album})
				if oldest == 0 || s.TS < oldest {
					oldest = s.TS
				}
			}
		}
	}()
	pages <- first

	fetch := func(n int) bool {
		p, err := c.RecentTracks(ctx, h.User, n, PageSize, from, to)
		if err != nil {
			return false
		}
		pages <- p
		mu.Lock()
		done++
		d := done
		mu.Unlock()
		progress.report("history", d, want)
		return true
	}

	jobs := make(chan int)
	var wg sync.WaitGroup
	for i := 0; i < opt.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := range jobs {
				if !fetch(n) && ctx.Err() == nil {
					mu.Lock()
					failed = append(failed, n)
					mu.Unlock()
				}
			}
		}()
	}
feed:
	for n := 2; n <= want; n++ {
		select {
		case jobs <- n:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()

	if ctx.Err() == nil {
		sort.Ints(failed)
		for _, n := range failed {
			if !fetch(n) {
				res.MissingPages = append(res.MissingPages, n)
			}
		}
	}
	close(pages)
	<-collected
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	h.Sort()
	res.Pages = want - len(res.MissingPages)
	res.OldestTS = oldest
	res.Elapsed = time.Since(start).Seconds()
	return res, nil
}

// MonthCount is Last.fm's own scrobble total for one calendar month (UTC).
type MonthCount struct {
	Month string `json:"month"` // "2006-01"
	Start int64  `json:"start"`
	End   int64  `json:"end"` // inclusive
	Plays int64  `json:"plays"`
}

// Months returns the calendar months (UTC) that overlap [from, to].
func Months(from, to int64) []MonthCount {
	if from <= 0 || to < from {
		return nil
	}
	t := time.Unix(from, 0).UTC()
	m := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	var out []MonthCount
	for m.Unix() <= to {
		next := m.AddDate(0, 1, 0)
		out = append(out, MonthCount{Month: m.Format("2006-01"), Start: m.Unix(), End: next.Unix() - 1})
		m = next
	}
	return out
}

// MonthTotals fills in Plays for each month with one cheap count request per
// month. Months already present in known (keyed by Month) with a complete End
// before "now" are reused instead of re-requested.
func (c *Client) MonthTotals(ctx context.Context, user string, months []MonthCount, known map[string]MonthCount, now int64, workers int, progress Progress) ([]MonthCount, error) {
	if workers <= 0 {
		workers = 8
	}
	out := make([]MonthCount, len(months))
	copy(out, months)
	var todo []int
	for i, m := range out {
		if k, ok := known[m.Month]; ok && m.End < now-86400 && k.End == m.End {
			out[i].Plays = k.Plays
			continue
		}
		todo = append(todo, i)
	}
	var (
		mu       sync.Mutex
		firstErr error
		done     int
	)
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				n, err := c.CountScrobbles(ctx, user, out[i].Start, out[i].End)
				mu.Lock()
				if err != nil && firstErr == nil {
					firstErr = err
				}
				out[i].Plays = n
				done++
				d := done
				mu.Unlock()
				progress.report("months", d, len(todo))
			}
		}()
	}
	for _, i := range todo {
		select {
		case jobs <- i:
		case <-ctx.Done():
		}
	}
	close(jobs)
	wg.Wait()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return out, firstErr
}

// TrackDurations looks up the length of each track (seconds). Tracks Last.fm
// does not know come back as 0. Failures are skipped, not fatal.
func (c *Client) TrackDurations(ctx context.Context, tracks []scrobble.Track, workers int, progress Progress) map[scrobble.Track]int {
	if workers <= 0 {
		workers = 8
	}
	out := make(map[scrobble.Track]int, len(tracks))
	var mu sync.Mutex
	jobs := make(chan scrobble.Track)
	var wg sync.WaitGroup
	done := 0
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range jobs {
				d, err := c.TrackDuration(ctx, t.Artist, t.Title)
				mu.Lock()
				if err == nil {
					out[t] = d
				}
				done++
				n := done
				mu.Unlock()
				progress.report("durations", n, len(tracks))
			}
		}()
	}
	for _, t := range tracks {
		select {
		case jobs <- t:
		case <-ctx.Done():
		}
	}
	close(jobs)
	wg.Wait()
	return out
}
