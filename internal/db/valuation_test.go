package db

import (
	"math"
	"path/filepath"
	"testing"
	"time"
)

func valuationNumber(value float64) *float64 { return &value }

func validValuation() *BrokerValuation {
	return &BrokerValuation{BaseCurrency: "EUR", PortfolioValue: valuationNumber(1050.00), CashBalance: valuationNumber(12.50), AccountValue: valuationNumber(1062.50), TotalPL: valuationNumber(75.25), FetchedAt: time.Date(2026, 11, 16, 16, 18, 0, 0, time.UTC), Source: "broker_frontend", PriceStatus: "quoted"}
}

func TestBrokerValuationReplacesUnknownFieldsAndClearsWithinAccount(t *testing.T) {
	database, err := New(filepath.Join(t.TempDir(), "valuation.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	for _, account := range []string{"default", "second"} {
		if err := database.SaveBrokerValuation(account, validValuation()); err != nil {
			t.Fatal(err)
		}
	}
	partial := validValuation()
	partial.TotalPL = nil
	partial.DayPL = valuationNumber(0)
	if err := database.SaveBrokerValuation("default", partial); err != nil {
		t.Fatal(err)
	}
	got, err := database.GetBrokerValuation("default")
	if err != nil || got == nil || got.TotalPL != nil || got.DayPL == nil || *got.DayPL != 0 {
		t.Fatalf("unknown and zero were conflated or previous P/L survived: %+v %v", got, err)
	}
	if err := database.SaveBrokerValuation("default", nil); err != nil {
		t.Fatal(err)
	}
	got, err = database.GetBrokerValuation("default")
	if err != nil || got != nil {
		t.Fatalf("stale totals survived: %+v %v", got, err)
	}
	got, err = database.GetBrokerValuation("second")
	if err != nil || got == nil || got.TotalPL == nil {
		t.Fatalf("other account changed: %+v %v", got, err)
	}
}

func TestBrokerValuationValidation(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*BrokerValuation)
	}{
		{"balance mismatch", func(v *BrokerValuation) { v.AccountValue = valuationNumber(1100.00) }},
		{"non-finite metric", func(v *BrokerValuation) { v.DayPL = valuationNumber(math.Inf(1)) }},
		{"non-finite reconciliation", func(v *BrokerValuation) { v.ReconciliationDifference = valuationNumber(math.NaN()) }},
		{"unknown base", func(v *BrokerValuation) { v.BaseCurrency = "" }},
		{"no collection time", func(v *BrokerValuation) { v.FetchedAt = time.Time{} }},
		{"unknown source", func(v *BrokerValuation) { v.Source = "estimated" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			v := validValuation()
			test.change(v)
			if err := v.Validate(); err == nil {
				t.Fatal("invalid valuation accepted")
			}
		})
	}
	v := validValuation()
	v.AccountValue = valuationNumber(*v.AccountValue + .02)
	if err := v.Validate(); err != nil {
		t.Fatalf("rounding tolerance rejected: %v", err)
	}
	v.TotalPL, v.DayPL, v.CashBalance = nil, nil, nil
	if err := v.Validate(); err != nil {
		t.Fatalf("unknown optional fields rejected: %v", err)
	}
}

func TestValueHistoryUsesActualValueCurrencyAndDoesNotInventFXCost(t *testing.T) {
	database, err := New(filepath.Join(t.TempDir(), "valuation.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	day := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	if err := database.SetMeta("transactions_backfilled:default", "verified"); err != nil {
		t.Fatal(err)
	}
	if err := database.UpsertTransactions([]Transaction{
		{AccountID: "default", ID: "eur", Date: day.AddDate(0, 0, -1), Type: "BUY", Currency: "EUR", Quantity: 1, Price: 100},
		{AccountID: "default", ID: "usd", Date: day.AddDate(0, 0, -1), Type: "BUY", Currency: "USD", Quantity: 1, Price: 100},
	}); err != nil {
		t.Fatal(err)
	}
	if err := database.SavePositionReports("default", []PositionReportResult{{Date: day, Available: true, Positions: []HistoricalPosition{
		{Identifier: "euro", Currency: "EUR", Value: 110, BaseCurrency: "EUR", BaseValue: valuationNumber(110)},
		{Identifier: "dollar", Currency: "USD", Value: 120, BaseCurrency: "EUR", BaseValue: valuationNumber(108)},
	}}}, day); err != nil {
		t.Fatal(err)
	}
	if err := database.ReplaceHoldings(day.AddDate(0, 0, 1), []Holding{
		{ProductID: "euro", Currency: "EUR", MarketValue: 111, ValueCurrency: "EUR"},
		{ProductID: "dollar", Currency: "USD", MarketValue: 109, ValueCurrency: "EUR"},
	}, "default"); err != nil {
		t.Fatal(err)
	}
	points, err := database.ValueHistory("default")
	if err != nil || len(points) != 2 {
		t.Fatalf("history: %+v %v", points, err)
	}
	for i, want := range []float64{218, 220} {
		if points[i].Currency != "EUR" || points[i].Value != want || points[i].NetInvested != nil || points[i].ProfitLoss != nil {
			t.Fatalf("foreign values mislabeled or foreign cost omitted: %+v", points[i])
		}
	}
	// Legacy snapshots still carry their old currency and are not reinterpreted.
	if err := database.ReplaceHoldings(day.AddDate(0, 0, -2), []Holding{{ProductID: "old", Currency: "USD", MarketValue: 99}}, "default"); err != nil {
		t.Fatal(err)
	}
	points, err = database.ValueHistory("default")
	if err != nil || len(points) != 3 || points[0].Currency != "USD" || points[0].Value != 99 {
		t.Fatalf("legacy value changed: %+v %v", points, err)
	}
	var currencies []string
	if err := database.Model(&PortfolioCurrency{}).Where("account_id = ?", "default").Order("currency").Pluck("currency", &currencies).Error; err != nil || len(currencies) != 2 || currencies[0] != "EUR" || currencies[1] != "USD" {
		t.Fatalf("market currencies not retained: %v %v", currencies, err)
	}
}
