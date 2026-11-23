package server

import (
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
)

func TestAccountAPIsAreScopedAndStatusDoesNotWakeWorker(t *testing.T) {
	var calls atomic.Int32
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); http.Error(w, "must not wake worker", 500) }))
	defer worker.Close()
	database, err := db.New(filepath.Join(t.TempDir(), "accounts.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Create(&db.Account{ID: "second", Name: "Savings"}).Error; err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	for _, id := range []string{"default", "second"} {
		if err := database.ReplaceHoldings(day, []db.Holding{{ProductID: "p", Symbol: id, Currency: "EUR", MarketValue: 100}}, id); err != nil {
			t.Fatal(err)
		}
	}
	app := httptest.NewServer(New(database, browser.NewClient(worker.URL), &fakeScheduler{}).Router())
	defer app.Close()
	response, err := http.Get(app.URL + "/api/holdings?account=second")
	if err != nil {
		t.Fatal(err)
	}
	var rows []db.Holding
	json.NewDecoder(response.Body).Decode(&rows)
	response.Body.Close()
	if len(rows) != 1 || rows[0].AccountID != "second" || rows[0].Symbol != "second" {
		t.Fatalf("cross-account holdings: %+v", rows)
	}
	for i := 0; i < 3; i++ {
		response, err = http.Get(app.URL + "/api/status?account=second")
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatal("status failed")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("status poll woke worker")
	}
	response, _ = http.Get(app.URL + "/api/holdings?account=missing")
	response.Body.Close()
	if response.StatusCode != 404 {
		t.Fatal("unknown account fell back to default")
	}
	request, _ := http.NewRequest("POST", app.URL+"/api/accounts", strings.NewReader(`{"id":"third","name":"Third account"}`))
	request.Header.Set("Origin", app.URL)
	request.Header.Set("Content-Type", "application/json")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 201 {
		t.Fatalf("create returned %d", response.StatusCode)
	}
}
