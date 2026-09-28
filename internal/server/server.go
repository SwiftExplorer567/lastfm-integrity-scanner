// Package server exposes scans over HTTP for the admin panel.
//
//	POST /v1/scans                     start a scan: {"user","mode","refresh"}
//	GET  /v1/scans/{id}                job status and progress (+ report when done)
//	GET  /v1/scans/{id}/report.{fmt}   the job's report as json|html|pdf|png|svg
//	GET  /v1/users/{user}/report.{fmt} the latest saved report for a user
//	POST /v1/batch                     start scans for many users: {"users":[...],"mode"}
//	POST /v1/check                     pre-import check, answers in seconds: {"user","force"}
//	GET  /v1/check/{user}              the same (cached for a few hours unless ?force=1)
//	GET  /v1/users/{user}/check.{fmt}  the latest pre-import check as json|html|pdf|png|svg
//	GET  /healthz
//
// Every /v1 route requires "Authorization: Bearer <token>" when a token is
// configured.
package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/swiftexplorer567/lastfm-integrity-scanner/internal/analysis"
	"github.com/swiftexplorer567/lastfm-integrity-scanner/internal/lastfm"
	"github.com/swiftexplorer567/lastfm-integrity-scanner/internal/render"
	"github.com/swiftexplorer567/lastfm-integrity-scanner/internal/scanner"
	"github.com/swiftexplorer567/lastfm-integrity-scanner/internal/scrobble"
)

type Config struct {
	Token string
	// MaxConcurrent scans run at once; the rest queue. They share one
	// rate-limited API client, so more concurrency does not mean more speed
	// once the limit is reached.
	MaxConcurrent int
	// JobTTL is how long finished jobs stay in memory. Their reports stay on
	// disk regardless.
	JobTTL time.Duration
}

type Server struct {
	cfg  Config
	scan *scanner.Scanner
	log  *slog.Logger
	sem  chan struct{}

	mu     sync.Mutex
	jobs   map[string]*Job
	byUser map[string]*Job // running or queued job per user
}

func New(s *scanner.Scanner, cfg Config, log *slog.Logger) *Server {
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 2
	}
	if cfg.JobTTL <= 0 {
		cfg.JobTTL = 6 * time.Hour
	}
	return &Server{
		cfg: cfg, scan: s, log: log,
		sem:    make(chan struct{}, cfg.MaxConcurrent),
		jobs:   map[string]*Job{},
		byUser: map[string]*Job{},
	}
}

// Job is one scan's lifecycle.
type Job struct {
	ID        string    `json:"id"`
	User      string    `json:"user"`
	Mode      string    `json:"mode"`
	Status    string    `json:"status"` // queued | running | done | failed
	Stage     string    `json:"stage,omitempty"`
	Done      int       `json:"done,omitempty"`
	Total     int       `json:"total,omitempty"`
	Error     string    `json:"error,omitempty"`
	ErrorCode string    `json:"error_code,omitempty"` // not_found | private | failed
	Created   time.Time `json:"created"`
	Started   time.Time `json:"started,omitzero"`
	Finished  time.Time `json:"finished,omitzero"`

	req    scanner.Request
	report *analysis.Report
}

type jobView struct {
	*Job
	Score   *analysis.Score   `json:"score,omitempty"`
	Action  string            `json:"leaderboard_action,omitempty"`
	Reports map[string]string `json:"reports,omitempty"`
	Report  *analysis.Report  `json:"report,omitempty"`
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /v1/scans", s.auth(s.handleCreate))
	mux.HandleFunc("POST /v1/batch", s.auth(s.handleBatch))
	mux.HandleFunc("GET /v1/scans", s.auth(s.handleList))
	mux.HandleFunc("GET /v1/scans/{id}", s.auth(s.handleGet))
	mux.HandleFunc("GET /v1/scans/{id}/{file}", s.auth(s.handleJobReport))
	mux.HandleFunc("GET /v1/users/{user}/{file}", s.auth(s.handleUserReport))
	mux.HandleFunc("POST /v1/check", s.auth(s.handleCheck))
	mux.HandleFunc("GET /v1/check/{user}", s.auth(s.handleCheck))
	return logRequests(s.log, mux)
}

func (s *Server) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.Token != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.Token)) != 1 {
				writeErr(w, http.StatusUnauthorized, "unauthorized", "missing or wrong bearer token")
				return
			}
		}
		h(w, r)
	}
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req scanner.Request
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "body must be JSON: {\"user\": \"name\", \"mode\": \"auto|full|quick\"}")
		return
	}
	job, err := s.enqueue(req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, s.view(job, false))
}

func (s *Server) handleBatch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Users   []string `json:"users"`
		Mode    string   `json:"mode"`
		Refresh bool     `json:"refresh"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil || len(body.Users) == 0 {
		writeErr(w, http.StatusBadRequest, "bad_request", "body must be JSON: {\"users\": [\"a\", \"b\"], \"mode\": \"auto\"}")
		return
	}
	var out []jobView
	for _, u := range body.Users {
		job, err := s.enqueue(scanner.Request{User: u, Mode: body.Mode, Refresh: body.Refresh})
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		out = append(out, s.view(job, false))
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"jobs": out})
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	var out []jobView
	for _, j := range s.jobs {
		out = append(out, s.viewLocked(j, false))
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"jobs": out})
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	job := s.job(r.PathValue("id"))
	if job == nil {
		writeErr(w, http.StatusNotFound, "not_found", "no such scan")
		return
	}
	writeJSON(w, http.StatusOK, s.view(job, r.URL.Query().Get("report") == "1"))
}

func (s *Server) handleJobReport(w http.ResponseWriter, r *http.Request) {
	job := s.job(r.PathValue("id"))
	if job == nil {
		writeErr(w, http.StatusNotFound, "not_found", "no such scan")
		return
	}
	s.mu.Lock()
	rep, status := job.report, job.Status
	s.mu.Unlock()
	if rep == nil {
		writeErr(w, http.StatusConflict, "not_ready", "scan is "+status)
		return
	}
	serveReport(w, r, rep, r.PathValue("file"))
}

// checkView is the answer an import pipeline acts on: the decision up
// front, the evidence behind it, links to the rendered report.
type checkView struct {
	User        string               `json:"user"`
	Decision    string               `json:"decision"`
	Reason      string               `json:"reason"`
	Score       analysis.Score       `json:"score"`
	Leaderboard analysis.Leaderboard `json:"leaderboard"`
	Signals     []analysis.Signal    `json:"signals"`
	Gate        *analysis.Gate       `json:"gate"`
	Reports     map[string]string    `json:"reports"`
	Report      *analysis.Report     `json:"report,omitempty"`
}

func (s *Server) handleCheck(w http.ResponseWriter, r *http.Request) {
	var body struct {
		User  string `json:"user"`
		Force bool   `json:"force"`
	}
	if r.Method == http.MethodPost {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", "body must be JSON: {\"user\": \"name\"}")
			return
		}
	} else {
		body.User = r.PathValue("user")
		body.Force = r.URL.Query().Get("force") == "1"
	}
	body.User = strings.TrimSpace(body.User)
	if body.User == "" || len(body.User) > 64 {
		writeErr(w, http.StatusBadRequest, "bad_request", "user is required")
		return
	}
	// The check bounds its own Last.fm time; this bounds the wait in the
	// queue behind other checks.
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	rep, err := s.scan.Check(ctx, body.User, body.Force)
	if err != nil {
		switch {
		case lastfm.IsNotFound(err):
			writeErr(w, http.StatusNotFound, "not_found", err.Error())
		case lastfm.IsPrivate(err):
			writeErr(w, http.StatusUnprocessableEntity, "private", err.Error())
		case errors.Is(err, context.DeadlineExceeded):
			writeErr(w, http.StatusServiceUnavailable, "busy", "too many checks queued; retry shortly")
		default:
			writeErr(w, http.StatusBadGateway, "failed", err.Error())
		}
		return
	}
	v := checkView{
		User: rep.User.Name, Decision: rep.Gate.Decision, Reason: rep.Gate.Reason,
		Score: rep.Score, Leaderboard: rep.Leaderboard, Signals: rep.Signals, Gate: rep.Gate,
		Reports: map[string]string{},
	}
	for _, f := range render.Formats {
		v.Reports[string(f)] = "/v1/users/" + rep.User.Name + "/check." + string(f)
	}
	if r.URL.Query().Get("report") == "1" {
		v.Report = rep
	}
	s.log.Info("check", "user", rep.User.Name, "decision", rep.Gate.Decision, "score", rep.Score.Value,
		"ms", rep.Gate.ElapsedMS, "requests", rep.Gate.Requests)
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleUserReport(w http.ResponseWriter, r *http.Request) {
	load := s.scan.LatestReport
	if strings.HasPrefix(r.PathValue("file"), "check.") {
		load = s.scan.LatestCheck
	}
	rep, err := load(r.PathValue("user"))
	if errors.Is(err, os.ErrNotExist) {
		writeErr(w, http.StatusNotFound, "not_found", "no saved report for this user; POST /v1/scans first")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed", err.Error())
		return
	}
	serveReport(w, r, rep, r.PathValue("file"))
}

func serveReport(w http.ResponseWriter, r *http.Request, rep *analysis.Report, file string) {
	name, ext, ok := strings.Cut(file, ".")
	if !ok || (name != "report" && name != "check") {
		writeErr(w, http.StatusNotFound, "not_found", "use report.{json,html,pdf,png,svg} or check.{json,html,pdf,png,svg}")
		return
	}
	f, err := render.ParseFormat(ext)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	w.Header().Set("Content-Type", f.ContentType())
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition", `attachment; filename="`+render.Filename(rep, f)+`"`)
	}
	if err := render.Render(w, rep, f); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) enqueue(req scanner.Request) (*Job, error) {
	req.User = strings.TrimSpace(req.User)
	if req.User == "" || len(req.User) > 64 {
		return nil, errors.New("user is required")
	}
	switch req.Mode {
	case "", scanner.ModeAuto, scanner.ModeFull, scanner.ModeQuick:
	default:
		return nil, errors.New("mode must be auto, full or quick")
	}
	if req.Mode == "" {
		req.Mode = scanner.ModeAuto
	}
	key := scrobble.SafeName(req.User)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked()
	if j, ok := s.byUser[key]; ok {
		return j, nil // one scan per user at a time; hand back the running one
	}
	j := &Job{ID: newID(), User: req.User, Mode: req.Mode, Status: "queued", Created: time.Now().UTC(), req: req}
	s.jobs[j.ID] = j
	s.byUser[key] = j
	go s.run(j, key)
	return j, nil
}

func (s *Server) run(j *Job, key string) {
	s.sem <- struct{}{}
	defer func() { <-s.sem }()
	s.update(j, func() { j.Status, j.Started = "running", time.Now().UTC() })

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
	defer cancel()
	rep, err := s.scan.Scan(ctx, j.req, func(stage string, done, total int) {
		s.update(j, func() { j.Stage, j.Done, j.Total = stage, done, total })
	})
	s.update(j, func() {
		j.Finished = time.Now().UTC()
		delete(s.byUser, key)
		if err != nil && rep == nil {
			j.Status, j.Error = "failed", err.Error()
			switch {
			case lastfm.IsNotFound(err):
				j.ErrorCode = "not_found"
			case lastfm.IsPrivate(err):
				j.ErrorCode = "private"
			default:
				j.ErrorCode = "failed"
			}
			return
		}
		j.Status, j.Stage, j.report = "done", "", rep
	})
	if err != nil {
		s.log.Warn("scan", "user", j.User, "err", err)
	} else {
		s.log.Info("scan", "user", j.User, "score", rep.Score.Value, "verdict", rep.Score.Verdict,
			"plays", rep.Integrity.PlaysAnalyzed, "seconds", rep.Measurement.ElapsedSec)
	}
}

func (s *Server) update(j *Job, f func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f()
}

func (s *Server) job(id string) *Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.jobs[id]
}

func (s *Server) view(j *Job, full bool) jobView {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.viewLocked(j, full)
}

func (s *Server) viewLocked(j *Job, full bool) jobView {
	cp := *j
	v := jobView{Job: &cp}
	if j.report != nil {
		v.Score = &j.report.Score
		v.Action = j.report.Leaderboard.Action
		v.Reports = map[string]string{}
		for _, f := range render.Formats {
			v.Reports[string(f)] = "/v1/scans/" + j.ID + "/report." + string(f)
		}
		if full {
			v.Report = j.report
		}
	}
	return v
}

func (s *Server) gcLocked() {
	cut := time.Now().Add(-s.cfg.JobTTL)
	for id, j := range s.jobs {
		if !j.Finished.IsZero() && j.Finished.Before(cut) {
			delete(s.jobs, id)
		}
	}
}

func newID() string {
	b := make([]byte, 9)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": code, "message": msg})
}

func logRequests(log *slog.Logger, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		h.ServeHTTP(w, r)
		if r.URL.Path != "/healthz" {
			log.Debug("http", "method", r.Method, "path", r.URL.Path, "ms", time.Since(start).Milliseconds())
		}
	})
}
