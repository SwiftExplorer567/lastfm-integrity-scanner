package lastfm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// A rate-limit answer lowers the shared rate; successes win it back, never
// above the configured rate.
func TestRateAdaptsToRateLimitAnswers(t *testing.T) {
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) <= 3 {
			fmt.Fprint(w, `{"error":29,"message":"Rate Limit Exceeded"}`)
			return
		}
		fmt.Fprint(w, `{"user":{"name":"u","playcount":"1","registered":{"unixtime":"1"}}}`)
	}))
	defer srv.Close()
	c := New(Config{APIKey: "k", BaseURL: srv.URL, RPS: 100, Burst: 100, RetryBase: time.Millisecond})
	ctx, st := WithRetryStats(context.Background())
	if _, err := c.UserInfo(ctx, "u"); err != nil {
		t.Fatal(err)
	}
	if got := st.Counts()["rate_limited"]; got != 3 {
		t.Errorf("rate_limited retries = %d, want 3", got)
	}
	low := float64(c.limiter.Limit())
	if low > 100*0.7*0.7*0.7+0.01 || c.limiter.Burst() > 25 {
		t.Fatalf("after 3 rate-limit answers: rate %.1f burst %d, want cut three times", low, c.limiter.Burst())
	}
	for i := 0; i < 400; i++ {
		if _, err := c.UserInfo(context.Background(), "u"); err != nil {
			t.Fatal(err)
		}
	}
	if got := float64(c.limiter.Limit()); got <= low || got > 100 {
		t.Errorf("after 400 successes: rate %.1f, want recovered above %.1f and at most 100", got, low)
	}
}

func TestUserMismatchIsRetried(t *testing.T) {
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		who := "alice"
		if n.Add(1) == 1 {
			who = "mallory" // Last.fm's "random results" bug
		}
		fmt.Fprintf(w, `{"recenttracks":{"track":[],"@attr":{"user":%q,"page":"1","totalPages":"0","total":"0"}}}`, who)
	}))
	defer srv.Close()
	c := New(Config{APIKey: "k", BaseURL: srv.URL, RPS: 100, Burst: 100})
	if _, err := c.RecentTracks(context.Background(), "Alice", 1, 10, 0, 0); err != nil {
		t.Fatal(err)
	}
	if n.Load() != 2 {
		t.Errorf("%d requests, want the wrong-user answer re-asked once", n.Load())
	}
}
