package scheduler

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/tatoalo/degitide/internal/browser"
	"github.com/tatoalo/degitide/internal/db"
)

func TestTriggerNowPersistsCompletePull(t *testing.T) {
	workerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/tasks/check-session":
			_ = json.NewEncoder(w).Encode(map[string]bool{"valid": true})
		case "/tasks/fetch-transactions":
			_ = json.NewEncoder(w).Encode(map[string]any{"transactions": []map[string]any{
				{
					"id": "tx-1", "type": "BUY", "date": "2026-08-24",
					"product_id": "1234", "symbol": "EXAMPLE", "quantity": 2, "price": 10.5,
					"currency": "EUR", "fee": -1.0, "raw_json": "{}",
				},
			}})
		case "/tasks/fetch-holdings":
			_ = json.NewEncoder(w).Encode(map[string]any{"holdings": []map[string]any{
				{
					"product_id": "1234", "symbol": "EXAMPLE", "quantity": 2, "avg_price": 10.5,
					"market_value": 22.0, "currency": "EUR",
				},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer workerServer.Close()

	database, err := db.New(filepath.Join(t.TempDir(), "degitide.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer database.Close()

	scheduler := New(database, browser.NewClient(workerServer.URL), "0 6 * * *")
	defer scheduler.Stop()
	if err := scheduler.TriggerNow(); err != nil {
		t.Fatalf("trigger pull: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for scheduler.IsRunning() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if scheduler.IsRunning() {
		t.Fatal("pull did not finish")
	}

	count, err := database.CountTransactions()
	if err != nil || count != 1 {
		t.Fatalf("transaction count = %d, err = %v", count, err)
	}
	var holdings []db.Holding
	if err := database.Find(&holdings).Error; err != nil || len(holdings) != 1 {
		t.Fatalf("holdings = %d, err = %v", len(holdings), err)
	}
	lastPull, err := database.GetMeta("last_pull")
	if err != nil || lastPull == "" {
		t.Fatalf("last_pull = %q, err = %v", lastPull, err)
	}
}

func TestStopCancelsAndWaitsForActivePull(t *testing.T) {
	started := make(chan struct{})
	workerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer workerServer.Close()

	database, err := db.New(filepath.Join(t.TempDir(), "degitide.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer database.Close()

	scheduler := New(database, browser.NewClient(workerServer.URL), "0 6 * * *")
	if err := scheduler.TriggerNow(); err != nil {
		t.Fatalf("trigger pull: %v", err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("worker request did not start")
	}

	scheduler.Stop()
	if scheduler.IsRunning() {
		t.Fatal("scheduler still running after Stop")
	}
	if err := scheduler.TriggerNow(); !errors.Is(err, ErrSchedulerStopped) {
		t.Fatalf("trigger after Stop error = %v", err)
	}
}

func TestDublinScheduleTracksDaylightSavingTime(t *testing.T) {
	scheduler := New(nil, nil, "CRON_TZ=Europe/Dublin 0 6 * * *")
	defer scheduler.cancel()

	entryID, err := scheduler.cron.AddFunc(scheduler.Settings("").Settings.Cron, func() {})
	if err != nil {
		t.Fatalf("parse Dublin schedule: %v", err)
	}
	schedule := scheduler.cron.Entry(entryID).Schedule

	winter := schedule.Next(time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC))
	if want := time.Date(2026, time.January, 1, 6, 0, 0, 0, time.UTC); !winter.Equal(want) {
		t.Fatalf("winter next run = %s, want %s", winter, want)
	}

	summer := schedule.Next(time.Date(2026, time.August, 26, 0, 0, 0, 0, time.UTC))
	if want := time.Date(2026, time.August, 26, 5, 0, 0, 0, time.UTC); !summer.Equal(want) {
		t.Fatalf("summer next run = %s, want %s", summer, want)
	}
}

func TestTransactionWindowBackfillsOnceThenResumesFromLastPull(t *testing.T) {
	var fromDates []string
	workerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/tasks/check-session":
			_ = json.NewEncoder(w).Encode(map[string]bool{"valid": true})
		case "/tasks/fetch-transactions":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			fromDates = append(fromDates, body["from"])
			_ = json.NewEncoder(w).Encode(map[string]any{"transactions": []map[string]any{}})
		case "/tasks/fetch-holdings":
			_ = json.NewEncoder(w).Encode(map[string]any{"holdings": []map[string]any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer workerServer.Close()

	database, err := db.New(filepath.Join(t.TempDir(), "degitide.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer database.Close()

	scheduler := New(database, browser.NewClient(workerServer.URL), "0 6 * * *")
	defer scheduler.Stop()
	pull := func() {
		t.Helper()
		if err := scheduler.TriggerNow(); err != nil {
			t.Fatalf("trigger pull: %v", err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for scheduler.IsRunning() && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if scheduler.IsRunning() {
			t.Fatal("pull did not finish")
		}
	}

	// First pull: the full history, and the backfill is recorded.
	pull()
	if got := fromDates[0]; got != "2000-01-01" {
		t.Fatalf("first pull from = %q, want full history", got)
	}
	if done, err := database.GetMeta(transactionBackfillKey(db.DefaultAccountID)); err != nil || done == "" {
		t.Fatalf("backfill meta = %q, err = %v", done, err)
	}

	// Next pull: the regular one-week window.
	pull()
	weekAgo := time.Now().AddDate(0, 0, -transactionOverlapDays).Format("2006-01-02")
	if got := fromDates[1]; got != weekAgo {
		t.Fatalf("second pull from = %q, want %q", got, weekAgo)
	}

	// After a month without a successful pull, resume a week before it.
	lastPull := time.Now().AddDate(0, 0, -30)
	if err := database.Model(&db.Account{}).Where("id = ?", db.DefaultAccountID).
		Update("last_pull", lastPull.Format(time.RFC3339)).Error; err != nil {
		t.Fatalf("set last_pull: %v", err)
	}
	pull()
	want := lastPull.AddDate(0, 0, -transactionOverlapDays).Format("2006-01-02")
	if got := fromDates[2]; got != want {
		t.Fatalf("pull after outage from = %q, want %q", got, want)
	}
}

func TestTransactionWindowFailedPullDoesNotRecordBackfill(t *testing.T) {
	workerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/tasks/check-session":
			_ = json.NewEncoder(w).Encode(map[string]bool{"valid": true})
		case "/tasks/fetch-transactions":
			_ = json.NewEncoder(w).Encode(map[string]any{"transactions": []map[string]any{}})
		default:
			http.Error(w, `{"error":"holdings unavailable"}`, http.StatusBadGateway)
		}
	}))
	defer workerServer.Close()

	database, err := db.New(filepath.Join(t.TempDir(), "degitide.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer database.Close()

	scheduler := New(database, browser.NewClient(workerServer.URL), "0 6 * * *")
	defer scheduler.Stop()
	if err := scheduler.TriggerNow(); err != nil {
		t.Fatalf("trigger pull: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for scheduler.IsRunning() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	// The holdings fetch failed, so the next pull must retry the full history.
	if done, err := database.GetMeta(transactionBackfillKey(db.DefaultAccountID)); err != nil || done != "" {
		t.Fatalf("backfill meta after failed pull = %q, err = %v", done, err)
	}
}

func TestValueHistoryDatesAreWeeklyNewestFirstAndSkipCoveredDays(t *testing.T) {
	first := time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC) // a Wednesday
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC) // a Friday: today is excluded
	got := valueHistoryDates(first, now, map[string]bool{"2026-09-11": true})
	want := []string{"2026-09-18", "2026-09-04"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("dates = %v, want %v", got, want)
	}
}

func TestPullBackfillsValueHistoryOncePerDate(t *testing.T) {
	var requested [][]string
	workerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/tasks/check-session":
			_ = json.NewEncoder(w).Encode(map[string]bool{"valid": true})
		case "/tasks/fetch-transactions":
			first := time.Now().AddDate(0, 0, -30).Format("2006-01-02")
			_ = json.NewEncoder(w).Encode(map[string]any{"transactions": []map[string]any{
				{"id": "tx-1", "type": "BUY", "date": first, "product_id": "1", "symbol": "VWCE",
					"quantity": 1, "price": 100, "currency": "EUR", "raw_json": "{}"},
			}})
		case "/tasks/fetch-holdings":
			_ = json.NewEncoder(w).Encode(map[string]any{"holdings": []map[string]any{
				{"product_id": "1", "symbol": "VWCE", "quantity": 1, "avg_price": 100, "market_value": 110, "currency": "EUR"},
			}})
		case "/tasks/fetch-positions":
			var body struct {
				Dates []string `json:"dates"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			requested = append(requested, body.Dates)
			snapshots := []map[string]any{}
			for _, day := range body.Dates[1:] { // the first (newest) requested date is "unavailable"
				snapshots = append(snapshots, map[string]any{"date": day, "base_currency": "EUR", "positions": []map[string]any{
					{"identifier": "IE00BK5BQT80", "name": "VANGUARD", "quantity": 1, "close": 100, "currency": "EUR", "value": 100, "base_value": 100},
				}})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"snapshots": snapshots, "unavailable": body.Dates[:1]})
		default:
			http.NotFound(w, r)
		}
	}))
	defer workerServer.Close()

	database, err := db.New(filepath.Join(t.TempDir(), "degitide.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer database.Close()
	scheduler := New(database, browser.NewClient(workerServer.URL), "0 6 * * *")
	defer scheduler.Stop()
	pull := func() {
		t.Helper()
		if err := scheduler.TriggerNow(); err != nil {
			t.Fatalf("trigger pull: %v", err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for scheduler.IsRunning() && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
	}

	pull()
	if len(requested) != 1 || len(requested[0]) < 4 {
		t.Fatalf("first pull requested %v, want the weekly dates of the last month", requested)
	}
	for _, day := range requested[0] {
		parsed, _ := time.Parse("2006-01-02", day)
		if parsed.Weekday() != time.Friday {
			t.Fatalf("requested non-Friday %s", day)
		}
	}
	points, err := database.ValueHistory(db.DefaultAccountID)
	if err != nil || len(points) != len(requested[0]) { // reports minus the unavailable one, plus today's snapshot
		t.Fatalf("points = %+v, err = %v", points, err)
	}

	// The next pull only retries the unavailable date.
	pull()
	if retried := requested[0][0]; len(requested) != 2 || len(requested[1]) != 1 || requested[1][0] != retried {
		t.Fatalf("second pull requested %v, want only %s", requested, retried)
	}
}
