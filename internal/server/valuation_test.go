package server

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

func TestPortfolioValuationIsAccountScopedAndReadOnly(t *testing.T) {
	database, err := db.New(filepath.Join(t.TempDir(), "valuation.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Create(&db.Account{ID: "second", Name: "Second"}).Error; err != nil {
		t.Fatal(err)
	}
	balance := 123.45
	if err := database.SaveBrokerValuation("second", &db.BrokerValuation{BaseCurrency: "EUR", AccountValue: &balance, FetchedAt: time.Now().UTC(), Source: "broker_frontend", PriceStatus: "quoted"}); err != nil {
		t.Fatal(err)
	}
	app := httptest.NewServer(New(database, browser.NewClient("http://127.0.0.1:1"), &fakeScheduler{}).Router())
	defer app.Close()
	for _, account := range []string{"default", "second", "missing"} {
		response, err := http.Get(app.URL + "/api/portfolio/valuation?account=" + account)
		if err != nil {
			t.Fatal(err)
		}
		if account == "missing" {
			response.Body.Close()
			if response.StatusCode != 404 {
				t.Fatal("unknown account fell back to default")
			}
			continue
		}
		var value *db.BrokerValuation
		err = json.NewDecoder(response.Body).Decode(&value)
		response.Body.Close()
		if err != nil || response.StatusCode != 200 {
			t.Fatalf("valuation response: %d %v", response.StatusCode, err)
		}
		if account == "default" && value != nil {
			t.Fatal("cross-account valuation leak")
		}
		if account == "second" && (value == nil || value.AccountValue == nil || *value.AccountValue != balance || value.TotalPL != nil) {
			t.Fatalf("missing values became zero: %+v", value)
		}
	}
}
