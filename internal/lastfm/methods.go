package lastfm

import (
	"context"
	"net/url"
	"strconv"
	"strings"
)

// UserInfo is the subset of user.getInfo the scanner needs.
type UserInfo struct {
	Name       string `json:"name"`
	RealName   string `json:"realname,omitempty"`
	URL        string `json:"url,omitempty"`
	Country    string `json:"country,omitempty"`
	Image      string `json:"image,omitempty"`
	Playcount  int64  `json:"playcount"`
	Registered int64  `json:"registered"`
	Subscriber bool   `json:"subscriber"`
}

func (c *Client) UserInfo(ctx context.Context, user string) (*UserInfo, error) {
	var resp struct {
		User struct {
			Name       string  `json:"name"`
			RealName   string  `json:"realname"`
			URL        string  `json:"url"`
			Country    string  `json:"country"`
			Playcount  flexInt `json:"playcount"`
			Subscriber flexInt `json:"subscriber"`
			Registered struct {
				Unixtime flexInt `json:"unixtime"`
			} `json:"registered"`
			Image []struct {
				Size string `json:"size"`
				URL  string `json:"#text"`
			} `json:"image"`
		} `json:"user"`
	}
	if err := c.call(ctx, "user.getInfo", url.Values{"user": {user}}, &resp); err != nil {
		return nil, err
	}
	u := resp.User
	info := &UserInfo{
		Name:       u.Name,
		RealName:   u.RealName,
		URL:        u.URL,
		Country:    u.Country,
		Playcount:  int64(u.Playcount),
		Registered: int64(u.Registered.Unixtime),
		Subscriber: u.Subscriber != 0,
	}
	if info.Country == "None" {
		info.Country = ""
	}
	for _, im := range u.Image {
		if im.URL != "" {
			info.Image = im.URL // the list runs small → extralarge; keep the largest
		}
	}
	return info, nil
}

// RawScrobble is one entry of user.getRecentTracks.
type RawScrobble struct {
	TS     int64
	Artist string
	Title  string
	Album  string
}

// RecentPage is one page of user.getRecentTracks.
type RecentPage struct {
	Page       int
	TotalPages int
	Total      int64
	Scrobbles  []RawScrobble
}

type recentTrack struct {
	Name   string `json:"name"`
	Artist struct {
		Text string `json:"#text"`
		Name string `json:"name"`
	} `json:"artist"`
	Album struct {
		Text string `json:"#text"`
	} `json:"album"`
	Date *struct {
		UTS flexInt `json:"uts"`
	} `json:"date"`
	Attr *struct {
		NowPlaying string `json:"nowplaying"`
	} `json:"@attr"`
}

// RecentTracks fetches one page of scrobbles. from and to are inclusive Unix
// times; 0 leaves the bound open. The "now playing" entry is dropped.
func (c *Client) RecentTracks(ctx context.Context, user string, page, limit int, from, to int64) (*RecentPage, error) {
	p := url.Values{
		"user":  {user},
		"page":  {strconv.Itoa(page)},
		"limit": {strconv.Itoa(limit)},
	}
	if from > 0 {
		p.Set("from", strconv.FormatInt(from, 10))
	}
	if to > 0 {
		p.Set("to", strconv.FormatInt(to, 10))
	}
	var resp struct {
		Recent struct {
			Track oneOrMany[recentTrack] `json:"track"`
			Attr  struct {
				User       string  `json:"user"`
				Page       flexInt `json:"page"`
				TotalPages flexInt `json:"totalPages"`
				Total      flexInt `json:"total"`
			} `json:"@attr"`
		} `json:"recenttracks"`
	}
	err := c.callFor(ctx, "user.getRecentTracks", p, &resp, user, func() string { return resp.Recent.Attr.User })
	if err != nil {
		return nil, err
	}
	r := resp.Recent
	out := &RecentPage{
		Page:       int(r.Attr.Page),
		TotalPages: int(r.Attr.TotalPages),
		Total:      int64(r.Attr.Total),
		Scrobbles:  make([]RawScrobble, 0, len(r.Track)),
	}
	for _, t := range r.Track {
		if t.Attr != nil && strings.EqualFold(t.Attr.NowPlaying, "true") {
			continue
		}
		if t.Date == nil || t.Date.UTS <= 0 {
			continue
		}
		artist := t.Artist.Text
		if artist == "" {
			artist = t.Artist.Name
		}
		out.Scrobbles = append(out.Scrobbles, RawScrobble{
			TS:     int64(t.Date.UTS),
			Artist: artist,
			Title:  t.Name,
			Album:  t.Album.Text,
		})
	}
	return out, nil
}

// TopTrack is one row of a user's lifetime (or period) top tracks.
type TopTrack struct {
	Artist  string
	Title   string
	Plays   int64
	Seconds int // 0 when Last.fm does not know the length
}

// TopTracks returns the user's most played tracks for period (overall,
// 7day, 1month, 3month, 6month, 12month) and how many distinct tracks the
// user has in that period.
func (c *Client) TopTracks(ctx context.Context, user, period string, limit int) ([]TopTrack, int64, error) {
	var resp struct {
		Top struct {
			Track oneOrMany[struct {
				Name     string  `json:"name"`
				Duration flexInt `json:"duration"`
				Plays    flexInt `json:"playcount"`
				Artist   struct {
					Name string `json:"name"`
				} `json:"artist"`
			}] `json:"track"`
			Attr struct {
				User  string  `json:"user"`
				Total flexInt `json:"total"`
			} `json:"@attr"`
		} `json:"toptracks"`
	}
	p := url.Values{"user": {user}, "period": {period}, "limit": {strconv.Itoa(limit)}}
	if err := c.callFor(ctx, "user.getTopTracks", p, &resp, user, func() string { return resp.Top.Attr.User }); err != nil {
		return nil, 0, err
	}
	out := make([]TopTrack, 0, len(resp.Top.Track))
	for _, t := range resp.Top.Track {
		out = append(out, TopTrack{Artist: t.Artist.Name, Title: t.Name, Plays: int64(t.Plays), Seconds: int(t.Duration)})
	}
	return out, int64(resp.Top.Attr.Total), nil
}

// CountScrobbles returns Last.fm's own count of scrobbles between from and to
// (inclusive) with a single one-item request.
func (c *Client) CountScrobbles(ctx context.Context, user string, from, to int64) (int64, error) {
	page, err := c.RecentTracks(ctx, user, 1, 1, from, to)
	if err != nil {
		return 0, err
	}
	return page.Total, nil
}

// TrackDuration returns a track's length in seconds, or 0 when Last.fm does
// not know it.
func (c *Client) TrackDuration(ctx context.Context, artist, title string) (int, error) {
	var resp struct {
		Track struct {
			Duration flexInt `json:"duration"`
		} `json:"track"`
	}
	p := url.Values{"artist": {artist}, "track": {title}, "autocorrect": {"1"}}
	if err := c.call(ctx, "track.getInfo", p, &resp); err != nil {
		return 0, err
	}
	return int(resp.Track.Duration / 1000), nil
}
