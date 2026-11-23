package db

import (
	"math"
	"strings"
)

// addTradePerformance compares securities value with cumulative buys minus sales
// in the same currency. This includes realized and unrealized trading P&L, but
// not cash, dividends, taxes or fees (fees can be in a different currency).
// Transfers have no recorded cost basis and can distort this comparison.
// It is not a deposit balance or a time-weighted return.
func (d *DB) addTradePerformance(accountID string, points []ValuePoint) error {
	backfilled, err := d.GetMeta("transactions_backfilled:" + accountID)
	if err != nil || backfilled == "" || len(points) == 0 {
		return err
	}
	var trades []Transaction
	if err := d.Where("account_id = ?", accountID).
		Select("date, type, quantity, price, currency").Order("date ASC").Find(&trades).Error; err != nil {
		return err
	}
	net := map[string]float64{}
	seen := map[string]bool{}
	next := 0
	for i := range points {
		point := &points[i]
		for next < len(trades) && dayKey(trades[next].Date) <= point.Date {
			trade := trades[next]
			next++
			amount := math.Abs(trade.Quantity * trade.Price)
			if math.IsNaN(amount) || math.IsInf(amount, 0) || trade.Price < 0 {
				continue
			}
			switch strings.ToUpper(trade.Type) {
			case "BUY":
				net[trade.Currency] += amount
			case "SELL":
				net[trade.Currency] -= amount
			default:
				continue
			}
			seen[trade.Currency] = true
		}
		// Unknown cost is not zero cost. Keep values unavailable until a
		// currency has saved trades and the full-history import has completed.
		// Base-currency valuations include foreign assets. Native-currency
		// trades cannot supply their historical FX-adjusted cost or proceeds.
		// Missing cost data must not turn into an overstated profit figure.
		canCompare := !point.converted
		if point.baseCurrencyValue {
			for currency := range seen {
				if currency != point.Currency {
					canCompare = false
				}
			}
		}
		if canCompare && seen[point.Currency] {
			invested := net[point.Currency]
			profit := point.Value - invested
			point.NetInvested, point.ProfitLoss = &invested, &profit
		}
	}
	return nil
}
