package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tatoalo/degitide/internal/browser"
	"github.com/tatoalo/degitide/internal/db"
	"github.com/tatoalo/degitide/internal/simplefin"
)

func sureFixture(t *testing.T) (*db.DB, http.Handler) {
	t.Helper()
	database, err := db.New(filepath.Join(t.TempDir(), "sure.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	if _, err := database.CreateAdmin("admin", "unused"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	day := now.Truncate(24 * time.Hour)
	if err := database.Model(&db.Account{}).Where("id = ?", "default").Updates(map[string]any{"last_pull": now.Format(time.RFC3339), "snapshot_date": day}).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.Create(&db.Account{ID: "private", Name: "Private account", LastPull: now.Format(time.RFC3339), SnapshotDate: &day}).Error; err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"default", "private"} {
		if err := database.ReplaceHoldings(day, []db.Holding{{ProductID: "p1", Symbol: "Fund name, not a ticker", Quantity: 10, AvgPrice: 8, MarketValue: 125.5, Currency: "EUR"}, {ProductID: "p2", Symbol: "AAPL", Quantity: 2, AvgPrice: 100, MarketValue: 400, Currency: "USD"}}, id); err != nil {
			t.Fatal(err)
		}
	}
	trades := []db.Transaction{}
	for i := 0; i < 125; i++ {
		trades = append(trades, db.Transaction{ID: time.Unix(int64(i), 0).Format(time.RFC3339), AccountID: "default", Date: day, Type: "BUY", ProductID: "p1", Symbol: "Fund", Quantity: 2, Price: 10, Currency: "EUR", Fee: 3, RawJSON: `{"secret":"must not leave"}`})
	}
	if err := database.UpsertTransactions(trades); err != nil {
		t.Fatal(err)
	}
	return database, New(database, browser.NewClient("http://127.0.0.1:1"), &fakeScheduler{}, Options{APIToken: "admin-secret", AllowedHosts: "localhost,127.0.0.1,degitide-sure"}).Router()
}
func sureRequest(handler http.Handler, method, path string, body any, user, password string) *httptest.ResponseRecorder {
	var input io.Reader
	if body != nil {
		encoded, _ := json.Marshal(body)
		input = bytes.NewReader(encoded)
	}
	req := httptest.NewRequest(method, "http://localhost"+path, input)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "http://localhost")
	}
	if user != "" {
		req.SetBasicAuth(user, password)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	return response
}
func sureGrant(t *testing.T, handler http.Handler, activity bool) (string, *url.URL) {
	t.Helper()
	response := sureRequest(handler, "POST", "/api/integrations/sure", map[string]any{"base_url": "http://localhost", "account_ids": []string{"default"}, "include_transactions": activity}, "degitide", "admin-secret")
	if response.Code != 201 {
		t.Fatalf("create %d: %s", response.Code, response.Body.String())
	}
	var result struct {
		ID    string `json:"id"`
		Token string `json:"setup_token"`
	}
	json.Unmarshal(response.Body.Bytes(), &result)
	claim, _ := base64.StdEncoding.DecodeString(result.Token)
	u, _ := url.Parse(string(claim))
	return result.ID, u
}
func sureClaim(t *testing.T, handler http.Handler, claim *url.URL) *url.URL {
	t.Helper()
	response := sureRequest(handler, "POST", claim.Path, nil, "", "")
	if response.Code != 200 {
		t.Fatalf("claim %d: %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("claim was cacheable")
	}
	access, err := url.Parse(response.Body.String())
	if err != nil {
		t.Fatal(err)
	}
	return access
}
func sureRead(handler http.Handler, access *url.URL, query string) *httptest.ResponseRecorder {
	password, _ := access.User.Password()
	return sureRequest(handler, "GET", access.Path+"/accounts"+query, nil, access.User.Username(), password)
}

func TestSureConnectionLifecycleAndIsolation(t *testing.T) {
	database, handler := sureFixture(t)
	id, claim := sureGrant(t, handler, false)
	access := sureClaim(t, handler, claim)
	if r := sureRequest(handler, "POST", claim.Path, nil, "", ""); r.Code != 403 {
		t.Fatal("setup token reused")
	}
	password, _ := access.User.Password()
	// Read capabilities cannot access administrator data, the worker or mutations.
	for _, path := range []string{"/api/transactions", "/api/accounts", "/api/integrations/sure"} {
		if r := sureRequest(handler, "GET", path, nil, access.User.Username(), password); r.Code != 401 {
			t.Fatalf("read token reached %s: %d", path, r.Code)
		}
	}
	if r := sureRequest(handler, "POST", "/api/pull/trigger", map[string]any{}, access.User.Username(), password); r.Code != 401 {
		t.Fatal("read token triggered a pull")
	}
	if r := sureRequest(handler, "GET", "/simplefin/access/accounts", nil, "degitide", "admin-secret"); r.Code != 403 {
		t.Fatal("dashboard token bypassed feed scope")
	}
	first := sureRead(handler, access, "?version=2")
	if first.Code != 200 {
		t.Fatalf("read %d %s", first.Code, first.Body.String())
	}
	var feed simplefin.Feed
	json.Unmarshal(first.Body.Bytes(), &feed)
	if len(feed.Accounts) != 2 {
		t.Fatalf("scope/currency isolation: %+v", feed)
	}
	for _, a := range feed.Accounts {
		marker, ok := a.Extra["degitide"].(map[string]any)
		if !ok || marker["version"] != float64(1) || marker["complete_holdings"] != true || marker["snapshot_at"] == "" {
			t.Fatal("missing versioned complete-snapshot contract for Sure")
		}
		if strings.Contains(a.ID, "private") || len(a.Transactions) != 0 {
			t.Fatal("scope leak")
		}
		if a.Currency == "EUR" && a.Balance != "125.50000000" {
			t.Fatalf("balance %s", a.Balance)
		}
		if !strings.HasPrefix(a.Holdings[0].Symbol, "CUSTOM:DEGIRO_") {
			t.Fatal("unverified exchange ticker emitted")
		}
	}
	second := sureRead(handler, access, "?version=2")
	if first.Body.String() != second.Body.String() {
		t.Fatal("repeat reads changed record identity")
	}
	status := sureRequest(handler, "GET", "/api/integrations/sure", nil, "degitide", "admin-secret")
	if strings.Contains(status.Body.String(), password) || strings.Contains(status.Body.String(), "claim_hash") || strings.Contains(status.Body.String(), "access_hash") {
		t.Fatal("secret leaked in status")
	}
	// A new handler simulates an app restart. Access lives in SQLite.
	restarted := New(database, browser.NewClient("http://127.0.0.1:1"), &fakeScheduler{}, Options{APIToken: "admin-secret", AllowedHosts: "localhost"}).Router()
	if r := sureRead(restarted, access, ""); r.Code != 200 {
		t.Fatal("access lost on restart")
	}
	if r := sureRequest(handler, "DELETE", "/api/integrations/sure/"+id, map[string]any{}, "degitide", "admin-secret"); r.Code != 200 {
		t.Fatal("revoke failed")
	}
	if r := sureRead(restarted, access, ""); r.Code != 403 {
		t.Fatal("revoked connection can read")
	}
}

func TestSureActivityIsOptInBoundedAndDeduplicable(t *testing.T) {
	_, handler := sureFixture(t)
	_, claim := sureGrant(t, handler, true)
	access := sureClaim(t, handler, claim)
	response := sureRead(handler, access, "?start-date=0&version=2")
	var feed simplefin.Feed
	json.Unmarshal(response.Body.Bytes(), &feed)
	rows := feed.Accounts[0].Transactions
	if len(rows) != 125 {
		t.Fatalf("activity truncated: %d", len(rows))
	}
	seen := map[string]bool{}
	for _, row := range rows {
		if seen[row.ID] || row.Amount != "-20.00000000" {
			t.Fatal("duplicate identity or wrong gross amount")
		}
		seen[row.ID] = true
	}
	if strings.Contains(response.Body.String(), "must not leave") {
		t.Fatal("raw broker data leaked")
	}
	for _, query := range []string{"?balances-only=1", "?start-date=0&end-date=1"} {
		json.Unmarshal(sureRead(handler, access, query).Body.Bytes(), &feed)
		for _, a := range feed.Accounts {
			if len(a.Transactions) != 0 {
				t.Fatal("transaction filter ignored")
			}
		}
	}
	json.Unmarshal(sureRead(handler, access, "?account=degitide-default-USD").Body.Bytes(), &feed)
	if len(feed.Accounts) != 1 || feed.Accounts[0].Currency != "USD" {
		t.Fatal("account filter ignored")
	}
	for _, query := range []string{"?start-date=oops", "?end-date=-1", "?start-date=20&end-date=10", "?version=3"} {
		if r := sureRead(handler, access, query); r.Code != 400 {
			t.Fatalf("accepted %s", query)
		}
	}
}

func TestSureClaimExpiresIsAtomicAndCannotBeClaimedCrossSite(t *testing.T) {
	database, handler := sureFixture(t)
	id, claim := sureGrant(t, handler, false)
	database.Model(&db.SimpleFINConnection{}).Where("id = ?", id).Update("expires_at", time.Now().Add(-time.Hour))
	if r := sureRequest(handler, "POST", claim.Path, nil, "", ""); r.Code != 403 {
		t.Fatal("expired claim accepted")
	}
	_, claim = sureGrant(t, handler, false)
	req := httptest.NewRequest("POST", "http://localhost"+claim.Path, nil)
	req.Header.Set("Origin", "https://evil.example")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != 403 {
		t.Fatal("cross-site claim accepted")
	}
	var wg sync.WaitGroup
	codes := make(chan int, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); codes <- sureRequest(handler, "POST", claim.Path, nil, "", "").Code }()
	}
	wg.Wait()
	close(codes)
	successes := 0
	for code := range codes {
		if code == 200 {
			successes++
		} else if code != 403 {
			t.Fatalf("unexpected claim response: %d", code)
		}
	}
	if successes != 1 {
		t.Fatalf("one-time token claimed %d times", successes)
	}
}
func TestSureRejectsUnsafeTargetsAndScope(t *testing.T) {
	_, handler := sureFixture(t)
	for _, base := range []string{"http://public.example", "https://unknown.example", "http://user:secret@localhost", "http://localhost/a", "http://localhost?x=y", "file:///tmp"} {
		r := sureRequest(handler, "POST", "/api/integrations/sure", map[string]any{"base_url": base}, "degitide", "admin-secret")
		if r.Code != 400 {
			t.Fatalf("accepted %s", base)
		}
	}
	for _, ids := range [][]string{{"missing"}, {"default", "default"}} {
		r := sureRequest(handler, "POST", "/api/integrations/sure", map[string]any{"base_url": "http://localhost", "account_ids": ids}, "degitide", "admin-secret")
		if r.Code != 400 {
			t.Fatal("invalid account scope accepted")
		}
	}
	if r := sureRequest(handler, "POST", "/api/integrations/sure", map[string]any{"base_url": "http://localhost"}, "", ""); r.Code != 401 {
		t.Fatal("anonymous setup accepted")
	}
}

func TestSureWithholdsStaleFailedAndUnsupportedHoldings(t *testing.T) {
	database, handler := sureFixture(t)
	_, claim := sureGrant(t, handler, false)
	access := sureClaim(t, handler, claim)
	now := time.Now().UTC()
	cases := []map[string]any{{"last_pull": now.Add(-48 * time.Hour).Format(time.RFC3339)}, {"last_pull": now.Format(time.RFC3339), "last_error": "secret broker response"}}
	for _, update := range cases {
		database.Model(&db.Account{}).Where("id = ?", "default").Updates(update)
		var feed simplefin.Feed
		response := sureRead(handler, access, "")
		json.Unmarshal(response.Body.Bytes(), &feed)
		if len(feed.Accounts) != 0 || len(feed.Errors) != 1 || strings.Contains(response.Body.String(), "secret broker") {
			t.Fatal("stale/failed data exposed")
		}
	}
	database.Model(&db.Account{}).Where("id = ?", "default").Updates(map[string]any{"last_error": ""})
	database.Model(&db.Holding{}).Where("account_id = ?", "default").Update("quantity", -1)
	var feed simplefin.Feed
	json.Unmarshal(sureRead(handler, access, "").Body.Bytes(), &feed)
	if len(feed.Accounts) != 0 || len(feed.Errors) != 1 {
		t.Fatal("short holdings were misrepresented")
	}
}

func TestSureReportsZeroAfterSameDayFullSale(t *testing.T) {
	database, handler := sureFixture(t)
	_, claim := sureGrant(t, handler, false)
	access := sureClaim(t, handler, claim)
	var source db.Account
	database.First(&source, "id = ?", "default")
	if err := database.ReplaceHoldings(*source.SnapshotDate, nil, "default"); err != nil {
		t.Fatal(err)
	}
	var feed simplefin.Feed
	json.Unmarshal(sureRead(handler, access, "").Body.Bytes(), &feed)
	if len(feed.Accounts) != 2 {
		t.Fatal("empty currency accounts disappeared")
	}
	for _, account := range feed.Accounts {
		if account.Balance != "0.00000000" || len(account.Holdings) != 0 {
			t.Fatal("sold positions still in feed")
		}
	}
}

func TestSureActivityKeepsCurrenciesWithoutCurrentHoldingsAndDecimalAmounts(t *testing.T) {
	database, handler := sureFixture(t)
	if err := database.UpsertTransactions([]db.Transaction{{AccountID: "default", ID: "gbp-trade", Date: time.Now().UTC().Truncate(24 * time.Hour), Type: "SELL", Currency: "GBP", Symbol: "Example", ProductID: "old", Quantity: 3, Price: 0.1}}); err != nil {
		t.Fatal(err)
	}
	_, claim := sureGrant(t, handler, true)
	access := sureClaim(t, handler, claim)
	var feed simplefin.Feed
	json.Unmarshal(sureRead(handler, access, "?account=degitide-default-GBP&start-date=0").Body.Bytes(), &feed)
	if len(feed.Accounts) != 1 || len(feed.Accounts[0].Transactions) != 1 {
		t.Fatal("trade-only currency disappeared")
	}
	if feed.Accounts[0].Balance != "0.00000000" || feed.Accounts[0].Transactions[0].Amount != "0.30000000" {
		t.Fatal("wrong balance or decimal consideration")
	}
}
