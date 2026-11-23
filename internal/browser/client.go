package browser

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/tatoalo/degitide/internal/market"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Client talks to the self-hosted browser worker over HTTP.
type Client struct {
	base    string
	token   string
	client  *http.Client
	account string
}

// ForAccount returns an isolated request scope while sharing the HTTP transport.
func (c *Client) ForAccount(account string) *Client {
	copy := *c
	copy.account = account
	return &copy
}

func NewClient(baseURL string, token ...string) *Client {
	workerToken := ""
	if len(token) > 0 {
		workerToken = token[0]
	}
	return &Client{
		base:  baseURL,
		token: workerToken,
		client: &http.Client{
			// Reauthentication can include two DEGIRO navigations plus a fresh
			// verification challenge. The public handler applies a tighter
			// per-request context while this remains the transport ceiling.
			Timeout: 220 * time.Second,
		},
	}
}

// --- Task responses ---

type SessionStatus struct {
	Valid bool `json:"valid"`
}

type Transaction struct {
	ID           string  `json:"id"`
	Type         string  `json:"type"`
	Date         string  `json:"date"`
	ProductID    string  `json:"product_id"`
	Symbol       string  `json:"symbol"`
	Quantity     float64 `json:"quantity"`
	Price        float64 `json:"price"`
	Currency     string  `json:"currency"`
	Fee          float64 `json:"fee"`
	Counterparty string  `json:"counterparty"`
	RawJSON      string  `json:"raw_json"`
}

type TransactionsResponse struct {
	Transactions []Transaction `json:"transactions"`
}

type Holding struct {
	ProductID       string   `json:"product_id"`
	Symbol          string   `json:"symbol"`
	Name            string   `json:"name"`
	ISIN            string   `json:"isin"`
	ExchangeMIC     string   `json:"exchange_mic"`
	ExchangeName    string   `json:"exchange_name"`
	Quantity        float64  `json:"quantity"`
	AvgPrice        float64  `json:"avg_price"`
	MarketValue     float64  `json:"market_value"`
	Currency        string   `json:"currency"`
	ValueCurrency   string   `json:"value_currency"`
	Price           *float64 `json:"price"`
	PriceAsOf       string   `json:"price_as_of"`
	ValuationSource string   `json:"valuation_source"`
}

type HoldingsResponse struct {
	Holdings       []Holding        `json:"holdings"`
	Valuation      *Valuation       `json:"valuation"`
	MarketSchedule *market.Schedule `json:"market_schedule"`
}

// Valuation contains DEGIRO account figures observed with one holdings pull.
// Missing numeric fields remain nil rather than being mistaken for zero.
type Valuation struct {
	BaseCurrency             string   `json:"base_currency"`
	PortfolioValue           *float64 `json:"portfolio_value"`
	CashBalance              *float64 `json:"cash_balance"`
	AccountValue             *float64 `json:"account_value"`
	TotalPL                  *float64 `json:"total_pl"`
	DayPL                    *float64 `json:"day_pl"`
	AvailableToTrade         *float64 `json:"available_to_trade"`
	FetchedAt                string   `json:"fetched_at"`
	Source                   string   `json:"source"`
	PriceStatus              string   `json:"price_status"`
	ReconciliationDifference *float64 `json:"reconciliation_difference"`
	Warning                  string   `json:"warning"`
}

// Position is one security in a historical DEGIRO position report.
type Position struct {
	Identifier string   `json:"identifier"`
	Name       string   `json:"name"`
	Quantity   float64  `json:"quantity"`
	Close      float64  `json:"close"`
	Currency   string   `json:"currency"`
	Value      float64  `json:"value"`
	BaseValue  *float64 `json:"base_value"`
}

// PositionSnapshot is the securities held at the end of one past day.
type PositionSnapshot struct {
	Date         string     `json:"date"`
	BaseCurrency string     `json:"base_currency"`
	Positions    []Position `json:"positions"`
}

// PositionsResponse lists fetched reports plus dates DEGIRO could not report.
type PositionsResponse struct {
	Snapshots   []PositionSnapshot `json:"snapshots"`
	Unavailable []string           `json:"unavailable"`
}

type ReauthenticationResponse struct {
	Authenticated bool `json:"authenticated"`
	SessionSaved  bool `json:"session_saved"`
}

// --- Methods ---

// WorkerInfo describes the installed worker and its runtime.
type WorkerInfo struct {
	Version    string `json:"version"`
	Python     string `json:"python"`
	Patchright string `json:"patchright"`
}

// Info reads process metadata without starting a browser or checking DEGIRO.
func (c *Client) Info(ctx context.Context) (*WorkerInfo, error) {
	var out WorkerInfo
	err := c.do(ctx, "GET", "/info", nil, &out)
	return &out, err
}

// CheckSession returns whether the browser worker's DeGiro session is still valid.
func (c *Client) CheckSession(ctx context.Context) (*SessionStatus, error) {
	var out SessionStatus
	err := c.do(ctx, "GET", "/tasks/check-session", nil, &out)
	return &out, err
}

// FetchTransactions retrieves transaction history for the given date range.
func (c *Client) FetchTransactions(ctx context.Context, from, to string) (*TransactionsResponse, error) {
	var out TransactionsResponse
	err := c.do(ctx, "POST", "/tasks/fetch-transactions", map[string]string{
		"from": from,
		"to":   to,
	}, &out)
	return &out, err
}

// FetchPositions retrieves DEGIRO position reports for past ISO dates.
func (c *Client) FetchPositions(ctx context.Context, dates []string) (*PositionsResponse, error) {
	var out PositionsResponse
	err := c.do(ctx, "POST", "/tasks/fetch-positions", map[string][]string{"dates": dates}, &out)
	return &out, err
}

// FetchHoldings retrieves current holdings snapshot.
func (c *Client) FetchHoldings(ctx context.Context) (*HoldingsResponse, error) {
	var out HoldingsResponse
	err := c.do(ctx, "POST", "/tasks/fetch-holdings", map[string]any{}, &out)
	return &out, err
}

// Reauthenticate submits a DEGIRO login attempt to the isolated worker.
// A supplied authenticator seed enables encrypted credential persistence.
func (c *Client) Reauthenticate(ctx context.Context, username, password, totp string, seeds ...string) (*ReauthenticationResponse, error) {
	seed := ""
	if len(seeds) > 0 {
		seed = seeds[0]
	}
	var out ReauthenticationResponse
	err := c.do(ctx, "POST", "/tasks/reauthenticate", map[string]string{
		"username": username,
		"password": password,
		"totp":     totp,
		"seed":     seed,
	}, &out)
	return &out, err
}

// --- Internal ---

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal body: %w", err)
		}
		reqBody = bytes.NewReader(b)
	}

	endpoint := c.base + path
	if c.account != "" {
		endpoint += "?account=" + url.QueryEscape(c.account)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reqBody)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("worker request %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("worker %s returned %d: %s", path, resp.StatusCode, string(b))
	}

	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}
