package scheduler

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/tatoalo/degitide/internal/db"
)

func TestVerifiedTransactionBackfillMarker(t *testing.T) {
	completed := time.Date(2026, 11, 16, 16, 0, 0, 0, time.UTC)
	if got := transactionBackfillValue(completed); got != "v2:2026-11-16T16:00:00Z" || !verifiedTransactionBackfill(got) {
		t.Fatalf("invalid new marker: %q", got)
	}
	for _, old := range []string{"", "2026-11-16T16:00:00Z", "v2:", "v2:invalid", "v1:2026-11-16T16:00:00Z"} {
		if verifiedTransactionBackfill(old) {
			t.Fatalf("unverified marker accepted: %q", old)
		}
	}
}

func TestLegacyBackfillMarkerForcesOneVerifiedFullImport(t *testing.T) {
	database, err := db.New(filepath.Join(t.TempDir(), "windows.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	scheduler := New(database, nil, "0 6 * * *")
	defer scheduler.Stop()
	now := time.Date(2026, 11, 16, 16, 0, 0, 0, time.UTC)
	account := db.Account{ID: db.DefaultAccountID, LastPull: now.Add(-time.Hour).Format(time.RFC3339)}
	if err := database.SetMeta(transactionBackfillKey(account.ID), "2026-11-15T06:00:00Z"); err != nil {
		t.Fatal(err)
	}
	start, backfill, err := scheduler.transactionWindowStart(account, now)
	if err != nil || !backfill || !start.Equal(transactionHistoryStart) {
		t.Fatalf("legacy import not refreshed: %v, %v, %v", start, backfill, err)
	}
	if err := database.SetMeta(transactionBackfillKey(account.ID), transactionBackfillValue(now)); err != nil {
		t.Fatal(err)
	}
	start, backfill, err = scheduler.transactionWindowStart(account, now)
	want := now.Add(-time.Hour).AddDate(0, 0, -transactionOverlapDays)
	if err != nil || backfill || !start.Equal(want) {
		t.Fatalf("verified import did not resume incrementally: %v, %v, %v", start, backfill, err)
	}
	// Another account still needs its own verified full import.
	start, backfill, err = scheduler.transactionWindowStart(db.Account{ID: "savings"}, now)
	if err != nil || !backfill || !start.Equal(transactionHistoryStart) {
		t.Fatalf("account marker leaked: %v, %v, %v", start, backfill, err)
	}
}
