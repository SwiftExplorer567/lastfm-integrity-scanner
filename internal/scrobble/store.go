package scrobble

import (
	"bufio"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const magic = "LFH1"

// Cached is a history together with the time range it fully covers: every
// scrobble with CoveredFrom <= TS <= CoveredTo is in Plays. CoveredFrom 0
// means "since the account opened".
type Cached struct {
	*History
	CoveredFrom int64
	CoveredTo   int64
}

// Store keeps fetched histories on disk so a rescan only downloads what is new.
type Store struct {
	Dir string
}

func (s Store) userDir(user string) string {
	return filepath.Join(s.Dir, "users", SafeName(user))
}

// SafeName lowercases a Last.fm username and strips anything that could escape
// a directory. Last.fm names are case-insensitive letters, digits, - and _.
func SafeName(user string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(user) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "_"
	}
	return b.String()
}

// Load reads the cached history for user. It returns (nil, nil) when nothing
// is cached.
func (s Store) Load(user string) (*Cached, error) {
	f, err := os.Open(filepath.Join(s.userDir(user), "history.bin.gz"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	return decode(bufio.NewReaderSize(zr, 1<<16))
}

// Save writes the history atomically.
func (s Store) Save(c *Cached) error {
	dir := s.userDir(c.User)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "history-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	zw, _ := gzip.NewWriterLevel(tmp, gzip.BestSpeed)
	bw := bufio.NewWriterSize(zw, 1<<16)
	if err := encode(bw, c); err != nil {
		tmp.Close()
		return err
	}
	if err := bw.Flush(); err != nil {
		tmp.Close()
		return err
	}
	if err := zw.Close(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, "history.bin.gz"))
}

// LoadJSON and SaveJSON keep small per-user side files (monthly totals).
func (s Store) LoadJSON(user, name string, v any) (bool, error) {
	b, err := os.ReadFile(filepath.Join(s.userDir(user), name))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(b, v)
}

func (s Store) SaveJSON(user, name string, v any) error {
	dir := s.userDir(user)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, name+".tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, name))
}

// Format: magic, user, coveredFrom, coveredTo, track count, tracks as three
// length-prefixed strings, play count, plays as (uvarint delta TS, uvarint
// track). Plays are sorted so deltas are small.

func encode(w *bufio.Writer, c *Cached) error {
	var buf [binary.MaxVarintLen64]byte
	putU := func(v uint64) {
		n := binary.PutUvarint(buf[:], v)
		w.Write(buf[:n])
	}
	putV := func(v int64) {
		n := binary.PutVarint(buf[:], v)
		w.Write(buf[:n])
	}
	putS := func(s string) {
		putU(uint64(len(s)))
		w.WriteString(s)
	}
	w.WriteString(magic)
	putS(c.User)
	putV(c.CoveredFrom)
	putV(c.CoveredTo)
	putU(uint64(len(c.Tracks)))
	for _, t := range c.Tracks {
		putS(t.Artist)
		putS(t.Title)
		putS(t.Album)
	}
	putU(uint64(len(c.Plays)))
	var prev int64
	for _, p := range c.Plays {
		putV(p.TS - prev)
		prev = p.TS
		putU(uint64(p.Track))
	}
	return nil
}

func decode(r *bufio.Reader) (*Cached, error) {
	head := make([]byte, len(magic))
	if _, err := io.ReadFull(r, head); err != nil || string(head) != magic {
		return nil, fmt.Errorf("scrobble cache: bad header")
	}
	var err error
	getU := func() uint64 {
		if err != nil {
			return 0
		}
		var v uint64
		v, err = binary.ReadUvarint(r)
		return v
	}
	getV := func() int64 {
		if err != nil {
			return 0
		}
		var v int64
		v, err = binary.ReadVarint(r)
		return v
	}
	getS := func() string {
		n := getU()
		if err != nil {
			return ""
		}
		if n > 1<<20 {
			err = fmt.Errorf("scrobble cache: string too long")
			return ""
		}
		b := make([]byte, n)
		_, err = io.ReadFull(r, b)
		return string(b)
	}
	c := &Cached{History: New(getS())}
	c.CoveredFrom = getV()
	c.CoveredTo = getV()
	nt := getU()
	if err != nil {
		return nil, err
	}
	c.Tracks = make([]Track, 0, nt)
	for i := uint64(0); i < nt && err == nil; i++ {
		c.Tracks = append(c.Tracks, Track{Artist: getS(), Title: getS(), Album: getS()})
	}
	np := getU()
	if err != nil {
		return nil, err
	}
	c.Plays = make([]Play, 0, np)
	var ts int64
	for i := uint64(0); i < np && err == nil; i++ {
		ts += getV()
		id := getU()
		if id >= nt {
			return nil, fmt.Errorf("scrobble cache: track index out of range")
		}
		c.Plays = append(c.Plays, Play{TS: ts, Track: uint32(id)})
	}
	if err != nil {
		return nil, fmt.Errorf("scrobble cache: %w", err)
	}
	c.rebuildIndex()
	return c, nil
}
