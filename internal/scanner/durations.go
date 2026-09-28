package scanner

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/swiftexplorer567/lastfm-integrity-scanner/internal/scrobble"
)

// DurationCache remembers track lengths across scans and users. Popular songs
// recur across accounts, so after a few scans most lookups are free.
type DurationCache struct {
	path  string
	mu    sync.Mutex
	m     map[string]int
	dirty bool
}

func OpenDurationCache(dir string) (*DurationCache, error) {
	c := &DurationCache{path: filepath.Join(dir, "durations.json"), m: map[string]int{}}
	b, err := os.ReadFile(c.path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &c.m); err != nil {
		// A corrupt cache is not worth failing a scan over.
		c.m = map[string]int{}
	}
	return c, nil
}

func durKey(t scrobble.Track) string {
	return strings.ToLower(t.Artist) + "\x00" + strings.ToLower(t.Title)
}

// Get returns the cached length in seconds; 0 with ok=true means Last.fm has
// no length for the track.
func (c *DurationCache) Get(t scrobble.Track) (int, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	d, ok := c.m[durKey(t)]
	return d, ok
}

func (c *DurationCache) Put(t scrobble.Track, d int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[durKey(t)] = d
	c.dirty = true
}

func (c *DurationCache) Save() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.dirty {
		return nil
	}
	b, err := json.Marshal(c.m)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return err
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	c.dirty = false
	return os.Rename(tmp, c.path)
}
