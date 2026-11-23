package db

import (
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Existing nonempty dates can be reconstructed from holdings. Only the latest
// empty date is recoverable from legacy accounts; older empty dates were not
// recorded, so this migration does not invent them.
func (d *DB) backfillHoldingSnapshotCurrencies() error {
	return d.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`INSERT OR IGNORE INTO holding_snapshot_currencies
 (account_id, snapshot_date, currency, base_currency_value)
 SELECT account_id, snapshot_date, COALESCE(NULLIF(value_currency, ''), currency),
 MAX(CASE WHEN value_currency <> '' THEN 1 ELSE 0 END)
 FROM holdings WHERE COALESCE(NULLIF(value_currency, ''), currency) <> ''
 GROUP BY account_id, snapshot_date, COALESCE(NULLIF(value_currency, ''), currency)`).Error; err != nil {
			return err
		}
		return tx.Exec(`INSERT OR IGNORE INTO holding_snapshot_currencies
 (account_id, snapshot_date, currency, base_currency_value)
 SELECT a.id, a.snapshot_date, p.currency,
 COALESCE((SELECT MAX(s.base_currency_value) FROM holding_snapshot_currencies s
 WHERE s.account_id = a.id AND s.currency = p.currency), 0)
 FROM accounts a JOIN portfolio_currencies p ON p.account_id = a.id
 WHERE a.snapshot_date IS NOT NULL`).Error
	})
}

// Save a marker for every currency already tracked on or before this date,
// including currencies missing from this complete replacement. Later currency
// discoveries must not invent zero balances before their first known snapshot.
func saveHoldingSnapshotCurrencies(tx *gorm.DB, accountID string, day time.Time, holdings []Holding) error {
	var currencies []string
	if err := tx.Model(&HoldingSnapshotCurrency{}).Where("account_id = ? AND snapshot_date <= ?", accountID, day).Distinct("currency").Pluck("currency", &currencies).Error; err != nil {
		return err
	}
	var baseCurrencies []string
	if err := tx.Model(&HoldingSnapshotCurrency{}).Where("account_id = ? AND snapshot_date <= ? AND base_currency_value = ?", accountID, day, true).Distinct("currency").Pluck("currency", &baseCurrencies).Error; err != nil {
		return err
	}
	known := map[string]bool{}
	for _, currency := range currencies {
		known[currency] = true
	}
	base := map[string]bool{}
	for _, currency := range baseCurrencies {
		base[currency] = true
	}
	for _, holding := range holdings {
		currency := holding.MarketCurrency()
		if currency != "" && !known[currency] {
			currencies = append(currencies, currency)
			known[currency] = true
		}
		if holding.ValueCurrency != "" {
			base[holding.ValueCurrency] = true
		}
	}
	markers := make([]HoldingSnapshotCurrency, 0, len(currencies))
	for _, currency := range currencies {
		markers = append(markers, HoldingSnapshotCurrency{AccountID: accountID, SnapshotDate: day, Currency: currency, BaseCurrencyValue: base[currency]})
	}
	if len(markers) == 0 {
		return nil
	}
	return tx.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "account_id"}, {Name: "snapshot_date"}, {Name: "currency"}},
		DoUpdates: clause.AssignmentColumns([]string{"base_currency_value"}),
	}).Create(&markers).Error
}
