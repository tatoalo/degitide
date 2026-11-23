package db

import (
	"path/filepath"
	"testing"
	"time"
)

func TestValueHistoryMergesReportsWithSnapshots(t *testing.T) {
	database, err := New(filepath.Join(t.TempDir(), "degitide.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer database.Close()

	day := func(s string) time.Time {
		parsed, err := time.Parse("2006-01-02", s)
		if err != nil {
			t.Fatal(err)
		}
		return parsed
	}
	reports := []PositionReportResult{
		{Date: day("2024-06-28"), Available: true, Positions: []HistoricalPosition{
			{Identifier: "ZZ0000000001", Quantity: 8, Close: 20.25, Currency: "EUR", Value: 162.00},
			{Identifier: "ZZ0000000002", Quantity: 5, Close: 10.50, Currency: "EUR", Value: 52.50},
		}},
		{Date: day("2024-07-05"), Available: true, Positions: []HistoricalPosition{
			{Identifier: "ZZ0000000001", Quantity: 8, Close: 22, Currency: "EUR", Value: 176},
			{Identifier: "ZZ0000000003", Quantity: 1, Close: 100, Currency: "USD", Value: 100},
		}},
		{Date: day("2024-07-12"), Available: true, BaseCurrency: "EUR"},
	}
	for i := range reports {
		for j := range reports[i].Positions {
			position := &reports[i].Positions[j]
			position.BaseCurrency = position.Currency
			position.BaseValue = valuationNumber(position.Value)
		}
	}
	if err := database.SavePositionReports(DefaultAccountID, reports, time.Now()); err != nil {
		t.Fatalf("save reports: %v", err)
	}
	// A synced snapshot on 2024-07-05 must replace both report points of that day.
	if err := database.ReplaceHoldings(time.Date(2024, 7, 5, 0, 0, 0, 0, time.Local), []Holding{
		{ProductID: "1", Symbol: "NTH", Quantity: 9, MarketValue: 198, Currency: "EUR"},
	}, DefaultAccountID); err != nil {
		t.Fatalf("replace holdings: %v", err)
	}

	points, err := database.ValueHistory(DefaultAccountID)
	if err != nil {
		t.Fatalf("value history: %v", err)
	}
	want := []ValuePoint{
		{Date: "2024-06-28", Currency: "EUR", Value: 214.50, Source: "report"},
		{Date: "2024-07-05", Currency: "EUR", Value: 198, Source: "snapshot"},
		{Date: "2024-07-12", Currency: "EUR", Value: 0, Source: "report"},
	}
	if len(points) != len(want) {
		t.Fatalf("points = %+v, want %+v", points, want)
	}
	for i := range want {
		got := points[i]
		if got.Date != want[i].Date || got.Currency != want[i].Currency || got.Source != want[i].Source ||
			got.Value < want[i].Value-0.001 || got.Value > want[i].Value+0.001 {
			t.Fatalf("point %d = %+v, want %+v", i, got, want[i])
		}
	}

	covered, err := database.ValueHistoryCoverage(DefaultAccountID)
	if err != nil {
		t.Fatalf("coverage: %v", err)
	}
	for _, key := range []string{"2024-06-28", "2024-07-05", "2024-07-12"} {
		if !covered[key] {
			t.Fatalf("%s not covered: %v", key, covered)
		}
	}

	// Re-saving a date replaces its positions instead of duplicating them.
	if err := database.SavePositionReports(DefaultAccountID, reports[:1], time.Now()); err != nil {
		t.Fatalf("re-save reports: %v", err)
	}
	var count int64
	database.Model(&HistoricalPosition{}).Where("date = ?", Day(day("2024-06-28"))).Count(&count)
	if count != 2 {
		t.Fatalf("positions on re-saved date = %d, want 2", count)
	}
}

func TestFirstTransactionDate(t *testing.T) {
	database, err := New(filepath.Join(t.TempDir(), "degitide.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer database.Close()
	if _, ok, err := database.FirstTransactionDate(DefaultAccountID); err != nil || ok {
		t.Fatalf("empty account: ok=%v err=%v", ok, err)
	}
	if err := database.UpsertTransactions([]Transaction{
		{AccountID: DefaultAccountID, ID: "b", Date: time.Date(2023, 1, 5, 0, 0, 0, 0, time.UTC)},
		{AccountID: DefaultAccountID, ID: "a", Date: time.Date(2021, 11, 10, 0, 0, 0, 0, time.UTC)},
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	first, ok, err := database.FirstTransactionDate(DefaultAccountID)
	if err != nil || !ok || first.Format("2006-01-02") != "2021-11-10" {
		t.Fatalf("first = %v ok=%v err=%v", first, ok, err)
	}
}

func TestEmptySuccessfulSnapshotOverridesReportWithZero(t *testing.T) {
	database, err := New(filepath.Join(t.TempDir(), "empty.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	day := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	if err := database.ReplaceHoldings(day, []Holding{{ProductID: "sold", Currency: "EUR", MarketValue: 100}}); err != nil {
		t.Fatal(err)
	}
	if err := database.SavePositionReports(DefaultAccountID, []PositionReportResult{{Date: day, Available: true, Positions: []HistoricalPosition{{Identifier: "sold", Currency: "EUR", Value: 100}}}}, day); err != nil {
		t.Fatal(err)
	}
	if err := database.ReplaceHoldings(day, nil); err != nil {
		t.Fatal(err)
	}
	if err := database.Model(&Account{}).Where("id = ?", DefaultAccountID).Update("snapshot_date", day).Error; err != nil {
		t.Fatal(err)
	}
	points, err := database.ValueHistory(DefaultAccountID)
	if err != nil || len(points) != 1 || points[0].Date != "2026-11-16" || points[0].Value != 0 || points[0].Source != "snapshot" || points[0].Currency != "EUR" {
		t.Fatalf("sold holding survived: %+v %v", points, err)
	}
}

func TestEmptySnapshotSurvivesLaterRepurchaseAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty-history.db")
	database, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	for i, value := range []float64{100, 0, 150} {
		var holdings []Holding
		if value != 0 {
			holdings = []Holding{{ProductID: "fund", Currency: "EUR", ValueCurrency: "EUR", MarketValue: value}}
		}
		if err := database.ReplaceHoldings(day.AddDate(0, 0, i), holdings); err != nil {
			t.Fatal(err)
		}
	}
	database.Close()
	database, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	points, err := database.ValueHistory(DefaultAccountID)
	if err != nil || len(points) != 3 {
		t.Fatalf("history: %+v %v", points, err)
	}
	for i, value := range []float64{100, 0, 150} {
		if points[i].Value != value || points[i].Date != day.AddDate(0, 0, i).Format("2006-01-02") || points[i].Source != "snapshot" {
			t.Fatalf("empty day disappeared after repurchase: %+v", points)
		}
	}
	coverage, err := database.ValueHistoryCoverage(DefaultAccountID)
	if err != nil || !coverage["2026-11-14"] {
		t.Fatalf("empty snapshot was needlessly requested again: %v %v", coverage, err)
	}
}

func TestReportCoverageRefreshesOnlyLegacyValuations(t *testing.T) {
	database, err := New(filepath.Join(t.TempDir(), "coverage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	day := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	legacy := PositionReportResult{Date: day, Available: true, Positions: []HistoricalPosition{{Identifier: "foreign", Currency: "USD", Value: 100}}}
	current := PositionReportResult{Date: day.AddDate(0, 0, 7), Available: true, Positions: []HistoricalPosition{{Identifier: "foreign", Currency: "USD", Value: 100, BaseCurrency: "EUR", BaseValue: valuationNumber(90)}}}
	empty := PositionReportResult{Date: day.AddDate(0, 0, 14), Available: true, BaseCurrency: "EUR"}
	if err := database.SavePositionReports(DefaultAccountID, []PositionReportResult{legacy, current, empty}, day); err != nil {
		t.Fatal(err)
	}
	coverage, err := database.ValueHistoryCoverage(DefaultAccountID)
	if err != nil || coverage["2026-10-30"] || !coverage["2026-11-06"] || !coverage["2026-11-13"] {
		t.Fatalf("wrong refresh coverage: %v %v", coverage, err)
	}
	points, err := database.ValueHistory(DefaultAccountID)
	if err != nil || len(points) != 3 || points[0].Currency != "USD" || points[0].Value != 100 {
		t.Fatalf("legacy data disappeared before successful refresh: %+v %v", points, err)
	}
	legacy.Positions[0].BaseCurrency = "EUR"
	legacy.Positions[0].BaseValue = valuationNumber(85)
	if err := database.SavePositionReports(DefaultAccountID, []PositionReportResult{legacy}, day); err != nil {
		t.Fatal(err)
	}
	coverage, err = database.ValueHistoryCoverage(DefaultAccountID)
	if err != nil || !coverage["2026-10-30"] {
		t.Fatalf("verified refresh not covered: %v %v", coverage, err)
	}
	points, err = database.ValueHistory(DefaultAccountID)
	if err != nil || len(points) != 3 || points[0].Currency != "EUR" || points[0].Value != 85 {
		t.Fatalf("base value not used after refresh: %+v %v", points, err)
	}
}

func TestEmptyReportsRequireAvailableDataAndKnownCurrency(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty-reports.db")
	database, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if err := database.SavePositionReports(DefaultAccountID, []PositionReportResult{
		{Date: day, Available: true, BaseCurrency: "EUR"},
		{Date: day.AddDate(0, 0, 1), Available: false, BaseCurrency: "EUR"},
		{Date: day.AddDate(0, 0, 2), Available: true},
		{Date: day.AddDate(0, 0, 3), Available: true, BaseCurrency: "unknown"},
	}, day); err != nil {
		t.Fatal(err)
	}
	if err := database.SavePositionReports("second", []PositionReportResult{{Date: day, Available: true, BaseCurrency: "USD"}}, day); err != nil {
		t.Fatal(err)
	}
	database.Close()
	database, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	points, err := database.ValueHistory(DefaultAccountID)
	if err != nil || len(points) != 1 || points[0].Date != "2026-10-01" || points[0].Currency != "EUR" || points[0].Value != 0 || points[0].Source != "report" {
		t.Fatalf("empty report lost or missing report became a zero: %+v %v", points, err)
	}
	coverage, err := database.ValueHistoryCoverage(DefaultAccountID)
	if err != nil || !coverage["2026-10-01"] || !coverage["2026-10-02"] || coverage["2026-10-03"] || coverage["2026-10-04"] {
		t.Fatalf("missing currency not retried or unavailable report retried: %+v %v", coverage, err)
	}
	if err := database.ReplaceHoldings(day, []Holding{{ProductID: "fund", Currency: "EUR", MarketValue: 100}}); err != nil {
		t.Fatal(err)
	}
	points, err = database.ValueHistory(DefaultAccountID)
	if err != nil || len(points) != 1 || points[0].Value != 100 || points[0].Source != "snapshot" {
		t.Fatalf("empty report overrode newer snapshot: %+v %v", points, err)
	}
}
