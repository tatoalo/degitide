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
	"github.com/tatoalo/degitide/internal/market"
)

func TestValuationAndHoldingsPublishAtomicallyAndFallbackClearsOldTotals(t *testing.T) {
	mode := "quoted"
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/tasks/check-session":
			json.NewEncoder(w).Encode(map[string]bool{"valid": true})
		case "/tasks/fetch-transactions":
			json.NewEncoder(w).Encode(map[string]any{"transactions": []any{map[string]any{"id": mode, "date": "2026-11-16", "type": "BUY", "product_id": "p", "symbol": "P", "quantity": 1, "price": 40, "currency": "EUR"}}})
		case "/tasks/fetch-holdings":
			valuation := map[string]any{"base_currency": "EUR", "portfolio_value": 50, "cash_balance": 10, "account_value": 60, "total_pl": 8, "day_pl": -1, "source": "broker_frontend", "price_status": "quoted", "fetched_at": "2026-11-16T16:00:00Z"}
			value := 50
			if mode == "rollback" {
				value = 70
				valuation["portfolio_value"] = 70
				valuation["account_value"] = 80
			}
			if mode == "fallback" {
				valuation = map[string]any{"base_currency": "EUR", "source": "broker_api", "price_status": "unavailable", "fetched_at": "2026-11-16T16:05:00Z"}
			}
			if mode == "invalid" {
				valuation["account_value"] = 999
			}
			now := time.Now()
			plan := &market.Schedule{GeneratedAt: now, ValidUntil: now.Add(28 * 24 * time.Hour), HoldingCount: 1, Markets: []market.Exchange{{MIC: "XETR", Name: "Xetra", Timezone: "Europe/Berlin", Sessions: []market.Session{{Open: now.Add(time.Hour), Close: now.Add(8 * time.Hour)}}}}}
			if mode == "rollback" {
				plan.Markets[0].MIC = "XLON"
			}
			if mode == "fallback" {
				plan = nil
			}
			json.NewEncoder(w).Encode(map[string]any{"holdings": []any{map[string]any{"product_id": "p", "symbol": "P", "quantity": 1, "avg_price": 40, "market_value": value, "currency": "EUR", "value_currency": "EUR", "exchange_mic": "XETR", "exchange_name": "Xetra"}}, "valuation": valuation, "market_schedule": plan})
		default:
			http.NotFound(w, r)
		}
	}))
	defer worker.Close()
	database, err := db.New(filepath.Join(t.TempDir(), "atomic-valuation.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	scheduler := New(database, browser.NewClient(worker.URL), "0 6 * * *")
	defer scheduler.Stop()
	pull := func(failed bool) {
		t.Helper()
		if err := scheduler.TriggerNow(); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for scheduler.IsRunning() && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if scheduler.IsRunning() || (scheduler.LastError() != "") != failed {
			t.Fatalf("pull failed=%t: %s", failed, scheduler.LastError())
		}
	}
	pull(false)
	if plan, err := database.MarketSchedule("default"); err != nil || plan == nil || plan.Markets[0].MIC != "XETR" {
		t.Fatalf("calendar not saved: %+v %v", plan, err)
	}
	// Force a failure after transaction and holdings writes, proving rollback of
	// the complete snapshot rather than only validation before the first write.
	if err := database.Exec(`CREATE TRIGGER reject_valuation BEFORE UPDATE ON broker_valuations BEGIN SELECT RAISE(ABORT, 'synthetic write failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	mode = "rollback"
	pull(true)
	mode = "invalid"
	pull(true)
	if plan, err := database.MarketSchedule("default"); err != nil || plan == nil || plan.Markets[0].MIC != "XETR" {
		t.Fatalf("failed snapshot changed calendar: %+v %v", plan, err)
	}
	count, err := database.CountTransactions()
	if err != nil || count != 1 {
		t.Fatalf("failed snapshot published trades: %d %v", count, err)
	}
	var holdings []db.Holding
	if err := database.Find(&holdings).Error; err != nil || len(holdings) != 1 || holdings[0].MarketValue != 50 {
		t.Fatalf("failed snapshot published holdings: %+v %v", holdings, err)
	}
	value, err := database.GetBrokerValuation(db.DefaultAccountID)
	if err != nil || value == nil || value.AccountValue == nil || *value.AccountValue != 60 {
		t.Fatalf("failed snapshot replaced valuation: %+v %v", value, err)
	}
	if err := database.Exec("DROP TRIGGER reject_valuation").Error; err != nil {
		t.Fatal(err)
	}
	mode = "fallback"
	pull(false)
	if plan, err := database.MarketSchedule("default"); err != nil || plan != nil {
		t.Fatalf("missing metadata retained old calendar: %+v %v", plan, err)
	}
	value, err = database.GetBrokerValuation(db.DefaultAccountID)
	if err != nil || value == nil || value.AccountValue != nil || value.TotalPL != nil || value.PriceStatus != "unavailable" {
		t.Fatalf("fallback retained stale totals: %+v %v", value, err)
	}
}
