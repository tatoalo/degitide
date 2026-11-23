package db

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/tatoalo/degitide/internal/market"
)

func dayPLTime(s string) time.Time {
	at, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return at
}
func dayPLCalendar() *market.Schedule {
	return &market.Schedule{GeneratedAt: dayPLTime("2026-11-20T16:50:00Z"), ValidUntil: dayPLTime("2026-12-01T00:00:00Z"), HoldingCount: 1,
		Markets: []market.Exchange{{MIC: "XETR", Name: "Xetra", Timezone: "Europe/Berlin", Sessions: []market.Session{
			{Open: dayPLTime("2026-11-19T08:00:00Z"), Close: dayPLTime("2026-11-19T16:30:00Z")},
			{Open: dayPLTime("2026-11-20T08:00:00Z"), Close: dayPLTime("2026-11-20T16:30:00Z")},
			{Open: dayPLTime("2026-11-23T08:00:00Z"), Close: dayPLTime("2026-11-23T16:30:00Z")},
		}}}}
}

func TestPreviousSessionDayPL(t *testing.T) {
	for _, tc := range []struct {
		name, at string
		value    float64
		change   func(*market.Schedule)
		fallback bool
	}{
		{name: "overnight", at: "2026-11-21T06:00:00Z", fallback: true},
		{name: "weekend", at: "2026-11-22T12:00:00Z", fallback: true},
		{name: "Monday before open", at: "2026-11-23T06:00:00Z", value: -1e-12, fallback: true},
		{name: "true zero at open", at: "2026-11-23T08:00:00Z"},
		{name: "true zero after close", at: "2026-11-23T17:00:00Z"},
		{name: "nonzero overnight", at: "2026-11-23T06:00:00Z", value: 1},
		{name: "unknown holding", at: "2026-11-23T06:00:00Z", change: func(p *market.Schedule) { p.UnknownHoldings = 1 }},
		{name: "expired coverage", at: "2026-12-01T06:00:00Z"},
		{name: "lunch break", at: "2026-11-23T12:00:00Z", change: func(p *market.Schedule) {
			a, b := dayPLTime("2026-11-23T11:00:00Z"), dayPLTime("2026-11-23T13:00:00Z")
			p.Markets[0].Sessions[2].BreakStart = &a
			p.Markets[0].Sessions[2].BreakEnd = &b
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, err := New(filepath.Join(t.TempDir(), "daypl.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			plan := dayPLCalendar()
			if tc.change != nil {
				tc.change(plan)
			}
			if err := database.SaveMarketSchedule("default", plan); err != nil {
				t.Fatal(err)
			}
			saved := validValuation()
			saved.DayPL = valuationNumber(-4.25)
			saved.FetchedAt = dayPLTime("2026-11-20T16:50:00Z")
			if err := database.SaveBrokerValuation("default", saved); err != nil {
				t.Fatal(err)
			}
			// A later reading from another account/currency cannot leak into this result.
			other := *saved
			other.DayPL = valuationNumber(999)
			other.FetchedAt = other.FetchedAt.Add(time.Minute)
			if err := database.SaveBrokerValuation("second", &other); err != nil {
				t.Fatal(err)
			}
			other.BaseCurrency = "USD"
			if err := database.SaveBrokerValuation("default", &other); err != nil {
				t.Fatal(err)
			}
			current := validValuation()
			current.DayPL = valuationNumber(tc.value)
			current.FetchedAt = dayPLTime(tc.at)
			if err := database.SaveBrokerValuation("default", current); err != nil {
				t.Fatal(err)
			}
			got, err := database.PreviousSessionDayPL("default", current)
			if err != nil {
				t.Fatal(err)
			}
			if (got != nil) != tc.fallback {
				t.Fatalf("fallback=%+v", got)
			}
			if got != nil && (got.Date != "2026-11-20" || got.Value == nil || *got.Value != -4.25 || !got.RecordedAt.Equal(saved.FetchedAt)) {
				t.Fatalf("incorrect prior session: %+v", got)
			}
			raw, err := database.GetBrokerValuation("default")
			if err != nil || *raw.DayPL != tc.value {
				t.Fatalf("raw broker value changed: %+v %v", raw, err)
			}
		})
	}
}

func TestPreviousSessionDoesNotInventMissingOrNonzeroResult(t *testing.T) {
	database, err := New(filepath.Join(t.TempDir(), "daypl.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.SaveMarketSchedule("default", dayPLCalendar()); err != nil {
		t.Fatal(err)
	}
	current := validValuation()
	current.DayPL = valuationNumber(0)
	current.FetchedAt = dayPLTime("2026-11-23T06:00:00Z")
	old := *current
	old.DayPL = valuationNumber(200)
	old.FetchedAt = dayPLTime("2026-11-19T16:50:00Z")
	if err := database.SaveBrokerValuation("default", &old); err != nil {
		t.Fatal(err)
	}
	got, err := database.PreviousSessionDayPL("default", current)
	if err != nil || got == nil || got.Value != nil || got.Date != "2026-11-20" {
		t.Fatalf("invented missing session: %+v %v", got, err)
	}
	old.FetchedAt = dayPLTime("2026-11-20T16:00:00Z")
	if err := database.SaveBrokerValuation("default", &old); err != nil {
		t.Fatal(err)
	}
	old.FetchedAt = dayPLTime("2026-11-20T16:50:00Z")
	old.DayPL = valuationNumber(0)
	if err := database.SaveBrokerValuation("default", &old); err != nil {
		t.Fatal(err)
	}
	got, err = database.PreviousSessionDayPL("default", current)
	if err != nil || got.Value == nil || *got.Value != 0 {
		t.Fatalf("ignored the latest true zero: %+v %v", got, err)
	}
}

func TestDayPLUpgradeAndAtomicSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daypl.db")
	database, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	saved := validValuation()
	saved.AccountID = "default"
	saved.DayPL = valuationNumber(-20)
	if err := database.Save(saved).Error; err != nil {
		t.Fatal(err)
	} // legacy installation: no observation yet
	database.Close()
	database, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var count int64
	database.Model(&DayPLObservation{}).Count(&count)
	if count != 1 {
		t.Fatalf("upgrade did not retain current reading: %d", count)
	}
	if err := database.Exec(`CREATE TRIGGER reject_valuation BEFORE UPDATE ON broker_valuations BEGIN SELECT RAISE(ABORT, 'test'); END`).Error; err != nil {
		t.Fatal(err)
	}
	saved.FetchedAt = saved.FetchedAt.Add(time.Hour)
	if err := database.SaveBrokerValuation("default", saved); err == nil {
		t.Fatal("expected save failure")
	}
	database.Model(&DayPLObservation{}).Count(&count)
	if count != 1 {
		t.Fatal("failed snapshot left an observation behind")
	}
}
