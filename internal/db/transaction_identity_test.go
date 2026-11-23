package db

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// Generated with the old and new Python worker serializers. Includes floats,
// nested maps, literal backslashes, HTML characters and a Unicode line separator.
const identityRaw = `{"orderId":"order-1","quantity":2,"productId":1234,"date":"2026-08-24T10:20:30+02:00","buysell":"B","price":18.25,"currency":"EUR","note":"Crème <&>  literal\\u2028","extra":{"tiny":1e-07,"whole":1.0,"flag":true,"nil":null}}`
const identityCanonical = `{"buysell":"B","currency":"EUR","date":"2026-08-24T10:20:30+02:00","extra":{"flag":true,"nil":null,"tiny":1e-07,"whole":1.0},"note":"Crème <&>  literal\\u2028","orderId":"order-1","price":18.25,"productId":1234,"quantity":2}`
const identityLegacy = "derived-b2eb2aa7a19002ee7b0e1140"
const identityCurrent = "derived-ba64d1d1711ba2b00b20ae02"

func identityDB(t *testing.T) *DB {
	t.Helper()
	database, err := New(filepath.Join(t.TempDir(), "identities.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	return database
}

func identityRow(id, raw string) Transaction {
	return Transaction{
		AccountID: DefaultAccountID, ID: id, RawJSON: raw, ProductID: "1234", Type: "BUY",
		Date: time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC), Quantity: 2, Price: 18.25, Currency: "EUR",
	}
}

func TestCanonicalTransactionIdentityMatchesPython(t *testing.T) {
	got, err := canonicalTransactionJSON(identityRaw)
	if err != nil || string(got) != identityCanonical {
		t.Fatalf("canonical JSON mismatch: %s, %v", got, err)
	}
	if got := derivedTransactionID(got); got != identityCurrent {
		t.Fatalf("canonical ID = %q", got)
	}
	if got := derivedTransactionID([]byte(identityRaw)); got != identityLegacy {
		t.Fatalf("legacy ID = %q", got)
	}
}

func TestRepairLegacyTransactionIdentity(t *testing.T) {
	for _, legacyID := range []string{"order-1", identityLegacy} {
		t.Run(legacyID, func(t *testing.T) {
			database := identityDB(t)
			if err := database.Create(&[]Transaction{identityRow(legacyID, identityRaw)}).Error; err != nil {
				t.Fatal(err)
			}
			if err := database.RepairTransactionIdentities(DefaultAccountID); err != nil {
				t.Fatal(err)
			}
			var rows []Transaction
			if err := database.Find(&rows).Error; err != nil || len(rows) != 1 || rows[0].ID != identityCurrent || rows[0].RawJSON != identityRaw {
				t.Fatalf("unexpected repaired rows: %+v, %v", rows, err)
			}
			if err := database.RepairTransactionIdentities(DefaultAccountID); err != nil {
				t.Fatalf("repeat migration: %v", err)
			}
		})
	}
}

func TestRepairTransactionIdentityPreservesExplicitExecutionsAndOtherAccounts(t *testing.T) {
	database := identityDB(t)
	other := identityRow("derived-unverifiable", "")
	other.AccountID = "savings"
	rows := []Transaction{identityRow("42", `{"id":42,"orderId":"shared"}`), identityRow("43", `{"transactionId":"43","orderId":"shared"}`), other}
	if err := database.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.RepairTransactionIdentities(DefaultAccountID); err != nil {
		t.Fatal(err)
	}
	var got []Transaction
	if err := database.Order("account_id,id").Find(&got).Error; err != nil || len(got) != 3 || got[0].ID != "42" || got[1].ID != "43" || got[2].ID != other.ID {
		t.Fatalf("execution/account scope changed: %+v, %v", got, err)
	}
}

func TestRepairTransactionIdentityMergesOnlyTheSameRawExecution(t *testing.T) {
	database := identityDB(t)
	legacy := identityRow("order-1", identityRaw)
	legacy.Currency = ""
	current := identityRow(identityCurrent, identityCanonical)
	current.Symbol = "RESOLVED"
	if err := database.Create(&[]Transaction{legacy, current}).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.RepairTransactionIdentities(DefaultAccountID); err != nil {
		t.Fatal(err)
	}
	var rows []Transaction
	if err := database.Find(&rows).Error; err != nil || len(rows) != 1 || rows[0].ID != identityCurrent || rows[0].Symbol != "RESOLVED" || rows[0].Currency != "EUR" {
		t.Fatalf("duplicate not safely reconciled: %+v, %v", rows, err)
	}
}

func TestRepairTransactionIdentityDoesNotDeleteDifferentFills(t *testing.T) {
	for _, conflict := range []Transaction{
		identityRow(identityCurrent, `{"orderId":"order-1","productId":1234,"quantity":1}`),
		func() Transaction {
			row := identityRow(identityCurrent, identityCanonical)
			row.Quantity = 1
			return row
		}(),
	} {
		t.Run(conflict.RawJSON, func(t *testing.T) {
			database := identityDB(t)
			if err := database.Create(&[]Transaction{identityRow("order-1", identityRaw), conflict}).Error; err != nil {
				t.Fatal(err)
			}
			if err := database.RepairTransactionIdentities(DefaultAccountID); !errors.Is(err, ErrTransactionIdentityReconciliation) {
				t.Fatalf("conflict error = %v", err)
			}
			count, err := database.CountTransactions()
			if err != nil || count != 2 {
				t.Fatalf("conflicting fill removed: %d, %v", count, err)
			}
		})
	}
}

func TestRepairTransactionIdentityFailureRollsBackAccount(t *testing.T) {
	database := identityDB(t)
	valid := identityRow("a-order", `{"orderId":"a-order","productId":1234,"quantity":2}`)
	invalid := identityRow("derived-unverifiable", "")
	if err := database.Create(&[]Transaction{valid, invalid}).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.RepairTransactionIdentities(DefaultAccountID); !errors.Is(err, ErrTransactionIdentityReconciliation) {
		t.Fatalf("missing evidence error = %v", err)
	}
	var rows []Transaction
	if err := database.Order("id").Find(&rows).Error; err != nil || len(rows) != 2 || rows[0].ID != "a-order" || rows[1].ID != invalid.ID {
		t.Fatalf("partial migration was published: %+v, %v", rows, err)
	}
}

func TestRepairTransactionIdentityRejectsUnprovenAliases(t *testing.T) {
	for _, row := range []Transaction{
		identityRow("unknown", identityRaw),
		identityRow("derived-unverifiable", `{}`),
		identityRow("different", `{"id":"actual"}`),
		identityRow("order", `{"id":true,"orderId":"order"}`),
	} {
		if _, _, err := repairedTransactionIdentity(row); !errors.Is(err, ErrTransactionIdentityReconciliation) {
			t.Fatalf("unproven ID %q accepted: %v", row.ID, err)
		}
	}
}

func TestRepairTransactionIdentityPreservesUnclassifiedManualRows(t *testing.T) {
	for _, raw := range []string{"", `{}`, "not-json"} {
		row := identityRow("manual", raw)
		got, _, err := repairedTransactionIdentity(row)
		if err != nil || got != row.ID {
			t.Fatalf("manual row changed: %q, %v", got, err)
		}
	}
}
