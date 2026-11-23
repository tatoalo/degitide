package server

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tatoalo/degitide/internal/browser"
	"github.com/tatoalo/degitide/internal/db"
)

func TestExportsIncludeAllSavedRowsAndIsolateAccounts(t *testing.T) {
	database, err := db.New(filepath.Join(t.TempDir(), "exports.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Create(&db.Account{ID: "savings", Name: "Savings, €"}).Error; err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	rows := []db.Transaction{}
	for i := 0; i < 125; i++ {
		rows = append(rows, db.Transaction{AccountID: "savings", ID: fmt.Sprintf("trade-%03d", i), Date: day, Type: "SELL", Symbol: "=HYPERLINK(\"unsafe\")", Quantity: -1.25, Price: 123.456, Currency: "EUR", Fee: .5, RawJSON: `{"private":"source data"}`})
	}
	rows = append(rows, db.Transaction{AccountID: "default", ID: "private-other-account", Date: day, Symbol: "OTHER"})
	if err := database.UpsertTransactions(rows); err != nil {
		t.Fatal(err)
	}
	app := httptest.NewServer(New(database, browser.NewClient("http://127.0.0.1:1"), &fakeScheduler{}).Router())
	defer app.Close()
	for _, format := range []string{"csv", "json", "pdf"} {
		t.Run(format, func(t *testing.T) {
			response, err := http.Get(app.URL + "/api/transactions/export?account=savings&format=" + format)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, _ := io.ReadAll(response.Body)
			if response.StatusCode != 200 {
				t.Fatalf("status %d: %s", response.StatusCode, body)
			}
			if !strings.Contains(response.Header.Get("Content-Disposition"), "degitide-savings-transactions-") || response.Header.Get("Cache-Control") != "no-store" {
				t.Fatal("missing safe download headers")
			}
			if strings.Contains(string(body), "private-other-account") || strings.Contains(string(body), "source data") {
				t.Fatal("export leaked unrelated or raw records")
			}
			switch format {
			case "csv":
				records, err := csv.NewReader(strings.NewReader(string(body))).ReadAll()
				if err != nil || len(records) != 126 {
					t.Fatalf("truncated CSV: %d rows, %v", len(records), err)
				}
				if records[1][6] != "'=HYPERLINK(\"unsafe\")" || records[1][7] != "-1.25" || records[1][8] != "123.456" {
					t.Fatalf("unsafe/lossy CSV: %v", records[1])
				}
			case "json":
				var records []exportedTransaction
				if err := json.Unmarshal(body, &records); err != nil || len(records) != 125 {
					t.Fatalf("truncated JSON: %d rows, %v", len(records), err)
				}
				if records[0].AccountName != "Savings, €" || records[0].Instrument != "=HYPERLINK(\"unsafe\")" {
					t.Fatal("JSON values changed")
				}
			case "pdf":
				if !strings.HasPrefix(string(body), "%PDF-") || !strings.Contains(string(body), "%%EOF") || strings.Count(string(body), "/Type /Page\n") < 2 {
					t.Fatal("invalid or unpaginated PDF")
				}
			}
		})
	}
	for _, query := range []string{"account=missing&format=csv", "account=savings&format=exe"} {
		response, err := http.Get(app.URL + "/api/transactions/export?" + query)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode < 400 {
			t.Fatal("invalid export accepted")
		}
	}
}

func TestEmptyAndUnicodeExports(t *testing.T) {
	csvData, err := transactionsCSV(nil)
	if err != nil || strings.Count(string(csvData), "\n") != 1 {
		t.Fatal("empty CSV must retain header")
	}
	pdfData, err := transactionsPDF(db.Account{ID: "empty", Name: "Épargne €"}, nil, time.Now())
	if err != nil || !strings.HasPrefix(string(pdfData), "%PDF-") {
		t.Fatalf("empty/unicode PDF failed: %v", err)
	}
}

func TestExportsRequireDashboardAuthentication(t *testing.T) {
	database, err := db.New(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	handler := New(database, browser.NewClient("http://127.0.0.1:1"), &fakeScheduler{}, Options{APIToken: "test-token"}).Router()
	request := httptest.NewRequest("GET", "http://localhost/api/transactions/export?format=csv", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code == 200 {
		t.Fatal("unauthenticated financial export allowed")
	}
}
