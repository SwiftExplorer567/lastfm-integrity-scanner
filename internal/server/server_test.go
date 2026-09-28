package server

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/swiftexplorer567/lastfm-integrity-scanner/internal/lastfm"
	"github.com/swiftexplorer567/lastfm-integrity-scanner/internal/mockfm"
	"github.com/swiftexplorer567/lastfm-integrity-scanner/internal/scanner"
	"github.com/swiftexplorer567/lastfm-integrity-scanner/internal/synth"
)

func TestScanOverHTTP(t *testing.T) {
	mock := mockfm.New()
	start := time.Now().AddDate(0, 0, -40)
	g := synth.Generate("carol", synth.Options{Profile: synth.TwoDevices, Seed: 3, Start: start, Days: 38})
	mock.AddUser(&mockfm.User{History: g.History, Registered: start.Unix(), Durations: g.Durations})
	fm := httptest.NewServer(mock)
	defer fm.Close()

	client := lastfm.New(lastfm.Config{APIKey: "k", BaseURL: fm.URL, RPS: 1000, Burst: 50, RetryBase: time.Millisecond})
	sc, err := scanner.New(client, t.TempDir(), scanner.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(New(sc, Config{Token: "secret"}, slog.New(slog.NewTextHandler(io.Discard, nil))).Handler())
	defer api.Close()

	do := func(method, path, body string) (*http.Response, []byte) {
		req, _ := http.NewRequest(method, api.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer secret")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, b
	}

	if resp, err := http.Post(api.URL+"/v1/scans", "application/json", strings.NewReader(`{"user":"carol"}`)); err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: got %v %v, want 401", resp.StatusCode, err)
	}

	resp, body := do("POST", "/v1/scans", `{"user":"carol","mode":"full"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	var job struct {
		ID      string            `json:"id"`
		Status  string            `json:"status"`
		Action  string            `json:"leaderboard_action"`
		Reports map[string]string `json:"reports"`
	}
	json.Unmarshal(body, &job)
	deadline := time.Now().Add(10 * time.Second)
	for job.Status != "done" {
		if job.Status == "failed" || time.Now().After(deadline) {
			t.Fatalf("job ended as %s: %s", job.Status, body)
		}
		time.Sleep(20 * time.Millisecond)
		_, body = do("GET", "/v1/scans/"+job.ID, "")
		json.Unmarshal(body, &job)
	}
	if job.Action != "keep" {
		t.Errorf("leaderboard action %q, want keep", job.Action)
	}
	for f, path := range job.Reports {
		resp, b := do("GET", path, "")
		if resp.StatusCode != 200 || len(b) < 100 {
			t.Errorf("%s: %d, %d bytes", f, resp.StatusCode, len(b))
		}
	}
	resp, b := do("GET", "/v1/users/CAROL/report.pdf", "")
	if resp.StatusCode != 200 || !bytes.HasPrefix(b, []byte("%PDF")) {
		t.Errorf("latest pdf: %d", resp.StatusCode)
	}
	resp, b = do("GET", "/v1/users/carol/report.png", "")
	if resp.StatusCode != 200 || !bytes.HasPrefix(b, []byte("\x89PNG")) {
		t.Errorf("latest png: %d", resp.StatusCode)
	}

	resp, body = do("POST", "/v1/scans", `{"user":"nobody"}`)
	json.Unmarshal(body, &job)
	for job.Status != "failed" {
		time.Sleep(20 * time.Millisecond)
		_, body = do("GET", "/v1/scans/"+job.ID, "")
		json.Unmarshal(body, &job)
	}
	if !strings.Contains(string(body), `"error_code":"not_found"`) {
		t.Errorf("unknown user: %s", body)
	}
}
