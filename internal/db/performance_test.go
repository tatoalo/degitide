package db

import (
	"fmt"
	"math"
	"path/filepath"
	"testing"
	"time"
)

func TestValueHistoryTradingPerformanceUsesFullAccountHistory(t *testing.T) {
	database, err := New(filepath.Join(t.TempDir(), "degitide.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	day := func(s string) time.Time { d, _ := time.Parse("2006-01-02", s); return d }
	trades := make([]Transaction, 0, 110)
	// More than the dashboard's latest-100 page, all before the chart window.
	for i := 0; i < 105; i++ {
		trades = append(trades, Transaction{ID: fmt.Sprint(i), Type: "BUY", Date: day("2025-01-01"), Quantity: 1, Price: 10, Currency: "EUR", Fee: -1})
	}
	trades = append(trades,
		Transaction{ID: "sale", Type: "SELL", Date: day("2025-02-01"), Quantity: -5, Price: 20, Currency: "EUR"},
		Transaction{ID: "usd", Type: "BUY", Date: day("2025-01-01"), Quantity: 2, Price: 50, Currency: "USD"},
		Transaction{ID: "dividend", Type: "DIVIDEND", Date: day("2025-01-01"), Quantity: 1, Price: 500, Currency: "EUR"},
		Transaction{ID: "future", Type: "BUY", Date: day("2025-03-01"), Quantity: 1, Price: 1000, Currency: "EUR"},
		Transaction{AccountID: "other", ID: "other", Type: "BUY", Date: day("2025-01-01"), Quantity: 1, Price: 10000, Currency: "EUR"},
	)
	if err := database.UpsertTransactions(trades); err != nil {
		t.Fatal(err)
	}
	if err := database.SavePositionReports(DefaultAccountID, []PositionReportResult{
		{Date: day("2024-12-31"), Available: true, Positions: []HistoricalPosition{{Identifier: "pre", Currency: "EUR", Value: 50}}},
		{Date: day("2025-01-31"), Available: true, Positions: []HistoricalPosition{{Identifier: "eur", Currency: "EUR", Value: 1200}}},
		{Date: day("2025-02-01"), Available: true, Positions: []HistoricalPosition{
			{Identifier: "eur", Currency: "EUR", Value: 1300},
			{Identifier: "usd", Currency: "USD", Value: 90},
			{Identifier: "gbp", Currency: "GBP", Value: 80},
		}},
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
	// A partial import must not manufacture a profit from an incomplete basis.
	points, err := database.ValueHistory(DefaultAccountID)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range points {
		if p.NetInvested != nil || p.ProfitLoss != nil {
			t.Fatalf("incomplete import has P&L: %+v", p)
		}
	}
	if err := database.SetMeta("transactions_backfilled:"+DefaultAccountID, "2025-02-02"); err != nil {
		t.Fatal(err)
	}
	points, err = database.ValueHistory(DefaultAccountID)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][2]float64{"2025-01-31/EUR": {1050, 150}, "2025-02-01/EUR": {950, 350}, "2025-02-01/USD": {100, -10}}
	checked := 0
	for _, p := range points {
		if expected, ok := want[p.Date+"/"+p.Currency]; ok {
			if p.NetInvested == nil || p.ProfitLoss == nil || math.Abs(*p.NetInvested-expected[0]) > 0.001 || math.Abs(*p.ProfitLoss-expected[1]) > 0.001 {
				t.Fatalf("point %+v does not match %v", p, expected)
			}
			checked++
		} else if p.NetInvested != nil || p.ProfitLoss != nil {
			t.Fatalf("unknown basis has P&L: %+v", p)
		}
	}
	if checked != len(want) {
		t.Fatalf("checked %d of %d points", checked, len(want))
	}
}

func TestTradePerformanceRetainsZeroAndNegativeNetInvested(t *testing.T) {
	database, err := New(filepath.Join(t.TempDir(), "degitide.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.SetMeta("transactions_backfilled:"+DefaultAccountID, "complete"); err != nil {
		t.Fatal(err)
	}
	for i, trade := range []Transaction{
		{Type: "BUY", Quantity: 1, Price: 100},
		{Type: "SELL", Quantity: 1, Price: 100},
		{Type: "BUY", Quantity: 1, Price: 100},
		{Type: "SELL", Quantity: 1, Price: 150},
	} {
		trade.ID, trade.Currency = fmt.Sprint(i), "EUR"
		trade.Date = time.Date(2025, 1, i+1, 0, 0, 0, 0, time.UTC)
		if err := database.UpsertTransactions([]Transaction{trade}); err != nil {
			t.Fatal(err)
		}
	}
	points := []ValuePoint{{Date: "2025-01-02", Currency: "EUR", Value: 0}, {Date: "2025-01-04", Currency: "EUR", Value: 0}}
	if err := database.addTradePerformance(DefaultAccountID, points); err != nil {
		t.Fatal(err)
	}
	if points[0].NetInvested == nil || *points[0].NetInvested != 0 || *points[0].ProfitLoss != 0 {
		t.Fatalf("zero net invested: %+v", points[0])
	}
	if points[1].NetInvested == nil || *points[1].NetInvested != -50 || *points[1].ProfitLoss != 50 {
		t.Fatalf("negative net invested: %+v", points[1])
	}
}
