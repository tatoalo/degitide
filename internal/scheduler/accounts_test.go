package scheduler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/tatoalo/degitide/internal/browser"
	"github.com/tatoalo/degitide/internal/db"
)

func TestAllAccountsContinueAfterFailureAndReportRealProgress(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("account") == "default" {
			if r.URL.Path != "/tasks/check-session" {
				t.Error("failed account continued fetching")
			}
			_ = json.NewEncoder(w).Encode(map[string]bool{"valid": false})
			return
		}
		switch r.URL.Path {
		case "/tasks/check-session":
			close(entered)
			<-release
			_ = json.NewEncoder(w).Encode(map[string]bool{"valid": true})
		case "/tasks/fetch-transactions":
			_ = json.NewEncoder(w).Encode(map[string]any{"transactions": []any{map[string]any{"id": "shared", "date": "2026-11-11", "product_id": "p", "symbol": "TEST", "currency": "EUR"}}})
		case "/tasks/fetch-holdings":
			_ = json.NewEncoder(w).Encode(map[string]any{"holdings": []any{map[string]any{"product_id": "p", "symbol": "TEST", "currency": "EUR", "quantity": 1, "market_value": 50}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer worker.Close()
	database, err := db.New(filepath.Join(t.TempDir(), "accounts.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Create(&db.Account{ID: "second", Name: "Savings"}).Error; err != nil {
		t.Fatal(err)
	}
	scheduler := New(database, browser.NewClient(worker.URL), "0 6 * * *")
	defer scheduler.Stop()
	if err := scheduler.TriggerNow(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("second account never ran")
	}
	progress := scheduler.Progress()
	if progress.Percent != 50 || progress.Accounts[0].State != "failed" || progress.Accounts[1].Stage != "Checking connection" {
		t.Fatalf("unexpected progress: %+v", progress)
	}
	if err := scheduler.TriggerNow(); err != ErrPullInProgress {
		t.Fatalf("overlap allowed: %v", err)
	}
	close(release)
	deadline := time.Now().Add(3 * time.Second)
	for scheduler.IsRunning() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond * 10)
	}
	if scheduler.IsRunning() {
		t.Fatal("sync did not finish")
	}
	progress = scheduler.Progress()
	if progress.Percent != 100 || progress.Running || progress.Accounts[1].State != "complete" || scheduler.LastError() == "" {
		t.Fatalf("bad final progress: %+v", progress)
	}
	var rows []db.Holding
	database.Find(&rows)
	if len(rows) != 1 || rows[0].AccountID != "second" {
		t.Fatalf("wrong account persisted: %+v", rows)
	}
	if err := scheduler.TriggerAccounts([]string{"missing"}); err == nil {
		t.Fatal("unknown account accepted")
	}
}

func TestShutdownDoesNotReportAccountFailure(t *testing.T) {
	entered := make(chan struct{})
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
	}))
	defer worker.Close()
	database, err := db.New(filepath.Join(t.TempDir(), "shutdown.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	scheduler := New(database, browser.NewClient(worker.URL), "0 6 * * *")
	scheduler.OnResult = func(db.Account, error) { t.Error("shutdown emitted an account result") }
	if err := scheduler.TriggerNow(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("worker not called")
	}
	scheduler.Stop()
	accounts, err := database.Accounts()
	if err != nil || accounts[0].LastError != "" {
		t.Fatalf("shutdown stored account failure: %+v, %v", accounts, err)
	}
	if scheduler.Progress().Stage != "Sync stopped" {
		t.Fatal("shutdown did not stop progress")
	}
}

func TestFailedHoldingsFetchDoesNotPublishPartialTransactions(t *testing.T) {
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/tasks/check-session":
			json.NewEncoder(w).Encode(map[string]bool{"valid": true})
		case "/tasks/fetch-transactions":
			json.NewEncoder(w).Encode(map[string]any{"transactions": []any{map[string]any{"id": "new", "date": "2026-11-11", "type": "BUY", "currency": "EUR"}}})
		default:
			http.Error(w, "holdings failed", 500)
		}
	}))
	defer worker.Close()
	database, err := db.New(filepath.Join(t.TempDir(), "atomic.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	scheduler := New(database, browser.NewClient(worker.URL), "0 6 * * *")
	defer scheduler.Stop()
	if err := scheduler.TriggerNow(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for scheduler.IsRunning() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if scheduler.IsRunning() || scheduler.LastError() == "" {
		t.Fatal("failed pull did not finish")
	}
	count, err := database.CountTransactions()
	if err != nil || count != 0 {
		t.Fatalf("partial pull became visible: %d, %v", count, err)
	}
}
