// Package lastfm is a small, read-only Last.fm API client built for bulk
// history downloads: one shared rate limiter, retries with backoff on the
// errors Last.fm returns under load, and tolerant decoding of its JSON.
package lastfm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const DefaultBaseURL = "https://ws.audioscrobbler.com/2.0/"

type Config struct {
	APIKey  string
	BaseURL string
	// RPS is the sustained request rate across every goroutine using this
	// client. Last.fm's terms allow 5 per second per IP, averaged over five
	// minutes, so short bursts are fine: RPS 4.5 with Burst 60 stays under
	// 1,500 requests in any five-minute window.
	RPS        float64
	Burst      int
	Timeout    time.Duration
	MaxRetries int
	// FixedRate turns off adapting the rate to rate-limit answers (tests
	// that inject such answers on purpose).
	FixedRate bool
	// RetryBase is the first backoff delay; it doubles per attempt and is
	// four times longer after a rate-limit error.
	RetryBase time.Duration
	UserAgent string
}

type Client struct {
	cfg     Config
	http    *http.Client
	limiter *rate.Limiter
	extra   *rate.Limiter // optional tighter limit, see Throttled
	aimd    *aimd
}

// aimd adapts the shared rate to what Last.fm actually tolerates: every
// rate-limit answer cuts the rate by 30% (never below 1/s) and shrinks the
// burst; each 50 successes after that win back 10%, up to the configured
// rate. The configured values are an upper bound, never exceeded.
type aimd struct {
	mu        sync.Mutex
	successes int
}

func (c *Client) slowDown() {
	c.aimd.mu.Lock()
	defer c.aimd.mu.Unlock()
	c.aimd.successes = 0
	c.limiter.SetLimit(max(1, c.limiter.Limit()*0.7))
	c.limiter.SetBurst(max(5, c.limiter.Burst()/2))
}

func (c *Client) speedUp() {
	lim := c.limiter.Limit()
	if float64(lim) >= c.cfg.RPS && c.limiter.Burst() >= c.cfg.Burst {
		return
	}
	c.aimd.mu.Lock()
	defer c.aimd.mu.Unlock()
	c.aimd.successes++
	if c.aimd.successes < 50 {
		return
	}
	c.aimd.successes = 0
	c.limiter.SetLimit(rate.Limit(min(c.cfg.RPS, float64(lim)*1.1)))
	c.limiter.SetBurst(min(c.cfg.Burst, c.limiter.Burst()+5))
}

// RetryStats counts, per reason, the retries made by calls whose context
// carries it (see WithRetryStats).
type RetryStats struct {
	mu     sync.Mutex
	counts map[string]int
}

type retryKey struct{}

// WithRetryStats returns a context whose calls record their retries in the
// returned stats.
func WithRetryStats(ctx context.Context) (context.Context, *RetryStats) {
	st := &RetryStats{counts: map[string]int{}}
	return context.WithValue(ctx, retryKey{}, st), st
}

func (s *RetryStats) add(reason string) {
	s.mu.Lock()
	s.counts[reason]++
	s.mu.Unlock()
}

// Counts returns a copy of the retry counts, or nil if there were none.
func (s *RetryStats) Counts() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.counts) == 0 {
		return nil
	}
	out := make(map[string]int, len(s.counts))
	for k, v := range s.counts {
		out[k] = v
	}
	return out
}

func retryReason(err error) string {
	var ae *APIError
	var he *httpError
	switch {
	case errors.As(err, &ae) && ae.Code == ErrRateLimited:
		return "rate_limited"
	case errors.As(err, &ae):
		return fmt.Sprintf("lastfm_error_%d", ae.Code)
	case errors.As(err, &he) && he.status == http.StatusTooManyRequests:
		return "rate_limited"
	case errors.As(err, &he):
		return fmt.Sprintf("http_%d", he.status)
	case err != nil && strings.Contains(err.Error(), "decode"):
		return "bad_response"
	}
	return "network"
}

func New(cfg Config) *Client {
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	if cfg.RPS <= 0 {
		cfg.RPS = 4.5
	}
	if cfg.Burst <= 0 {
		cfg.Burst = 60
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = 6
	}
	if cfg.RetryBase <= 0 {
		cfg.RetryBase = 500 * time.Millisecond
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = "lastfm-integrity-scanner/1.0"
	}
	return &Client{
		cfg: cfg,
		http: &http.Client{
			Timeout: cfg.Timeout,
			Transport: &http.Transport{
				Proxy:               http.ProxyFromEnvironment,
				MaxIdleConnsPerHost: 32,
				ForceAttemptHTTP2:   true,
			},
		},
		limiter: rate.NewLimiter(rate.Limit(cfg.RPS), cfg.Burst),
		aimd:    &aimd{},
	}
}

// APIError is an error document returned by Last.fm.
type APIError struct {
	Code    int    `json:"error"`
	Message string `json:"message"`
}

func (e *APIError) Error() string { return fmt.Sprintf("last.fm error %d: %s", e.Code, e.Message) }

// Last.fm error codes worth knowing about.
const (
	ErrInvalidParams   = 6  // also "User not found"
	ErrOperationFailed = 8  // backend hiccup, retry
	ErrServiceOffline  = 11 // retry
	ErrTemporary       = 16 // retry
	ErrLoginRequired   = 17 // user hides recent listening
	ErrRateLimited     = 29
)

func (e *APIError) retryable() bool {
	switch e.Code {
	case ErrOperationFailed, ErrServiceOffline, ErrTemporary, ErrRateLimited:
		return true
	}
	return false
}

// IsNotFound reports whether err means the user does not exist.
func IsNotFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Code == ErrInvalidParams
}

// IsPrivate reports whether the user hides their listening history.
func IsPrivate(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Code == ErrLoginRequired
}

// callFor is call for per-user methods. Last.fm occasionally answers with
// another user's data (a documented API bug); an answer whose user does not
// match is discarded and asked for again, so one user's plays can never end
// up in another user's report.
func (c *Client) callFor(ctx context.Context, method string, params url.Values, out any, user string, got func() string) error {
	for attempt := 0; ; attempt++ {
		if err := c.call(ctx, method, params, out); err != nil {
			return err
		}
		g := got()
		if g == "" || strings.EqualFold(g, user) {
			return nil
		}
		if attempt == 2 {
			return fmt.Errorf("last.fm: %s returned data for %q instead of %q", method, g, user)
		}
	}
}

// Tokens reports how many requests could start right now without waiting.
// It is how callers notice the shared budget is under pressure.
func (c *Client) Tokens() float64 {
	t := c.limiter.Tokens()
	if c.extra != nil {
		t = min(t, c.extra.Tokens())
	}
	return t
}

// Throttled returns a client that shares this one's connection pool and
// rate limit but is additionally held to rps. Bulk downloads use it so they
// always leave headroom for the fast pre-import checks.
func (c *Client) Throttled(rps float64) *Client {
	cp := *c
	cp.extra = rate.NewLimiter(rate.Limit(rps), max(1, int(rps)))
	return &cp
}

type httpError struct {
	status int
	body   string
}

func (e *httpError) Error() string { return fmt.Sprintf("last.fm http %d: %s", e.status, e.body) }

// call performs one API method and decodes the JSON body into out.
func (c *Client) call(ctx context.Context, method string, params url.Values, out any) error {
	if c.cfg.APIKey == "" {
		return errors.New("last.fm: no API key configured (set LASTFM_API_KEY)")
	}
	q := url.Values{}
	for k, v := range params {
		q[k] = v
	}
	q.Set("method", method)
	q.Set("api_key", c.cfg.APIKey)
	q.Set("format", "json")
	u := c.cfg.BaseURL + "?" + q.Encode()

	var last error
	for attempt := 0; attempt <= c.cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			reason := retryReason(last)
			if st, ok := ctx.Value(retryKey{}).(*RetryStats); ok {
				st.add(reason)
			}
			if reason == "rate_limited" && !c.cfg.FixedRate {
				c.slowDown()
			}
			if err := sleep(ctx, backoff(c.cfg.RetryBase, attempt, last)); err != nil {
				return err
			}
		}
		if c.extra != nil {
			if err := c.extra.Wait(ctx); err != nil {
				return err
			}
		}
		if err := c.limiter.Wait(ctx); err != nil {
			return err
		}
		body, err := c.get(ctx, u)
		if err != nil {
			last = err
			if ctx.Err() != nil {
				return ctx.Err()
			}
			var he *httpError
			if errors.As(err, &he) && he.status < 500 && he.status != http.StatusTooManyRequests {
				// A 4xx other than 429 carries an API error document; fall through
				// to decode it if there is one.
				if ae := decodeAPIError(he.body); ae != nil {
					if !ae.retryable() {
						return ae
					}
					last = ae
				}
			}
			continue
		}
		if ae := decodeAPIError(string(body)); ae != nil {
			if !ae.retryable() {
				return ae
			}
			last = ae
			continue
		}
		if err := json.Unmarshal(body, out); err != nil {
			// Truncated or garbled responses happen on deep pages; retry them.
			last = fmt.Errorf("last.fm: decode %s: %w", method, err)
			continue
		}
		c.speedUp()
		return nil
	}
	return fmt.Errorf("last.fm: %s failed after %d attempts: %w", method, c.cfg.MaxRetries+1, last)
}

func (c *Client) get(ctx context.Context, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.cfg.UserAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &httpError{status: resp.StatusCode, body: truncate(string(body), 300)}
	}
	return body, nil
}

func decodeAPIError(body string) *APIError {
	if len(body) == 0 || len(body) > 1024 {
		return nil
	}
	var ae APIError
	if json.Unmarshal([]byte(body), &ae) != nil || ae.Code == 0 {
		return nil
	}
	return &ae
}

func backoff(base time.Duration, attempt int, last error) time.Duration {
	var ae *APIError
	var he *httpError
	if (errors.As(last, &ae) && ae.Code == ErrRateLimited) ||
		(errors.As(last, &he) && he.status == http.StatusTooManyRequests) {
		base *= 4
	}
	d := base << (attempt - 1)
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	return d/2 + time.Duration(rand.Int64N(int64(d/2)+1))
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// flexInt decodes Last.fm numbers, which arrive as JSON numbers or strings.
type flexInt int64

func (f *flexInt) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		if s == "" {
			*f = 0
			return nil
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			*f = 0
			return nil
		}
		*f = flexInt(n)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		*f = 0
		return nil
	}
	v, _ := n.Int64()
	*f = flexInt(v)
	return nil
}

// oneOrMany decodes a field that Last.fm returns as an object when there is a
// single item and as an array otherwise.
type oneOrMany[T any] []T

func (o *oneOrMany[T]) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '{' {
		var one T
		if err := json.Unmarshal(b, &one); err != nil {
			return err
		}
		*o = []T{one}
		return nil
	}
	var many []T
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*o = many
	return nil
}
