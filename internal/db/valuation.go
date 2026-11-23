package db

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"time"

	"gorm.io/gorm"
)

// BrokerValuation is the latest set of account figures observed from DEGIRO.
// It is saved inside the same transaction as holdings and the successful pull.
// FetchedAt describes collection time; it is never presented as a quote time.
type BrokerValuation struct {
	AccountID                string    `gorm:"primaryKey" json:"account_id"`
	BaseCurrency             string    `json:"base_currency"`
	PortfolioValue           *float64  `json:"portfolio_value"`
	CashBalance              *float64  `json:"cash_balance"`
	AccountValue             *float64  `json:"account_value"`
	TotalPL                  *float64  `json:"total_pl"`
	DayPL                    *float64  `json:"day_pl"`
	AvailableToTrade         *float64  `json:"available_to_trade"`
	FetchedAt                time.Time `json:"fetched_at"`
	Source                   string    `json:"source"`
	PriceStatus              string    `json:"price_status"`
	ReconciliationDifference *float64  `json:"reconciliation_difference"`
	Warning                  string    `json:"warning"`
}

var valuationCurrency = regexp.MustCompile(`^[A-Z]{3}$`)

// MarketCurrency identifies the units of MarketValue, independently of the
// instrument's native price and cost currency. The fallback preserves old rows.
func (h Holding) MarketCurrency() string {
	if h.ValueCurrency != "" {
		return h.ValueCurrency
	}
	return h.Currency
}

func (v *BrokerValuation) Validate() error {
	if v == nil {
		return nil
	}
	if !valuationCurrency.MatchString(v.BaseCurrency) {
		return errors.New("broker valuation has no valid base currency")
	}
	if v.FetchedAt.IsZero() {
		return errors.New("broker valuation has no collection timestamp")
	}
	if v.Source != "broker_frontend" && v.Source != "broker_api" {
		return errors.New("broker valuation has an unknown source")
	}
	if v.PriceStatus != "quoted" && v.PriceStatus != "closing" && v.PriceStatus != "unavailable" {
		return errors.New("broker valuation has an unknown price status")
	}
	for name, value := range map[string]*float64{
		"portfolio value": v.PortfolioValue, "cash balance": v.CashBalance,
		"account value": v.AccountValue, "total P/L": v.TotalPL, "day P/L": v.DayPL,
		"available to trade": v.AvailableToTrade, "reconciliation difference": v.ReconciliationDifference,
	} {
		if value != nil && (math.IsNaN(*value) || math.IsInf(*value, 0)) {
			return fmt.Errorf("broker valuation has a non-finite %s", name)
		}
	}
	if v.AccountValue != nil && v.PortfolioValue != nil && v.CashBalance != nil &&
		math.Abs(*v.AccountValue-*v.PortfolioValue-*v.CashBalance) > 0.020000001 {
		return errors.New("broker account value does not reconcile with portfolio and cash")
	}
	return nil
}

// SaveBrokerValuation replaces all fields, including explicit nil values. A
// missing valuation clears the old one so it cannot accompany newer holdings.
func (d *DB) SaveBrokerValuation(accountID string, valuation *BrokerValuation) error {
	if valuation == nil {
		return d.Where("account_id = ?", accountID).Delete(&BrokerValuation{}).Error
	}
	if err := valuation.Validate(); err != nil {
		return err
	}
	copy := *valuation
	copy.AccountID = accountID
	return d.Transaction(func(tx *gorm.DB) error {
		if copy.DayPL != nil {
			observation := DayPLObservation{AccountID: accountID, FetchedAt: copy.FetchedAt.UTC(), Currency: copy.BaseCurrency, Value: *copy.DayPL}
			if err := tx.Save(&observation).Error; err != nil {
				return err
			}
		}
		return tx.Save(&copy).Error
	})
}

func (d *DB) GetBrokerValuation(accountID string) (*BrokerValuation, error) {
	var valuation BrokerValuation
	if err := d.Where("account_id = ?", accountID).First(&valuation).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &valuation, nil
}
