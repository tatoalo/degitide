package db

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestNewRestrictsDatabasePermissions(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "private")
	path := filepath.Join(directory, "degitide.db")
	database, err := New(path)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer database.Close()

	dirInfo, err := os.Stat(directory)
	if err != nil {
		t.Fatalf("stat database directory: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("database directory mode = %o", got)
	}

	dbInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat database: %v", err)
	}
	if got := dbInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("database mode = %o", got)
	}
}

func TestConcurrentAdminClaimHasOneWinner(t *testing.T) {
	database, err := New(filepath.Join(t.TempDir(), "degitide.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer database.Close()

	const claimants = 8
	start := make(chan struct{})
	errorsByClaimant := make([]error, claimants)
	var wait sync.WaitGroup
	for i := range claimants {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, errorsByClaimant[i] = database.CreateAdmin(fmt.Sprintf("owner-%d", i), "password-hash")
		}()
	}
	close(start)
	wait.Wait()

	winners := 0
	for _, err := range errorsByClaimant {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, ErrAdminAlreadyExists):
		default:
			t.Fatalf("unexpected concurrent claim error: %v", err)
		}
	}
	if winners != 1 {
		t.Fatalf("admin claim winners = %d", winners)
	}
	admin, err := database.GetAdmin()
	if err != nil || admin == nil {
		t.Fatalf("stored admin = %#v, err = %v", admin, err)
	}
}

func TestCreateAdminCanOnlyClaimDatabaseOnce(t *testing.T) {
	database, err := New(filepath.Join(t.TempDir(), "degitide.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer database.Close()

	admin, err := database.GetAdmin()
	if err != nil || admin != nil {
		t.Fatalf("initial admin = %#v, err = %v", admin, err)
	}

	created, err := database.CreateAdmin("owner", "password-hash")
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	if created.ID != 1 || created.Username != "owner" || created.PasswordHash != "password-hash" {
		t.Fatalf("created admin = %#v", created)
	}

	stored, err := database.GetAdmin()
	if err != nil {
		t.Fatalf("get admin: %v", err)
	}
	if stored == nil || stored.Username != "owner" || stored.PasswordHash != "password-hash" {
		t.Fatalf("stored admin = %#v", stored)
	}

	if _, err := database.CreateAdmin("attacker", "other-hash"); !errors.Is(err, ErrAdminAlreadyExists) {
		t.Fatalf("second create error = %v", err)
	}
	stored, err = database.GetAdmin()
	if err != nil || stored == nil || stored.Username != "owner" {
		t.Fatalf("admin after second create = %#v, err = %v", stored, err)
	}
}

func TestBackfillTransactionMetadata(t *testing.T) {
	d, err := New(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	rows := []Transaction{
		{ID: "old", ProductID: "123", Symbol: "123"},
		{ID: "new", ProductID: "123", Symbol: "VWCE", Currency: "EUR"},
		{ID: "other", ProductID: "456", Symbol: "456"},
		{ID: "ambiguous", ProductID: "789", Symbol: "789"},
		{ID: "eur", ProductID: "789", Symbol: "ABC", Currency: "EUR"},
		{ID: "usd", ProductID: "789", Symbol: "ABC", Currency: "USD"},
	}
	if err := d.UpsertTransactions(rows); err != nil {
		t.Fatal(err)
	}
	var old, other, ambiguous Transaction
	d.First(&old, "id = ?", "old")
	d.First(&other, "id = ?", "other")
	d.First(&ambiguous, "id = ?", "ambiguous")
	if old.Symbol != "VWCE" || old.Currency != "EUR" {
		t.Fatalf("metadata not repaired: %+v", old)
	}
	if other.Symbol != "456" || other.Currency != "" {
		t.Fatal("unrelated product changed")
	}
	if ambiguous.Currency != "" {
		t.Fatal("ambiguous currency guessed")
	}
}

func TestRepairHoldingTransportLabel(t *testing.T) {
	d, err := New(filepath.Join(t.TempDir(), "labels.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Create(&Transaction{ID: "trade", ProductID: "123", Symbol: "VWCE"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := d.Create(&[]Holding{{ProductID: "123", Symbol: "positionrow"}, {ProductID: "999", Symbol: "positionrow"}, {ProductID: "456", Symbol: "KEEP"}}).Error; err != nil {
		t.Fatal(err)
	}
	if err := d.RepairHoldingLabels(); err != nil {
		t.Fatal(err)
	}
	var repaired, unknown, preserved Holding
	d.First(&repaired, "product_id = ?", "123")
	d.First(&unknown, "product_id = ?", "999")
	d.First(&preserved, "product_id = ?", "456")
	if repaired.Symbol != "VWCE" || unknown.Symbol != "positionrow" || preserved.Symbol != "KEEP" {
		t.Fatal("incorrect label repair")
	}
}
