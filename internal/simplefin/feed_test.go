package simplefin

import (
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/tatoalo/degitide/internal/db"
)

func TestFeedKeepsMarketCurrencySeparateFromForeignCostBasis(t *testing.T) {
	database, err := db.New(filepath.Join(t.TempDir(), "feed.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	now := time.Now().UTC().Truncate(time.Second)
	day := db.Day(now)
	if err := database.ReplaceHoldings(day, []db.Holding{
		{ProductID: "foreign", Symbol: "USD fund", Quantity: 2, AvgPrice: 100, Currency: "USD", ValueCurrency: "EUR", MarketValue: 220},
		{ProductID: "native", Symbol: "EUR fund", Quantity: 1, AvgPrice: 90, Currency: "EUR", ValueCurrency: "EUR", MarketValue: 110},
	}, "default"); err != nil {
		t.Fatal(err)
	}
	if err := database.Model(&db.Account{}).Where("id = ?", "default").Updates(map[string]any{"last_pull": now.Format(time.RFC3339), "snapshot_date": day}).Error; err != nil {
		t.Fatal(err)
	}
	feed, err := Build(database, db.SimpleFINConnection{AccountIDs: `["default"]`}, Window{}, now)
	if err != nil || len(feed.Accounts) != 1 || len(feed.Errors) != 0 {
		t.Fatalf("feed: %+v %v", feed, err)
	}
	account := feed.Accounts[0]
	if account.Currency != "EUR" || account.Balance != "330.00000000" || len(account.Holdings) != 2 {
		t.Fatalf("market value currency mislabeled: %+v", account)
	}
	if account.Holdings[0].Description != "USD fund" || account.Holdings[0].Currency != "EUR" || account.Holdings[0].CostBasis != "" {
		t.Fatalf("foreign cost basis labeled as EUR: %+v", account.Holdings[0])
	}
	if account.Holdings[1].CostBasis != "90" {
		t.Fatalf("same-currency cost basis lost: %+v", account.Holdings[1])
	}
}

func TestFeedExportsFullSavedValuationHistoryWithoutSpendingActivity(t *testing.T) {
	database, err := db.New(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	now := time.Now().UTC().Truncate(time.Second)
	day := db.Day(now)
	older := day.AddDate(-2, 0, 0)
	if err := database.SavePositionReports("default", []db.PositionReportResult{
		{Date: older, Available: true, Positions: []db.HistoricalPosition{{Identifier: "fund", Currency: "EUR", Value: 70}, {Identifier: "foreign", Currency: "USD", Value: 12}}},
		{Date: older.AddDate(0, 0, 7), Available: true, BaseCurrency: "EUR"},
		{Date: older.AddDate(0, 0, 14), Available: false, BaseCurrency: "EUR"},
		{Date: older.AddDate(0, 0, 21), Available: true},
		{Date: day.AddDate(0, 0, -2), Available: true, Positions: []db.HistoricalPosition{{Identifier: "fund", Currency: "EUR", Value: 999}}},
	}, now); err != nil {
		t.Fatal(err)
	}
	if err := database.SavePositionReports("second", []db.PositionReportResult{{Date: older, Available: true, Positions: []db.HistoricalPosition{{Identifier: "fund", Currency: "EUR", Value: 99999}}}}, now); err != nil {
		t.Fatal(err)
	}
	for i, value := range []float64{80, 0, 100} {
		var holdings []db.Holding
		if value > 0 {
			holdings = []db.Holding{{ProductID: "fund", Currency: "EUR", MarketValue: value, Quantity: 1}}
		}
		if err := database.ReplaceHoldings(day.AddDate(0, 0, i-2), holdings, "default"); err != nil {
			t.Fatal(err)
		}
	}
	if err := database.UpsertTransactions([]db.Transaction{{AccountID: "default", ID: "buy", Type: "BUY", Date: older, Currency: "EUR", Quantity: 1, Price: 70}}); err != nil {
		t.Fatal(err)
	}
	if err := database.Model(&db.Account{}).Where("id = ?", "default").Updates(map[string]any{"last_pull": now.Format(time.RFC3339), "snapshot_date": day}).Error; err != nil {
		t.Fatal(err)
	}
	window := Window{Start: now.AddDate(0, 0, -1).Unix(), End: now.Unix(), BalancesOnly: true}
	feed, err := Build(database, db.SimpleFINConnection{AccountIDs: `["default"]`, IncludeTransactions: false}, window, now)
	if err != nil || len(feed.Accounts) != 1 || len(feed.Errors) != 0 {
		t.Fatalf("history feed: %+v %v", feed, err)
	}
	account := feed.Accounts[0]
	if len(account.Transactions) != 0 {
		t.Fatalf("valuation history became spending: %+v", account.Transactions)
	}
	extra := account.Extra["degitide"].(map[string]any)
	if extra["version"] != 1 || extra["history_version"] != 1 || extra["history_scope"] != "securities_only" {
		t.Fatalf("wrong extension contract: %+v", extra)
	}
	history := extra["balance_history"].([]BalanceHistoryPoint)
	if len(history) != 5 || history[0].Date != older.Format("2006-01-02") || history[0].Balance != "70" || history[0].Source != "report" || history[1].Date != older.AddDate(0, 0, 7).Format("2006-01-02") || history[1].Balance != "0" || history[1].Source != "report" || history[2].Balance != "80" || history[3].Balance != "0" || history[4].Balance != "100" {
		t.Fatalf("history truncated, cross-account/currency leak or report overrode snapshot: %+v", history)
	}
}

func TestBalanceHistoryRejectsInvalidOrInventedValues(t *testing.T) {
	day := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	for _, points := range [][]db.ValuePoint{
		{{Date: "2026-11-13", Currency: "EUR", Value: math.Inf(1), Source: "snapshot"}},
		{{Date: "2026-11-13", Currency: "EUR", Value: -1, Source: "report"}},
		{{Date: "invalid", Currency: "EUR", Value: 100, Source: "report"}},
		{{Date: "2026-11-17", Currency: "EUR", Value: 100, Source: "report"}},
		{{Date: "2026-11-13", Currency: "EUR", Value: 100, Source: "estimated"}},
		{{Date: "2026-11-13", Currency: "EUR", Value: 100, Source: "report"}, {Date: "2026-11-13", Currency: "EUR", Value: 101, Source: "snapshot"}},
		{{Date: "2026-11-14", Currency: "EUR", Value: 100, Source: "report"}, {Date: "2026-11-13", Currency: "EUR", Value: 101, Source: "snapshot"}},
	} {
		if history, err := balanceHistory(points, "EUR", day); err == nil || history != nil {
			t.Fatalf("invalid history accepted: %+v %v", points, err)
		}
	}
}
