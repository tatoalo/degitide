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
	"github.com/tatoalo/degitide/internal/market"
)

func TestPortfolioSnapshotUsesSelectedAccountAndLatestEmptySnapshot(t *testing.T) {
	database, err := db.New(filepath.Join(t.TempDir(), "snapshot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Create(&db.Account{ID: "second", Name: "Second"}).Error; err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	for _, account := range []string{"default", "second"} {
		if err := database.ReplaceHoldings(day, []db.Holding{{ProductID: account, Symbol: account, Quantity: 1, MarketValue: 50, Currency: "USD", ValueCurrency: "EUR"}}, account); err != nil {
			t.Fatal(err)
		}
		if err := database.UpsertTransactions([]db.Transaction{{AccountID: account, ID: account, Date: day, Type: "BUY", Symbol: account, Currency: "EUR", Quantity: 1, Price: 40}}); err != nil {
			t.Fatal(err)
		}
	}
	cash, portfolio, balance := 10.0, 50.0, 60.0
	if err := database.SaveBrokerValuation("second", &db.BrokerValuation{BaseCurrency: "EUR", PortfolioValue: &portfolio, CashBalance: &cash, AccountValue: &balance, FetchedAt: day, Source: "broker_frontend", PriceStatus: "quoted"}); err != nil {
		t.Fatal(err)
	}
	app := httptest.NewServer(New(database, browser.NewClient("http://127.0.0.1:1"), &fakeScheduler{}).Router())
	defer app.Close()
	read := func(account string) struct {
		Holdings     []db.Holding        `json:"holdings"`
		Transactions []db.Transaction    `json:"transactions"`
		History      []db.ValuePoint     `json:"history"`
		Valuation    *db.BrokerValuation `json:"valuation"`
	} {
		t.Helper()
		var result struct {
			Holdings     []db.Holding        `json:"holdings"`
			Transactions []db.Transaction    `json:"transactions"`
			History      []db.ValuePoint     `json:"history"`
			Valuation    *db.BrokerValuation `json:"valuation"`
		}
		response, err := http.Get(app.URL + "/api/portfolio/snapshot?account=" + account)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("status: %d", response.StatusCode)
		}
		if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	result := read("second")
	if len(result.Holdings) != 1 || result.Holdings[0].ProductID != "second" || len(result.Transactions) != 1 || result.Transactions[0].ID != "second" || result.Valuation == nil || *result.Valuation.AccountValue != 60 || len(result.History) != 1 || result.History[0].Currency != "EUR" {
		t.Fatalf("wrong account snapshot: %+v", result)
	}
	if value := read("default").Valuation; value != nil {
		t.Fatalf("valuation leaked: %+v", value)
	}
	if err := database.ReplaceHoldings(day.AddDate(0, 0, 1), []db.Holding{}, "second"); err != nil {
		t.Fatal(err)
	}
	// SnapshotDate is published by the scheduler with the complete sync.
	if err := database.Model(&db.Account{}).Where("id = ?", "second").Update("snapshot_date", day.AddDate(0, 0, 1)).Error; err != nil {
		t.Fatal(err)
	}
	result = read("second")
	if result.Holdings == nil || len(result.Holdings) != 0 || len(result.History) != 2 || result.History[1].Value != 0 {
		t.Fatalf("empty snapshot resurrected older holdings: %+v", result)
	}
	response, err := http.Get(app.URL + "/api/portfolio/snapshot?account=missing")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatal("unknown account fell back to default")
	}
}

func TestPortfolioSnapshotKeepsRawZeroAndDatedPreviousSession(t *testing.T) {
	database, err := db.New(filepath.Join(t.TempDir(), "session.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	day := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	amount := -4.25
	value := &db.BrokerValuation{BaseCurrency: "EUR", DayPL: &amount, FetchedAt: day.Add(17 * time.Hour), Source: "broker_frontend", PriceStatus: "quoted"}
	if err := database.SaveBrokerValuation("default", value); err != nil {
		t.Fatal(err)
	}
	amount = 0
	value.FetchedAt = day.Add(30 * time.Hour)
	if err := database.SaveBrokerValuation("default", value); err != nil {
		t.Fatal(err)
	}
	plan := &market.Schedule{GeneratedAt: value.FetchedAt, ValidUntil: day.Add(72 * time.Hour), HoldingCount: 1, Markets: []market.Exchange{{MIC: "XETR", Timezone: "Europe/Berlin", Sessions: []market.Session{
		{Open: day.Add(8 * time.Hour), Close: day.Add(16*time.Hour + 30*time.Minute)},
		{Open: day.Add(32 * time.Hour), Close: day.Add(40*time.Hour + 30*time.Minute)},
	}}}}
	if err := database.SaveMarketSchedule("default", plan); err != nil {
		t.Fatal(err)
	}
	app := New(database, nil, &fakeScheduler{}).Router()
	response := httptest.NewRecorder()
	app.ServeHTTP(response, httptest.NewRequest("GET", "http://127.0.0.1/api/portfolio/snapshot", nil))
	var result struct {
		Valuation *db.BrokerValuation `json:"valuation"`
		Previous  *db.SessionDayPL    `json:"previous_session_day_pl"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if response.Code != 200 || result.Valuation == nil || *result.Valuation.DayPL != 0 || result.Previous == nil || result.Previous.Value == nil || *result.Previous.Value != -4.25 || result.Previous.Date != "2026-11-17" {
		t.Fatalf("snapshot mixed periods: %s", response.Body.String())
	}
}
