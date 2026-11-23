// Package simplefin exposes saved portfolios through Sure's existing read-only
// provider. It never calls the browser worker or writes to the broker.
package simplefin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tatoalo/degitide/internal/db"
	"gorm.io/gorm"
)

type Feed struct {
	Errors      []string     `json:"errors"`
	ErrorList   []FeedError  `json:"errlist"`
	Connections []Connection `json:"connections"`
	Accounts    []Account    `json:"accounts"`
}
type FeedError struct {
	Code         string `json:"code"`
	Message      string `json:"msg"`
	ConnectionID string `json:"conn_id"`
}
type Connection struct {
	ID    string `json:"conn_id"`
	Name  string `json:"name"`
	OrgID string `json:"org_id"`
	URL   string `json:"sfin_url"`
}
type Account struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	ConnectionID string            `json:"conn_id"`
	Org          map[string]string `json:"org"`
	Type         string            `json:"type"`
	Subtype      string            `json:"subtype"`
	Currency     string            `json:"currency"`
	Balance      string            `json:"balance"`
	BalanceDate  int64             `json:"balance-date"`
	Holdings     []Holding         `json:"holdings"`
	Transactions []Transaction     `json:"transactions"`
	Extra        map[string]any    `json:"extra"`
}
type Holding struct {
	ID          string `json:"id"`
	Symbol      string `json:"symbol"`
	Description string `json:"description"`
	Shares      string `json:"shares"`
	MarketValue string `json:"market_value"`
	Currency    string `json:"currency"`
	CostBasis   string `json:"cost_basis,omitempty"`
}

// BalanceHistoryPoint is a saved securities valuation, not a cash transaction.
// The scoped adapter uses it to populate Sure's native historical valuations.
type BalanceHistoryPoint struct {
	Date    string `json:"date"`
	Balance string `json:"balance"`
	Source  string `json:"source"`
}
type Transaction struct {
	ID          string            `json:"id"`
	Posted      int64             `json:"posted"`
	Amount      string            `json:"amount"`
	Description string            `json:"description"`
	Currency    string            `json:"currency"`
	Memo        string            `json:"memo"`
	Extra       map[string]string `json:"extra"`
}

type Window struct {
	Start, End   int64
	Accounts     map[string]bool
	BalancesOnly bool
}

func ParseWindow(q url.Values, now time.Time) (Window, error) {
	w := Window{Start: now.AddDate(0, 0, -90).Unix(), End: now.Unix(), Accounts: map[string]bool{}, BalancesOnly: q.Get("balances-only") == "1"}
	for key, target := range map[string]*int64{"start-date": &w.Start, "end-date": &w.End} {
		if value := q.Get(key); value != "" {
			parsed, err := strconv.ParseInt(value, 10, 64)
			if err != nil || parsed < 0 {
				return w, fmt.Errorf("invalid %s", key)
			}
			*target = parsed
		}
	}
	if w.Start > w.End {
		return w, fmt.Errorf("start-date must precede end-date")
	}
	if v := q.Get("version"); v != "" && v != "1" && v != "2" {
		return w, fmt.Errorf("unsupported version")
	}
	for _, id := range q["account"] {
		w.Accounts[id] = true
	}
	return w, nil
}

var currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)

func decimal(n float64) string { return strconv.FormatFloat(n, 'f', -1, 64) }
func finite(n float64) bool    { return !math.IsNaN(n) && !math.IsInf(n, 0) }
func stableID(parts ...string) string {
	s := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(s[:16])
}

func balanceHistory(points []db.ValuePoint, currency string, snapshotDate time.Time) ([]BalanceHistoryPoint, error) {
	history := []BalanceHistoryPoint{}
	latest := snapshotDate.Format("2006-01-02")
	previous := ""
	for _, point := range points {
		if point.Currency != currency {
			continue
		}
		day, err := time.Parse("2006-01-02", point.Date)
		if err != nil || day.Year() < 1900 || point.Date <= previous || point.Date > latest ||
			!finite(point.Value) || point.Value < 0 || (point.Source != "report" && point.Source != "snapshot") {
			return nil, fmt.Errorf("invalid saved %s valuation history", currency)
		}
		history = append(history, BalanceHistoryPoint{Date: point.Date, Balance: decimal(point.Value), Source: point.Source})
		previous = point.Date
	}
	return history, nil
}

// Build takes a consistent read transaction. Each currency is a separate
// securities-only account. Broker cash and account totals are deliberately
// separate from this connector’s holdings-only balances.
func Build(database *db.DB, grant db.SimpleFINConnection, window Window, now time.Time) (Feed, error) {
	feed := Feed{Errors: []string{}, ErrorList: []FeedError{}, Connections: []Connection{}, Accounts: []Account{}}
	var ids []string
	if err := json.Unmarshal([]byte(grant.AccountIDs), &ids); err != nil {
		return feed, err
	}
	err := database.DB.Transaction(func(tx *gorm.DB) error {
		for _, id := range ids {
			var source db.Account
			if err := tx.First(&source, "id = ?", id).Error; err != nil {
				return err
			}
			connID := "degitide-" + id
			feed.Connections = append(feed.Connections, Connection{ID: connID, Name: source.Name, OrgID: "degitide", URL: grant.BaseURL + "/simplefin"})
			fail := func(message string) {
				feed.Errors = append(feed.Errors, source.Name+": "+message)
				feed.ErrorList = append(feed.ErrorList, FeedError{Code: "con.", Message: message, ConnectionID: connID})
			}
			pulled, err := time.Parse(time.RFC3339, source.LastPull)
			if err != nil || source.SnapshotDate == nil || source.LastError != "" || now.Sub(pulled) > 36*time.Hour || pulled.After(now.Add(time.Minute)) {
				fail("A fresh successful Degitide sync is required. Check the account in Degitide.")
				continue
			}
			var holdings []db.Holding
			if err := tx.Where("account_id = ? AND snapshot_date = ?", id, *source.SnapshotDate).Order("product_id").Find(&holdings).Error; err != nil {
				return err
			}
			// Keep currency accounts discoverable after their last holding is sold.
			var currencies []string
			if err := tx.Model(&db.PortfolioCurrency{}).Where("account_id = ?", id).Order("currency").Pluck("currency", &currencies).Error; err != nil {
				return err
			}
			if grant.IncludeTransactions {
				var tradeCurrencies []string
				if err := tx.Model(&db.Transaction{}).Where("account_id = ?", id).Distinct("currency").Pluck("currency", &tradeCurrencies).Error; err != nil {
					return err
				}
				seen := map[string]bool{}
				for _, currency := range currencies {
					seen[currency] = true
				}
				for _, currency := range tradeCurrencies {
					if !seen[currency] {
						currencies = append(currencies, currency)
						seen[currency] = true
					}
				}
			}
			var trades []db.Transaction
			if grant.IncludeTransactions && !window.BalancesOnly {
				if err := tx.Omit("raw_json").Where("account_id = ? AND date >= ? AND date <= ?", id, time.Unix(window.Start, 0).UTC(), time.Unix(window.End, 0).UTC()).Order("date, id").Find(&trades).Error; err != nil {
					return err
				}
			}
			invalid := false
			for _, h := range holdings {
				if h.ProductID == "" || !currencyPattern.MatchString(h.Currency) || !currencyPattern.MatchString(h.MarketCurrency()) || !finite(h.Quantity) || !finite(h.MarketValue) || !finite(h.AvgPrice) || h.Quantity < 0 || h.MarketValue < 0 {
					invalid = true
				}
			}
			if invalid {
				fail("Holdings contain unknown currencies, invalid values or short positions that this connector cannot represent.")
				continue
			}
			// Historical balances are separate from the opt-in activity ledger and
			// its date window. An initial import needs all known valuation dates.
			historyPoints, err := (&db.DB{DB: tx}).ValueHistory(id)
			if err != nil {
				return err
			}
			for _, currency := range currencies {
				if !currencyPattern.MatchString(currency) {
					continue
				}
				accountID := "degitide-" + id + "-" + currency
				if len(window.Accounts) > 0 && !window.Accounts[accountID] {
					continue
				}
				history, err := balanceHistory(historyPoints, currency, *source.SnapshotDate)
				if err != nil {
					fail("Saved " + currency + " valuation history is inconsistent. Check the account in Degitide.")
					continue
				}
				account := Account{ID: accountID, Name: source.Name + " · " + currency + " securities", ConnectionID: connID, Org: map[string]string{"id": "degitide", "name": "Degitide", "domain": "degiro.com"}, Type: "investment", Subtype: "brokerage", Currency: currency, BalanceDate: pulled.Unix(), Holdings: []Holding{}, Transactions: []Transaction{}, Extra: map[string]any{"scope": "securities_only", "cash_included": false, "native_trades": false, "degitide": map[string]any{"version": 1, "complete_holdings": true, "snapshot_at": pulled.Format(time.RFC3339), "history_version": 1, "history_scope": "securities_only", "balance_history": history}}}
				total := new(big.Rat)
				for _, h := range holdings {
					if h.MarketCurrency() != currency || h.Quantity == 0 {
						continue
					}
					n, _ := new(big.Rat).SetString(decimal(h.MarketValue))
					total.Add(total, n)
					// Product labels are not verified exchange tickers. CUSTOM keeps Sure
					// from resolving the name to an unrelated quoted instrument.
					holding := Holding{ID: "degitide-" + stableID(id, h.ProductID), Symbol: "CUSTOM:DEGIRO_" + strings.ToUpper(stableID(h.ProductID)), Description: h.Symbol, Shares: decimal(h.Quantity), MarketValue: decimal(h.MarketValue), Currency: currency}
					if holding.Description == "" {
						holding.Description = "DEGIRO product " + h.ProductID
					}
					if h.AvgPrice > 0 && h.Currency == currency {
						holding.CostBasis = decimal(h.AvgPrice)
					}
					account.Holdings = append(account.Holdings, holding)
				}
				account.Balance = total.FloatString(8)
				for _, t := range trades {
					if t.Currency != currency {
						continue
					}
					if (t.Type != "BUY" && t.Type != "SELL") || t.ID == "" || !finite(t.Quantity) || !finite(t.Price) || !finite(t.Fee) || t.Quantity == 0 || t.Price < 0 || !finite(t.Quantity*t.Price) {
						fail("Some transaction activity could not be represented and was omitted.")
						continue
					}
					quantity, _ := new(big.Rat).SetString(decimal(math.Abs(t.Quantity)))
					price, _ := new(big.Rat).SetString(decimal(t.Price))
					amount := new(big.Rat).Mul(quantity, price)
					if t.Type == "BUY" {
						amount.Neg(amount)
					}
					account.Transactions = append(account.Transactions, Transaction{ID: "degitide-" + stableID(id, t.ID), Posted: t.Date.Unix(), Amount: amount.FloatString(8), Currency: currency, Description: t.Type + " " + decimal(math.Abs(t.Quantity)) + " " + t.Symbol, Memo: "DEGIRO activity; gross consideration only. Fees excluded because their currency is not recorded. Not a native Sure trade.", Extra: map[string]string{"product_id": t.ProductID, "quantity": decimal(t.Quantity), "price": decimal(t.Price), "fee_reported": decimal(t.Fee), "fee_currency": "unknown", "type": t.Type}})
				}
				feed.Accounts = append(feed.Accounts, account)
			}
		}
		return nil
	})
	sort.Slice(feed.Accounts, func(i, j int) bool { return feed.Accounts[i].ID < feed.Accounts[j].ID })
	return feed, err
}
