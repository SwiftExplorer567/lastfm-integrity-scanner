// Package mockfm is a stand-in for the Last.fm API that serves generated
// histories. It speaks the same JSON (numbers as strings, a lone track as an
// object, a "now playing" entry on page one) and can inject the errors the
// real API throws under load, so the client and scanner are tested against
// the awkward parts too.
package mockfm

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/swiftexplorer567/lastfm-integrity-scanner/internal/scrobble"
)

type User struct {
	History    *scrobble.History
	Registered int64
	Durations  map[scrobble.Track]int
}

type Server struct {
	mu    sync.RWMutex
	users map[string]*User
	// FailEvery makes every Nth request fail, alternating between a rate
	// limit error and an HTTP 500. 0 disables it.
	FailEvery int64
	Requests  atomic.Int64
}

func New() *Server { return &Server{users: map[string]*User{}} }

func (s *Server) AddUser(u *User) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.users[strings.ToLower(u.History.User)] = u
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n := s.Requests.Add(1)
	w.Header().Set("Content-Type", "application/json")
	if s.FailEvery > 0 && n%s.FailEvery == 0 {
		if (n/s.FailEvery)%2 == 0 {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `<html>Internal Server Error</html>`)
		} else {
			writeErr(w, 29, "Rate Limit Exceeded")
		}
		return
	}
	q := r.URL.Query()
	if q.Get("api_key") == "" {
		writeErr(w, 10, "Invalid API key")
		return
	}
	s.mu.RLock()
	u := s.users[strings.ToLower(q.Get("user"))]
	s.mu.RUnlock()
	switch strings.ToLower(q.Get("method")) {
	case "user.getinfo":
		if u == nil {
			writeErr(w, 6, "User not found")
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"user": map[string]any{
			"name":       u.History.User,
			"playcount":  strconv.Itoa(len(u.History.Plays)),
			"country":    "None",
			"url":        "https://www.last.fm/user/" + u.History.User,
			"subscriber": "0",
			"registered": map[string]any{"unixtime": strconv.FormatInt(u.Registered, 10), "#text": u.Registered},
			"image":      []map[string]string{{"size": "small", "#text": ""}},
		}})
	case "user.getrecenttracks":
		if u == nil {
			writeErr(w, 6, "User not found")
			return
		}
		s.recent(w, q, u)
	case "track.getinfo":
		s.mu.RLock()
		var d int
		for _, u := range s.users {
			if v, ok := u.Durations[scrobble.Track{Artist: q.Get("artist"), Title: q.Get("track")}]; ok {
				d = v
				break
			}
		}
		s.mu.RUnlock()
		json.NewEncoder(w).Encode(map[string]any{"track": map[string]any{"name": q.Get("track"), "duration": strconv.Itoa(d * 1000)}})
	default:
		writeErr(w, 3, "Invalid Method")
	}
}

func (s *Server) recent(w http.ResponseWriter, q map[string][]string, u *User) {
	get := func(k string) string {
		if v := q[k]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	page, _ := strconv.Atoi(get("page"))
	if page < 1 {
		page = 1
	}
	limit, _ := strconv.Atoi(get("limit"))
	if limit < 1 || limit > 200 {
		limit = 50
	}
	from, _ := strconv.ParseInt(get("from"), 10, 64)
	to, _ := strconv.ParseInt(get("to"), 10, 64)
	plays := u.History.Plays
	lo := sort.Search(len(plays), func(i int) bool { return plays[i].TS >= from })
	hi := len(plays)
	if to > 0 {
		hi = sort.Search(len(plays), func(i int) bool { return plays[i].TS > to })
	}
	total := hi - lo
	if total < 0 {
		total = 0
	}
	pages := (total + limit - 1) / limit
	// Newest first.
	var tracks []map[string]any
	if page == 1 && to == 0 {
		tracks = append(tracks, map[string]any{
			"name": "Now Playing Song", "artist": map[string]string{"#text": "Someone"},
			"album": map[string]string{"#text": ""}, "@attr": map[string]string{"nowplaying": "true"},
		})
	}
	for k := hi - 1 - (page-1)*limit; k >= lo && k > hi-1-page*limit; k-- {
		t := u.History.Tracks[plays[k].Track]
		tracks = append(tracks, map[string]any{
			"name":   t.Title,
			"artist": map[string]string{"#text": t.Artist, "mbid": ""},
			"album":  map[string]string{"#text": t.Album, "mbid": ""},
			"date":   map[string]string{"uts": strconv.FormatInt(plays[k].TS, 10), "#text": ""},
		})
	}
	var trackField any = tracks
	if len(tracks) == 1 {
		trackField = tracks[0] // Last.fm's single-item quirk
	}
	if tracks == nil {
		trackField = []any{}
	}
	json.NewEncoder(w).Encode(map[string]any{"recenttracks": map[string]any{
		"track": trackField,
		"@attr": map[string]string{
			"user": u.History.User, "page": strconv.Itoa(page), "perPage": strconv.Itoa(limit),
			"totalPages": strconv.Itoa(pages), "total": strconv.Itoa(total),
		},
	}})
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	json.NewEncoder(w).Encode(map[string]any{"error": code, "message": msg})
}
