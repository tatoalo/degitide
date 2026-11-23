package db

import (
	"path/filepath"
	"testing"
	"time"
)

func TestLegacyAccountMigrationPreservesDataAndAllowsOverlappingIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	database, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	statements := []string{
		"DROP TABLE accounts", "DROP TABLE transactions", "DROP TABLE holdings",
		`CREATE TABLE transactions (id text PRIMARY KEY,type text,date datetime,product_id text,symbol text,quantity real,price real,currency text,fee real,counterparty text,raw_json text,created_at datetime)`,
		`CREATE TABLE holdings (snapshot_date datetime,product_id text,symbol text,quantity real,avg_price real,market_value real,currency text,PRIMARY KEY(snapshot_date,product_id))`,
		`INSERT INTO transactions VALUES ('same','BUY','2026-11-01','fund','VWCE',2,100,'EUR',1,'','{}','2026-11-01')`,
		`INSERT INTO holdings VALUES ('2026-11-01','fund','VWCE',2,100,220,'EUR')`,
	}
	for _, statement := range statements {
		if err := database.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := database.SetMeta("last_pull", "2026-11-01T06:00:00Z"); err != nil {
		t.Fatal(err)
	}
	database.Close()
	database, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var original Transaction
	if err := database.Where("id = ? AND account_id = ?", "same", "default").First(&original).Error; err != nil {
		t.Fatal(err)
	}
	if original.Symbol != "VWCE" || original.Price != 100 {
		t.Fatalf("migration changed record: %+v", original)
	}
	var account Account
	database.First(&account, "id = ?", "default")
	if account.LastPull != "2026-11-01T06:00:00Z" {
		t.Fatalf("last pull lost: %+v", account)
	}
	if err := database.UpsertTransactions([]Transaction{{AccountID: "second", ID: "same", Symbol: "OTHER", ProductID: "fund", Price: 55}}); err != nil {
		t.Fatal(err)
	}
	var count int64
	database.Model(&Transaction{}).Count(&count)
	if count != 2 {
		t.Fatalf("overlapping IDs clobbered each other: %d", count)
	}
	// Reopening must not recreate or remigrate the old records.
	database.Close()
	database, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	database.Model(&Transaction{}).Count(&count)
	if count != 2 {
		t.Fatal("migration not idempotent")
	}
}

func TestHoldingsAndMetadataStayWithinTheirAccount(t *testing.T) {
	database, err := New(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	day := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	for _, id := range []string{"default", "second"} {
		if err := database.ReplaceHoldings(day, []Holding{{ProductID: "same", MarketValue: 100}}, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := database.ReplaceHoldings(day, nil, "second"); err != nil {
		t.Fatal(err)
	}
	var list []Holding
	database.Find(&list)
	if len(list) != 1 || list[0].AccountID != "default" {
		t.Fatalf("account replacement crossed boundary: %+v", list)
	}
	if err := database.UpsertTransactions([]Transaction{{AccountID: "default", ID: "a", ProductID: "fund", Symbol: ""}, {AccountID: "second", ID: "a", ProductID: "fund", Symbol: "OTHER", Currency: "USD"}}); err != nil {
		t.Fatal(err)
	}
	var tx Transaction
	database.Where("account_id = ?", "default").First(&tx)
	if tx.Symbol != "" || tx.Currency != "" {
		t.Fatalf("cross-account metadata leak: %+v", tx)
	}
}
