package server

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tatoalo/degitide/internal/browser"
	"github.com/tatoalo/degitide/internal/db"
	"github.com/tatoalo/degitide/internal/scheduler"
)

type progressScheduler struct {
	progress atomic.Pointer[scheduler.Progress]
}

func (s *progressScheduler) Progress() scheduler.Progress { return *s.progress.Load() }
func (s *progressScheduler) IsRunning() bool              { return s.Progress().Running }
func (s *progressScheduler) LastError() string            { return "" }
func (s *progressScheduler) TriggerNow() error            { return scheduler.ErrPullInProgress }

func TestSyncEventsObserveExternalRunsWithoutWorkerRequests(t *testing.T) {
	var calls atomic.Int32
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "must not wake worker", 500)
	}))
	defer worker.Close()
	database, err := db.New(filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	admin, err := database.CreateAdmin("demo", "unused-hash")
	if err != nil {
		t.Fatal(err)
	}
	runner := &progressScheduler{}
	runner.progress.Store(&scheduler.Progress{})
	service := New(database, browser.NewClient(worker.URL), runner, Options{APIToken: "test-secret"})
	app := httptest.NewServer(service.Router())
	defer app.Close()

	response, err := http.Get(app.URL + "/api/sync/events")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated stream: %d", response.StatusCode)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, "GET", app.URL+"/api/sync/events", nil)
	request.AddCookie(&http.Cookie{Name: dashboardSessionCookie, Value: dashboardSessionValue("test-secret", admin)})
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "text/event-stream" ||
		response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("X-Accel-Buffering") != "no" {
		t.Fatalf("stream headers: %d %v", response.StatusCode, response.Header)
	}
	events := make(chan string, 10)
	go func() {
		defer close(events)
		scanner := bufio.NewScanner(response.Body)
		for scanner.Scan() {
			if data, ok := strings.CutPrefix(scanner.Text(), "data: "); ok {
				events <- data
			}
		}
	}()
	read := func() scheduler.Progress {
		t.Helper()
		select {
		case data := <-events:
			var event struct {
				Running  bool               `json:"pull_running"`
				Progress scheduler.Progress `json:"progress"`
			}
			if err := json.Unmarshal([]byte(data), &event); err != nil {
				t.Fatal(err)
			}
			if event.Running != event.Progress.Running {
				t.Fatal("inconsistent running state")
			}
			return event.Progress
		case <-time.After(5 * time.Second):
			t.Fatal("live update never arrived")
			return scheduler.Progress{}
		}
	}
	if read().Running {
		t.Fatal("initial idle snapshot missing")
	}
	started := time.Now()
	for _, percent := range []int{25, 75, 100} {
		progress := scheduler.Progress{Running: percent < 100, Percent: percent, StartedAt: started,
			Accounts: []scheduler.AccountProgress{{ID: "second", Name: "Savings", State: "running"}}}
		if percent == 100 {
			progress.FinishedAt = time.Now()
			progress.Accounts[0].State = "complete"
		}
		runner.progress.Store(&progress)
		got := read()
		if got.Percent != percent || len(got.Accounts) != 1 || got.Accounts[0].ID != "second" {
			t.Fatalf("wrong global progress: %+v", got)
		}
	}
	cancel()
	select {
	case _, open := <-events:
		if open {
			t.Fatal("unexpected duplicate update")
		}
	case <-time.After(time.Second):
		t.Fatal("stream did not close on disconnect")
	}
	if calls.Load() != 0 {
		t.Fatal("observing sync woke the worker")
	}

	request, _ = http.NewRequest("POST", app.URL+"/api/pull/trigger", strings.NewReader("{}"))
	request.SetBasicAuth("degitide", "test-secret")
	request.Header.Set("Origin", app.URL)
	request.Header.Set("Content-Type", "application/json")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var conflict map[string]string
	if err := json.NewDecoder(response.Body).Decode(&conflict); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusConflict || conflict["code"] != "sync_in_progress" {
		t.Fatalf("sync conflict not identifiable: %d %v", response.StatusCode, conflict)
	}
}
