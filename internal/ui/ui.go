// Package ui is a local admin console for pre-import checks: run checks and
// watch them progress, browse and filter results, read the evidence, label
// accounts as fake or honest, re-score everything offline with the current
// rules, and export captures and labels for calibration.
//
// It is meant for one admin on their own machine: it listens on localhost
// and has no login.
package ui

import (
	"archive/zip"
	"context"
	"embed"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/swiftexplorer567/lastfm-integrity-scanner/internal/analysis"
	"github.com/swiftexplorer567/lastfm-integrity-scanner/internal/lastfm"
	"github.com/swiftexplorer567/lastfm-integrity-scanner/internal/render"
	"github.com/swiftexplorer567/lastfm-integrity-scanner/internal/scanner"
	"github.com/swiftexplorer567/lastfm-integrity-scanner/internal/scrobble"
)

//go:embed static
var static embed.FS

type Server struct {
	scan    *scanner.Scanner
	dataDir string
	log     *slog.Logger
	labels  *labelStore

	mu    sync.Mutex
	jobs  []*job
	queue chan *job
	cache map[string]cachedAccount // by reports path
}

type cachedAccount struct {
	mod time.Time
	acc Account
}

// New returns the console. scan.Client may be nil when no API key is set:
// everything but running new checks still works. workers checks run at once;
// set scan.Opts.Check.Patient so long batches read every account in full.
func New(scan *scanner.Scanner, workers int, log *slog.Logger) (*Server, error) {
	dir := scan.Store.Dir
	ls, err := openLabels(filepath.Join(dir, "labels.json"))
	if err != nil {
		return nil, err
	}
	s := &Server{scan: scan, dataDir: dir, log: log, labels: ls, queue: make(chan *job, 10000), cache: map[string]cachedAccount{}}
	for range max(workers, 1) {
		go s.worker()
	}
	return s, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	sub, _ := fs.Sub(static, "static")
	mux.Handle("GET /", http.FileServer(http.FS(sub)))
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("GET /api/accounts", s.handleAccounts)
	mux.HandleFunc("GET /api/accounts/{user}/{file}", s.handleReport)
	mux.HandleFunc("POST /api/check", s.handleCheck)
	mux.HandleFunc("GET /api/jobs", s.handleJobs)
	mux.HandleFunc("POST /api/jobs/clear", s.handleClearJobs)
	mux.HandleFunc("POST /api/labels/{user}", s.handleLabel)
	mux.HandleFunc("POST /api/recheck", s.handleRecheck)
	mux.HandleFunc("GET /api/export", s.handleExport)
	return mux
}

// ---- status ----

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	st := map[string]any{"api_key": s.scan.Client != nil}
	if c := s.scan.Client; c != nil {
		st["tokens"] = int(c.Tokens())
		st["burst"] = c.Burst()
	}
	s.mu.Lock()
	counts := map[string]int{}
	for _, j := range s.jobs {
		counts[j.Status]++
	}
	s.mu.Unlock()
	st["jobs"] = counts
	writeJSON(w, st)
}

// ---- accounts ----

// Account is one row of the results table: the latest check of a user,
// with its label.
type Account struct {
	User         string   `json:"user"`
	Decision     string   `json:"decision"`
	Score        int      `json:"score"`
	Reason       string   `json:"reason"`
	Scrobbles    int64    `json:"scrobbles"`
	Adjusted     int64    `json:"adjusted"`
	RemovedShare float64  `json:"removed_share"`
	ExcessShare  float64  `json:"excess_share"`
	PeakPerDay   float64  `json:"peak_per_day"`
	AvgPerDay    float64  `json:"avg_per_day"`
	CheckedAt    string   `json:"checked_at"`
	Seconds      float64  `json:"seconds"`
	Requests     int      `json:"requests"`
	Signals      []string `json:"signals"`
	Registered   int64    `json:"registered"`
	Country      string   `json:"country,omitempty"`
	Image        string   `json:"image,omitempty"`
	DupShare     float64  `json:"dup_share"`
	SameSecond   float64  `json:"same_second_share"`
	TopShare     float64  `json:"top_track_share"`
	MonthsOver   int      `json:"months_over_limit"`
	Complete     bool     `json:"complete"`
	Early        bool     `json:"decided_early"`
	Capture      bool     `json:"capture"`
	Label        string   `json:"label"`
	Note         string   `json:"note"`
}

func (s *Server) accounts() ([]Account, error) {
	paths, _ := filepath.Glob(filepath.Join(s.dataDir, "reports", "*", "check-latest.json"))
	var out []Account
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			continue
		}
		s.mu.Lock()
		c, ok := s.cache[p]
		s.mu.Unlock()
		if !ok || !c.mod.Equal(st.ModTime()) {
			r, err := readReport(p)
			if err != nil || r.Gate == nil {
				continue
			}
			c = cachedAccount{mod: st.ModTime(), acc: accountOf(r)}
			s.mu.Lock()
			s.cache[p] = c
			s.mu.Unlock()
		}
		a := c.acc
		_, err = os.Stat(filepath.Join(s.dataDir, "checks", scrobble.SafeName(a.User)+".json.gz"))
		a.Capture = err == nil
		if l, ok := s.labels.get(a.User); ok {
			a.Label, a.Note = l.Label, l.Note
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Scrobbles > out[j].Scrobbles })
	return out, nil
}

func accountOf(r *analysis.Report) Account {
	g := r.Gate
	a := Account{
		User: r.User.Name, Decision: g.Decision, Score: r.Score.Value, Reason: g.Reason,
		Scrobbles: r.User.Playcount, Adjusted: r.Leaderboard.AdjustedScrobbles, RemovedShare: r.Leaderboard.RemovedShare,
		ExcessShare: r.Integrity.ExcessShare, PeakPerDay: r.Stats.PeakMonth.PerDay, AvgPerDay: r.Stats.AvgPerDay,
		CheckedAt: r.GeneratedAt.Format(time.RFC3339), Seconds: float64(g.ElapsedMS) / 1000, Requests: g.Requests,
		Registered: r.User.Registered, Country: r.User.Country, Image: r.User.Image,
		SameSecond: r.Integrity.SameSecondShare, TopShare: g.TopTrackShare, MonthsOver: r.Stats.MonthsOverLimit,
		Complete: g.Complete, Early: g.DecidedEarly,
	}
	if n := r.Integrity.PlaysAnalyzed; n > 0 {
		a.DupShare = float64(r.Integrity.Duplicates) / float64(n)
	}
	for _, sg := range r.Signals {
		if sg.Points > 0 && len(a.Signals) < 4 {
			a.Signals = append(a.Signals, fmt.Sprintf("%s +%d", sg.Title, sg.Points))
		}
	}
	return a
}

func readReport(path string) (*analysis.Report, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r analysis.Report
	return &r, json.Unmarshal(b, &r)
}

func (s *Server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	acc, err := s.accounts()
	if err != nil {
		httpErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, map[string]any{"accounts": acc})
}

func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	rep, err := s.scan.LatestCheck(r.PathValue("user"))
	if err != nil {
		httpErr(w, http.StatusNotFound, err)
		return
	}
	name, ext, _ := strings.Cut(r.PathValue("file"), ".")
	f, err := render.ParseFormat(ext)
	if name != "report" || err != nil {
		httpErr(w, http.StatusNotFound, errors.New("use report.html, report.json, report.png, report.pdf or report.svg"))
		return
	}
	w.Header().Set("Content-Type", f.ContentType())
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition", `attachment; filename="`+render.Filename(rep, f)+`"`)
	}
	render.Render(w, rep, f)
}

// ---- checks ----

type job struct {
	ID      int     `json:"id"`
	User    string  `json:"user"`
	Force   bool    `json:"force"`
	Status  string  `json:"status"` // queued | running | pass | review | block | unknown | private | not found | failed
	Error   string  `json:"error,omitempty"`
	Score   int     `json:"score"`
	Seconds float64 `json:"seconds"`
	Cached  bool    `json:"cached,omitempty"`
	// Waited is time spent waiting for the rate budget before the check.
	Waited   float64   `json:"waited,omitempty"`
	Queued   time.Time `json:"queued"`
	Started  time.Time `json:"started,omitzero"`
	Finished time.Time `json:"finished,omitzero"`
}

func (s *Server) handleCheck(w http.ResponseWriter, r *http.Request) {
	if s.scan.Client == nil {
		httpErr(w, http.StatusBadRequest, errors.New("no LASTFM_API_KEY set: checks cannot run, but results, labels, recheck and export work"))
		return
	}
	var body struct {
		Users string `json:"users"`
		Force bool   `json:"force"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		httpErr(w, http.StatusBadRequest, err)
		return
	}
	seen := map[string]bool{}
	var added []*job
	s.mu.Lock()
	for _, u := range strings.FieldsFunc(body.Users, func(r rune) bool { return r == ',' || r == ';' || r == ' ' || r == '\n' || r == '\t' || r == '\r' }) {
		u = strings.TrimSpace(strings.TrimPrefix(strings.TrimRight(u, "/"), "https://www.last.fm/user/"))
		if i := strings.LastIndex(u, "/user/"); i >= 0 {
			u = u[i+6:]
		}
		key := scrobble.SafeName(u)
		if u == "" || seen[key] {
			continue
		}
		seen[key] = true
		j := &job{ID: len(s.jobs) + 1, User: u, Force: body.Force, Status: "queued", Queued: time.Now()}
		s.jobs = append(s.jobs, j)
		added = append(added, j)
	}
	s.mu.Unlock()
	for _, j := range added {
		s.queue <- j
	}
	writeJSON(w, map[string]any{"queued": len(added)})
}

// worker takes checks off the queue.
func (s *Server) worker() {
	for j := range s.queue {
		s.mu.Lock()
		j.Status, j.Started = "running", time.Now()
		s.mu.Unlock()

		rep, err := s.scan.Check(context.Background(), j.User, j.Force)

		s.mu.Lock()
		j.Finished = time.Now()
		j.Seconds = j.Finished.Sub(j.Started).Seconds()
		switch {
		case lastfm.IsPrivate(err):
			j.Status = "private"
		case lastfm.IsNotFound(err):
			j.Status = "not found"
		case err != nil:
			j.Status, j.Error = "failed", err.Error()
		default:
			j.Status, j.Score = rep.Gate.Decision, rep.Score.Value
			j.Seconds = float64(rep.Gate.ElapsedMS) / 1000
			j.Waited = max(0, j.Finished.Sub(j.Started).Seconds()-j.Seconds)
			j.Cached = rep.GeneratedAt.Before(j.Started.Add(-time.Second))
		}
		s.mu.Unlock()
		if err != nil {
			s.log.Info("check", "user", j.User, "status", j.Status, "err", err)
		} else {
			s.log.Info("check", "user", j.User, "decision", rep.Gate.Decision, "score", rep.Score.Value, "s", fmt.Sprintf("%.1f", j.Seconds))
		}
	}
}

func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	out := make([]job, len(s.jobs))
	for i, j := range s.jobs {
		out[i] = *j
	}
	s.mu.Unlock()
	writeJSON(w, map[string]any{"jobs": out})
}

func (s *Server) handleClearJobs(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	keep := s.jobs[:0]
	for _, j := range s.jobs {
		if j.Status == "queued" || j.Status == "running" {
			keep = append(keep, j)
		}
	}
	s.jobs = keep
	s.mu.Unlock()
	writeJSON(w, map[string]any{"ok": true})
}

// ---- labels ----

// Label is an admin's own verdict on an account, used to calibrate rules.
type Label struct {
	User  string    `json:"user"`
	Label string    `json:"label"` // fake | honest | unsure
	Note  string    `json:"note,omitempty"`
	At    time.Time `json:"at"`
}

type labelStore struct {
	path string
	mu   sync.Mutex
	m    map[string]Label
}

func openLabels(path string) (*labelStore, error) {
	ls := &labelStore{path: path, m: map[string]Label{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ls, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &ls.m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return ls, nil
}

func (ls *labelStore) get(user string) (Label, bool) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	l, ok := ls.m[scrobble.SafeName(user)]
	return l, ok
}

func (ls *labelStore) set(l Label) error {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	key := scrobble.SafeName(l.User)
	if l.Label == "" && l.Note == "" {
		delete(ls.m, key)
	} else {
		ls.m[key] = l
	}
	b, err := json.MarshalIndent(ls.m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(ls.path), 0o755); err != nil {
		return err
	}
	tmp := ls.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, ls.path)
}

func (s *Server) handleLabel(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Label string `json:"label"`
		Note  string `json:"note"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		httpErr(w, http.StatusBadRequest, err)
		return
	}
	switch body.Label {
	case "", "fake", "honest", "unsure":
	default:
		httpErr(w, http.StatusBadRequest, errors.New("label must be fake, honest, unsure or empty"))
		return
	}
	if err := s.labels.set(Label{User: r.PathValue("user"), Label: body.Label, Note: body.Note, At: time.Now().UTC()}); err != nil {
		httpErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// ---- recheck ----

type change struct {
	User        string `json:"user"`
	OldDecision string `json:"old_decision"`
	OldScore    int    `json:"old_score"`
	NewDecision string `json:"new_decision"`
	NewScore    int    `json:"new_score"`
	Label       string `json:"label,omitempty"`
}

// handleRecheck re-scores every saved capture with the current rules and
// saves the result as the latest check. No request goes to Last.fm.
func (s *Server) handleRecheck(w http.ResponseWriter, r *http.Request) {
	files, _ := filepath.Glob(filepath.Join(s.dataDir, "checks", "*.json.gz"))
	var changes []change
	total := 0
	for _, f := range files {
		cp, err := scanner.LoadCapture(f)
		if err != nil {
			continue
		}
		old, _ := s.scan.LatestCheck(cp.Info.Name)
		rep, err := s.scan.Recheck(cp)
		if err != nil {
			continue
		}
		total++
		if err := s.scan.SaveCheck(rep); err != nil {
			httpErr(w, http.StatusInternalServerError, err)
			return
		}
		c := change{User: rep.User.Name, NewDecision: rep.Gate.Decision, NewScore: rep.Score.Value}
		if old != nil && old.Gate != nil {
			c.OldDecision, c.OldScore = old.Gate.Decision, old.Score.Value
		}
		if l, ok := s.labels.get(rep.User.Name); ok {
			c.Label = l.Label
		}
		if c.OldDecision != c.NewDecision || c.OldScore != c.NewScore {
			changes = append(changes, c)
		}
	}
	writeJSON(w, map[string]any{"rechecked": total, "changes": changes})
}

// ---- export ----

// handleExport zips the captures, labels and a summary: everything needed to
// calibrate the rules on these accounts elsewhere.
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="lfscan-calibration-%s.zip"`, time.Now().UTC().Format("20060102-1504")))
	zw := zip.NewWriter(w)
	defer zw.Close()
	files, _ := filepath.Glob(filepath.Join(s.dataDir, "checks", "*.json.gz"))
	for _, f := range files {
		if err := addFile(zw, "checks/"+filepath.Base(f), f); err != nil {
			s.log.Warn("export", "file", f, "err", err)
		}
	}
	addFile(zw, "labels.json", s.labels.path)
	acc, _ := s.accounts()
	cw, err := zw.Create("summary.csv")
	if err != nil {
		return
	}
	c := csv.NewWriter(cw)
	c.Write([]string{"user", "decision", "score", "label", "note", "scrobbles", "adjusted", "excess_share", "peak_per_day", "signals"})
	for _, a := range acc {
		c.Write([]string{a.User, a.Decision, strconv.Itoa(a.Score), a.Label, a.Note, strconv.FormatInt(a.Scrobbles, 10),
			strconv.FormatInt(a.Adjusted, 10), fmt.Sprintf("%.4f", a.ExcessShare), fmt.Sprintf("%.0f", a.PeakPerDay), strings.Join(a.Signals, "; ")})
	}
	c.Flush()
}

func addFile(zw *zip.Writer, name, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w, err := zw.Create(name)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, f)
	return err
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func httpErr(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
