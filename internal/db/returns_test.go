package db

import (
	"math"
	"path/filepath"
	"testing"
	"time"
)

func TestPriceReturnsFromWeeklyClosesAndLatestSnapshot(t *testing.T) {
	database, err := New(filepath.Join(t.TempDir(), "degitide.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer database.Close()

	// Weekly closes for 3 years + 2 weeks, growing 10% a year; a second fund
	// held only for the last 3 weeks.
	end := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	start := end.AddDate(-3, 0, -14)
	var reports []PositionReportResult
	for day := start; !day.After(end); day = day.AddDate(0, 0, 7) {
		years := day.Sub(start).Hours() / 24 / 365.25
		positions := []HistoricalPosition{{Identifier: "ZZ0000000001", Name: "NORTH", Quantity: 1,
			Close: 100 * math.Pow(1.10, years), Currency: "EUR"}}
		if end.Sub(day) <= 14*24*time.Hour {
			positions = append(positions, HistoricalPosition{Identifier: "ZZ0000000002", Name: "SOUTH", Quantity: 1, Close: 50, Currency: "EUR"})
		}
		reports = append(reports, PositionReportResult{Date: day, Available: true, Positions: positions})
	}
	if err := database.SavePositionReports(DefaultAccountID, reports, time.Now()); err != nil {
		t.Fatalf("save reports: %v", err)
	}
	// A newer synced snapshot extends the South Fund series with its implied price.
	if err := database.ReplaceHoldings(time.Date(2026, 11, 16, 0, 0, 0, 0, time.Local), []Holding{
		{ProductID: "2", Symbol: "STH", ISIN: "ZZ0000000002", Quantity: 3, MarketValue: 165, Currency: "EUR"},
	}, DefaultAccountID); err != nil {
		t.Fatalf("replace holdings: %v", err)
	}

	returns, err := database.PriceReturns(DefaultAccountID)
	if err != nil {
		t.Fatalf("price returns: %v", err)
	}
	byID := map[string]PriceReturn{}
	for _, r := range returns {
		byID[r.Identifier] = r
	}

	north := byID["ZZ0000000001"]
	labels := []string{}
	for _, p := range north.Periods {
		labels = append(labels, p.Label)
		if p.Annualized == nil || math.Abs(*p.Annualized-10) > 0.05 {
			t.Fatalf("%s annualized = %v, want ~10%%", p.Label, p.Annualized)
		}
	}
	if len(labels) != 3 || labels[0] != "1 year" || labels[1] != "3 years" || labels[2] != "Since first held" {
		t.Fatalf("north periods = %v (5 years must be skipped, not measured from a stale start)", labels)
	}

	south := byID["ZZ0000000002"]
	if south.EndDate != "2026-11-16" || south.EndClose != 55 {
		t.Fatalf("south end = %s %v, want the snapshot's implied 55", south.EndDate, south.EndClose)
	}
	if len(south.Periods) != 1 || south.Periods[0].Annualized != nil || math.Abs(south.Periods[0].Total-10) > 1e-9 {
		t.Fatalf("south periods = %+v, want only an unannualized +10%% since first held", south.Periods)
	}
}

func TestPriceReturnsSkipsSinceFirstHeldWhenItRepeatsAPeriod(t *testing.T) {
	database, err := New(filepath.Join(t.TempDir(), "degitide.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer database.Close()
	// Held for exactly one year: "1 year" and "since first held" start together.
	end := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	var reports []PositionReportResult
	for day := end; !day.Before(end.AddDate(-1, 0, -6)); day = day.AddDate(0, 0, -7) {
		reports = append(reports, PositionReportResult{Date: day, Available: true, Positions: []HistoricalPosition{
			{Identifier: "ZZ0000000002", Quantity: 1, Close: 100, Currency: "EUR"},
		}})
	}
	if err := database.SavePositionReports(DefaultAccountID, reports, time.Now()); err != nil {
		t.Fatalf("save reports: %v", err)
	}
	returns, err := database.PriceReturns(DefaultAccountID)
	if err != nil || len(returns) != 1 {
		t.Fatalf("returns = %+v, err = %v", returns, err)
	}
	if periods := returns[0].Periods; len(periods) != 1 || periods[0].Label != "1 year" {
		t.Fatalf("periods = %+v, want only 1 year", periods)
	}
}

func TestPriceReturnsUseNativeQuoteAndQuoteDateForConvertedHoldings(t *testing.T) {
	database, err := New(filepath.Join(t.TempDir(), "quote.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	day := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	reports := []PositionReportResult{
		{Date: day, Available: true, Positions: []HistoricalPosition{{Identifier: "USD_FUND", Currency: "USD", Close: 100}}},
		{Date: day.AddDate(0, 0, 7), Available: true, Positions: []HistoricalPosition{{Identifier: "USD_FUND", Currency: "USD", Close: 105}}},
	}
	if err := database.SavePositionReports(DefaultAccountID, reports, day); err != nil {
		t.Fatal(err)
	}
	holding := Holding{ProductID: "usd", ISIN: "USD_FUND", Currency: "USD", ValueCurrency: "EUR", Quantity: 2, MarketValue: 190}
	snapshotDay := day.AddDate(0, 0, 10)
	if err := database.ReplaceHoldings(snapshotDay, []Holding{holding}); err != nil {
		t.Fatal(err)
	}
	returns, err := database.PriceReturns(DefaultAccountID)
	if err != nil || len(returns) != 1 || returns[0].EndClose != 105 || returns[0].EndDate != "2026-11-13" {
		t.Fatalf("EUR value used as USD price: %+v %v", returns, err)
	}
	price := 108.0
	for _, asOf := range []string{"2026-11-14T16:30:00Z", "2026-11-14"} {
		holding.Price, holding.PriceAsOf = &price, asOf
		if err := database.ReplaceHoldings(snapshotDay, []Holding{holding}); err != nil {
			t.Fatal(err)
		}
		returns, err = database.PriceReturns(DefaultAccountID)
		if err != nil || len(returns) != 1 || returns[0].EndClose != 108 || returns[0].EndDate != "2026-11-14" {
			t.Fatalf("native quote or quote date %s lost: %+v %v", asOf, returns, err)
		}
	}
}
