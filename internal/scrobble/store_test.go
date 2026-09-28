package scrobble

import (
	"reflect"
	"testing"
)

func TestStoreRoundTrip(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	h := New("Some_User")
	h.Add(1_700_000_100, Track{"Sezen Aksu", "Gülümse", "Gülümse"})
	h.Add(1_700_000_000, Track{"BTS", "Body to Body", ""})
	h.Add(1_700_000_000, Track{"BTS", "SWIM", "Arirang"})
	h.Add(1_700_000_300, Track{"BTS", "Body to Body", ""})
	h.Sort()
	c := &Cached{History: h, CoveredFrom: 0, CoveredTo: 1_700_000_400}
	if err := s.Save(c); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load("some_user")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Plays, h.Plays) || !reflect.DeepEqual(got.Tracks, h.Tracks) || got.CoveredTo != c.CoveredTo {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, c)
	}
	// Interning still works after a load.
	if id := got.Intern(Track{"BTS", "SWIM", "Arirang"}); id != 2 {
		t.Errorf("intern after load = %d, want 2", id)
	}
}

func TestReplace(t *testing.T) {
	h := New("u")
	a, b := Track{"A", "a", ""}, Track{"B", "b", ""}
	for _, ts := range []int64{10, 20, 30, 40} {
		h.Add(ts, a)
	}
	n := New("u")
	n.Add(25, b) // before the cut: ignored
	n.Add(30, b)
	n.Add(50, a)
	h.Replace(30, n)
	var got []int64
	for _, p := range h.Plays {
		got = append(got, p.TS)
	}
	if want := []int64{10, 20, 30, 50}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if h.Tracks[h.Plays[2].Track] != b {
		t.Error("replaced play has the wrong track")
	}
}
