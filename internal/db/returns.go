package db

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// PricePeriod is the change in one security's DEGIRO closing price over a period.
type PricePeriod struct {
	Label      string   `json:"label"`
	StartDate  string   `json:"start_date"`
	StartClose float64  `json:"start_close"`
	Years      float64  `json:"years"`
	Total      float64  `json:"total"`
	Annualized *float64 `json:"annualized"`
}

// PriceReturn summarizes one security's price change in its trading currency.
type PriceReturn struct {
	Identifier string        `json:"identifier"`
	Name       string        `json:"name"`
	Currency   string        `json:"currency"`
	EndDate    string        `json:"end_date"`
	EndClose   float64       `json:"end_close"`
	Weekly     int           `json:"weekly_closes"`
	Periods    []PricePeriod `json:"periods"`
}

type pricePoint struct {
	day   time.Time
	close float64
}

// priceStartTolerance is how far before a period's start the nearest close may
// lie. A longer gap (the security was not held then) skips the period instead
// of measuring from a stale price.
const priceStartTolerance = 14 * 24 * time.Hour

// PriceReturns derives price returns per security from the weekly DEGIRO
// closes in historical_positions. A series ends at its newest close or, when
// newer, at the price implied by the latest synced snapshot (value / quantity).
// These are price returns while the security was held: they include
// reinvested income only for accumulating funds and exclude your own trades.
func (d *DB) PriceReturns(accountID string) ([]PriceReturn, error) {
	var rows []HistoricalPosition
	if err := d.DB.Where("account_id = ? AND close > 0", accountID).
		Order("identifier ASC, currency ASC, date ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	series := map[string][]pricePoint{}
	names := map[string]string{}
	var order []string
	for _, r := range rows {
		key := r.Identifier + "|" + r.Currency
		if _, seen := series[key]; !seen {
			order = append(order, key)
		}
		series[key] = append(series[key], pricePoint{Day(r.Date), r.Close})
		names[key] = r.Name
	}
	weekly := map[string]int{}
	for key, points := range series {
		weekly[key] = len(points)
	}

	var latest []Holding
	if err := d.DB.Where("account_id = ? AND snapshot_date = (SELECT MAX(snapshot_date) FROM holdings WHERE account_id = ?)", accountID, accountID).
		Find(&latest).Error; err != nil {
		return nil, err
	}
	for _, h := range latest {
		if h.ISIN == "" || h.Quantity <= 0 || h.MarketValue <= 0 {
			continue
		}
		key := strings.ToUpper(h.ISIN) + "|" + h.Currency
		points, ok := series[key]
		if !ok {
			continue
		}
		// A converted market value is not the instrument's native price.
		// Prefer the preserved quote and its timestamp over the collection day.
		close := h.MarketValue / h.Quantity
		if h.Price != nil {
			close = *h.Price
		} else if h.MarketCurrency() != h.Currency {
			continue
		}
		if close <= 0 || math.IsNaN(close) || math.IsInf(close, 0) {
			continue
		}
		day := Day(h.SnapshotDate)
		if h.PriceAsOf != "" {
			asOf, err := time.Parse(time.RFC3339, h.PriceAsOf)
			if err != nil {
				asOf, err = time.Parse(time.DateOnly, h.PriceAsOf)
			}
			if err != nil {
				continue
			}
			day = Day(asOf)
		}
		if day.After(points[len(points)-1].day) {
			series[key] = append(points, pricePoint{day, close})
		}
		if h.Name != "" {
			names[key] = h.Name
		}
	}

	out := []PriceReturn{}
	for _, key := range order {
		points := series[key]
		if len(points) < 2 {
			continue
		}
		parts := strings.SplitN(key, "|", 2)
		end := points[len(points)-1]
		result := PriceReturn{
			Identifier: parts[0], Currency: parts[1], Name: names[key],
			EndDate: end.day.Format("2006-01-02"), EndClose: end.close, Weekly: weekly[key],
		}
		for _, years := range []int{1, 3, 5} {
			target := end.day.AddDate(-years, 0, 0)
			start, ok := closeOnOrBefore(points, target)
			if !ok || target.Sub(start.day) > priceStartTolerance {
				continue
			}
			label := "1 year"
			if years > 1 {
				label = fmt.Sprintf("%d years", years)
			}
			result.Periods = append(result.Periods, pricePeriod(label, start, end))
		}
		// Skip "since first held" when a fixed period already starts there.
		first := points[0].day.Format("2006-01-02")
		if n := len(result.Periods); n == 0 || result.Periods[n-1].StartDate != first {
			result.Periods = append(result.Periods, pricePeriod("Since first held", points[0], end))
		}
		out = append(out, result)
	}
	return out, nil
}

func closeOnOrBefore(points []pricePoint, day time.Time) (pricePoint, bool) {
	for i := len(points) - 1; i >= 0; i-- {
		if !points[i].day.After(day) {
			return points[i], true
		}
	}
	return pricePoint{}, false
}

func pricePeriod(label string, start, end pricePoint) PricePeriod {
	years := end.day.Sub(start.day).Hours() / 24 / 365.25
	ratio := end.close / start.close
	period := PricePeriod{
		Label: label, StartDate: start.day.Format("2006-01-02"), StartClose: start.close,
		Years: math.Round(years*1000) / 1000, Total: (ratio - 1) * 100,
	}
	// Annualizing a shorter span would extrapolate a few weeks into a year.
	if years >= 1 {
		annualized := (math.Pow(ratio, 1/years) - 1) * 100
		period.Annualized = &annualized
	}
	return period
}
