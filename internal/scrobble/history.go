// Package scrobble holds a user's listening history in a compact, interned
// form: every distinct track is stored once and each play is a timestamp plus
// a track index. Millions of plays fit in tens of megabytes this way.
package scrobble

import (
	"sort"
)

// Track is one distinct artist/title/album combination as Last.fm reported it.
type Track struct {
	Artist string `json:"artist"`
	Title  string `json:"title"`
	Album  string `json:"album,omitempty"`
}

// Play is a single scrobble: the Unix time the track started and the index of
// the track in History.Tracks.
type Play struct {
	TS    int64
	Track uint32
}

// History is every scrobble fetched for one user.
type History struct {
	User   string
	Tracks []Track
	Plays  []Play

	index map[Track]uint32
}

// New returns an empty history for user.
func New(user string) *History {
	return &History{User: user, index: make(map[Track]uint32)}
}

// Intern returns the index of t, adding it if it has not been seen.
func (h *History) Intern(t Track) uint32 {
	if h.index == nil {
		h.rebuildIndex()
	}
	if id, ok := h.index[t]; ok {
		return id
	}
	id := uint32(len(h.Tracks))
	h.Tracks = append(h.Tracks, t)
	h.index[t] = id
	return id
}

// Add appends a play. Call Sort once all plays are in.
func (h *History) Add(ts int64, t Track) {
	h.Plays = append(h.Plays, Play{TS: ts, Track: h.Intern(t)})
}

// Sort orders plays by time. Plays at the same second keep a stable order.
func (h *History) Sort() {
	sort.SliceStable(h.Plays, func(i, j int) bool { return h.Plays[i].TS < h.Plays[j].TS })
}

// Replace drops every play with TS >= from and adds the plays of newer in
// their place. It is how a fresh download of a recent window is spliced onto
// a cached history, so plays deleted or added late on Last.fm are picked up.
func (h *History) Replace(from int64, newer *History) {
	cut := sort.Search(len(h.Plays), func(i int) bool { return h.Plays[i].TS >= from })
	h.Plays = h.Plays[:cut:cut]
	for _, p := range newer.Plays {
		if p.TS >= from {
			h.Add(p.TS, newer.Tracks[p.Track])
		}
	}
	h.Sort()
}

// Window returns a view of the plays with from <= TS <= to that shares the
// track table with h. A zero bound is open.
func (h *History) Window(from, to int64) *History {
	lo := 0
	if from > 0 {
		lo = sort.Search(len(h.Plays), func(i int) bool { return h.Plays[i].TS >= from })
	}
	hi := len(h.Plays)
	if to > 0 {
		hi = sort.Search(len(h.Plays), func(i int) bool { return h.Plays[i].TS > to })
	}
	return &History{User: h.User, Tracks: h.Tracks, Plays: h.Plays[lo:hi], index: h.index}
}

// First and Last return the time of the earliest and latest play, or 0.
func (h *History) First() int64 {
	if len(h.Plays) == 0 {
		return 0
	}
	return h.Plays[0].TS
}

func (h *History) Last() int64 {
	if len(h.Plays) == 0 {
		return 0
	}
	return h.Plays[len(h.Plays)-1].TS
}

func (h *History) rebuildIndex() {
	h.index = make(map[Track]uint32, len(h.Tracks))
	for i, t := range h.Tracks {
		h.index[t] = uint32(i)
	}
}
