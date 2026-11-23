package db

import (
	"sort"
	"time"

	"gorm.io/gorm"
)

// HistoricalPosition is one security held at the end of a past day, valued by
// DEGIRO at that day's close. It feeds the value history only. Current
// holdings, exports and Sure keep reading the synced Holding snapshots.
type HistoricalPosition struct {
	AccountID    string    `gorm:"primaryKey;default:default" json:"account_id"`
	Date         time.Time `gorm:"primaryKey" json:"date"`
	Identifier   string    `gorm:"primaryKey" json:"identifier"`
	Name         string    `json:"name"`
	Quantity     float64   `json:"quantity"`
	Close        float64   `json:"close"`
	Currency     string    `json:"currency"`
	Value        float64   `json:"value"`
	BaseCurrency string    `json:"base_currency"`
	BaseValue    *float64  `json:"base_value"`
}

const PositionReportValuationVersion = 2

// HoldingSnapshotCurrency records a complete daily snapshot even when a
// currency's last position was sold. Holding rows alone cannot represent zero.
type HoldingSnapshotCurrency struct {
	AccountID         string    `gorm:"primaryKey"`
	SnapshotDate      time.Time `gorm:"primaryKey"`
	Currency          string    `gorm:"primaryKey"`
	BaseCurrencyValue bool
}

// PositionReport records each fetched date. Version 2 retains broker base
// values, so legacy reports can be refreshed without deleting their old data.
type PositionReport struct {
	AccountID        string    `gorm:"primaryKey;default:default" json:"account_id"`
	Date             time.Time `gorm:"primaryKey" json:"date"`
	Positions        int       `json:"positions"`
	Available        bool      `json:"available"`
	BaseCurrency     string    `json:"base_currency"`
	FetchedAt        time.Time `json:"fetched_at"`
	ValuationVersion int       `json:"valuation_version"`
}

// PositionReportResult is one fetched report ready to persist.
type PositionReportResult struct {
	Date         time.Time
	Available    bool
	BaseCurrency string
	Positions    []HistoricalPosition
}

// ValuePoint is the summed value of one account's securities in one currency.
type ValuePoint struct {
	Date              string   `json:"date"`
	Currency          string   `json:"currency"`
	Value             float64  `json:"value"`
	Source            string   `json:"source"`
	NetInvested       *float64 `json:"net_invested"`
	ProfitLoss        *float64 `json:"profit_loss"`
	baseCurrencyValue bool
	converted         bool
}

// dayKey formats a stored date as YYYY-MM-DD in its own zone: synced holdings
// use local midnight, reports UTC midnight, and both name the calendar day.
func dayKey(t time.Time) string { return t.Format("2006-01-02") }

// Day normalizes a report date to UTC midnight, the stored form.
func Day(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// SavePositionReports replaces the stored positions for each reported date.
func (d *DB) SavePositionReports(accountID string, reports []PositionReportResult, fetchedAt time.Time) error {
	return d.DB.Transaction(func(tx *gorm.DB) error {
		for _, report := range reports {
			day := Day(report.Date)
			version := PositionReportValuationVersion
			if report.Available && len(report.Positions) == 0 && !valuationCurrency.MatchString(report.BaseCurrency) {
				version = 1
			}
			for _, position := range report.Positions {
				if position.BaseValue == nil || position.BaseCurrency == "" {
					version = 1
				}
			}
			if err := tx.Where("account_id = ? AND date = ?", accountID, day).Delete(&HistoricalPosition{}).Error; err != nil {
				return err
			}
			rows := make([]HistoricalPosition, 0, len(report.Positions))
			for _, position := range report.Positions {
				position.AccountID, position.Date = accountID, day
				rows = append(rows, position)
			}
			if len(rows) > 0 {
				if err := tx.Create(&rows).Error; err != nil {
					return err
				}
			}
			record := PositionReport{AccountID: accountID, Date: day, Positions: len(rows), Available: report.Available, BaseCurrency: report.BaseCurrency, FetchedAt: fetchedAt, ValuationVersion: version}
			if err := tx.Save(&record).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// FirstTransactionDate returns the account's earliest saved transaction date.
func (d *DB) FirstTransactionDate(accountID string) (time.Time, bool, error) {
	var first Transaction
	err := d.DB.Where("account_id = ?", accountID).Order("date ASC").Limit(1).Find(&first).Error
	if err != nil || first.ID == "" {
		return time.Time{}, false, err
	}
	return first.Date, true, nil
}

// ValueHistoryCoverage returns dates with a current-format report or a synced
// snapshot. Older reports remain visible while their base values are refreshed.
func (d *DB) ValueHistoryCoverage(accountID string) (map[string]bool, error) {
	covered := map[string]bool{}
	var reported []PositionReport
	if err := d.Where("account_id = ? AND valuation_version >= ?", accountID, PositionReportValuationVersion).Find(&reported).Error; err != nil {
		return nil, err
	}
	for _, report := range reported {
		// An empty available report needs its own currency: there is no
		// position row from which a verified zero can be recovered.
		if report.Available && report.Positions == 0 && !valuationCurrency.MatchString(report.BaseCurrency) {
			continue
		}
		covered[dayKey(report.Date)] = true
	}
	var snapshots []time.Time
	if err := d.DB.Model(&Holding{}).Where("account_id = ?", accountID).Distinct("snapshot_date").Pluck("snapshot_date", &snapshots).Error; err != nil {
		return nil, err
	}
	for _, day := range snapshots {
		covered[dayKey(day)] = true
	}
	var markers []time.Time
	if err := d.Model(&HoldingSnapshotCurrency{}).Where("account_id = ?", accountID).Distinct("snapshot_date").Pluck("snapshot_date", &markers).Error; err != nil {
		return nil, err
	}
	for _, day := range markers {
		covered[dayKey(day)] = true
	}
	return covered, nil
}

// ValueHistory merges reconstructed history with synced snapshots, one point
// per date and currency. A synced snapshot wins over a report for the same date.
func (d *DB) ValueHistory(accountID string) ([]ValuePoint, error) {
	type row struct {
		Date              time.Time
		Currency          string
		Value             float64
		BaseCurrencyValue bool
		Converted         bool
	}
	points := map[string]ValuePoint{}
	var reported []row
	if err := d.DB.Model(&HistoricalPosition{}).Where("account_id = ?", accountID).
		Select("date, CASE WHEN base_value IS NOT NULL AND base_currency <> '' THEN base_currency ELSE currency END AS currency, SUM(CASE WHEN base_value IS NOT NULL AND base_currency <> '' THEN base_value ELSE value END) AS value, MAX(CASE WHEN base_value IS NOT NULL AND base_currency <> '' THEN 1 ELSE 0 END) AS base_currency_value, MAX(CASE WHEN base_value IS NOT NULL AND base_currency <> '' AND base_currency <> currency THEN 1 ELSE 0 END) AS converted").
		Group("date, CASE WHEN base_value IS NOT NULL AND base_currency <> '' THEN base_currency ELSE currency END").Scan(&reported).Error; err != nil {
		return nil, err
	}
	for _, r := range reported {
		key := dayKey(r.Date) + "|" + r.Currency
		points[key] = ValuePoint{Date: dayKey(r.Date), Currency: r.Currency, Value: r.Value, Source: "report", baseCurrencyValue: r.BaseCurrencyValue, converted: r.Converted}
	}
	var emptyReports []PositionReport
	if err := d.Where("account_id = ? AND available = ? AND positions = 0", accountID, true).Find(&emptyReports).Error; err != nil {
		return nil, err
	}
	for _, report := range emptyReports {
		if !valuationCurrency.MatchString(report.BaseCurrency) {
			continue
		}
		day := dayKey(report.Date)
		points[day+"|"+report.BaseCurrency] = ValuePoint{Date: day, Currency: report.BaseCurrency, Source: "report", baseCurrencyValue: true}
	}
	var snapshots []row
	if err := d.DB.Model(&Holding{}).Where("account_id = ?", accountID).
		Select("snapshot_date AS date, COALESCE(NULLIF(value_currency, ''), currency) AS currency, SUM(market_value) AS value, MAX(CASE WHEN value_currency <> '' THEN 1 ELSE 0 END) AS base_currency_value, MAX(CASE WHEN value_currency <> '' AND value_currency <> currency THEN 1 ELSE 0 END) AS converted").
		Group("snapshot_date, COALESCE(NULLIF(value_currency, ''), currency)").Scan(&snapshots).Error; err != nil {
		return nil, err
	}
	snapshotDays := map[string]bool{}
	for _, r := range snapshots {
		snapshotDays[dayKey(r.Date)] = true
	}
	var markers []HoldingSnapshotCurrency
	if err := d.Where("account_id = ?", accountID).Find(&markers).Error; err != nil {
		return nil, err
	}
	for _, marker := range markers {
		snapshotDays[dayKey(marker.SnapshotDate)] = true
	}
	// A snapshot replaces every report point of its day, also in currencies the
	// snapshot no longer holds, so a sold-out currency cannot linger there.
	for key, point := range points {
		if snapshotDays[point.Date] {
			delete(points, key)
		}
	}
	for _, marker := range markers {
		day := dayKey(marker.SnapshotDate)
		points[day+"|"+marker.Currency] = ValuePoint{Date: day, Currency: marker.Currency, Source: "snapshot", baseCurrencyValue: marker.BaseCurrencyValue}
	}
	for _, r := range snapshots {
		key := dayKey(r.Date) + "|" + r.Currency
		points[key] = ValuePoint{Date: dayKey(r.Date), Currency: r.Currency, Value: r.Value, Source: "snapshot", baseCurrencyValue: r.BaseCurrencyValue, converted: r.Converted}
	}
	out := make([]ValuePoint, 0, len(points))
	for _, point := range points {
		out = append(out, point)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Date != out[j].Date {
			return out[i].Date < out[j].Date
		}
		return out[i].Currency < out[j].Currency
	})
	if err := d.addTradePerformance(accountID, out); err != nil {
		return nil, err
	}
	return out, nil
}
