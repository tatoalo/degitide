package db

import (
	"math"
	"time"

	"github.com/tatoalo/degitide/internal/market"
	"gorm.io/gorm"
)

// DayPLObservation retains the broker's day result, not a calculated return.
// Collection time is kept so an intraday reading is never called a closing result.
type DayPLObservation struct {
	AccountID string    `gorm:"primaryKey"`
	FetchedAt time.Time `gorm:"primaryKey"`
	Currency  string
	Value     float64
}

type SessionDayPL struct {
	Date       string     `json:"date"`
	Currency   string     `json:"currency"`
	Value      *float64   `json:"value"`
	RecordedAt *time.Time `json:"recorded_at"`
}

func (d *DB) backfillDayPLObservations() error {
	return d.Transaction(func(tx *gorm.DB) error {
		var values []BrokerValuation
		if err := tx.Where("day_pl IS NOT NULL").Find(&values).Error; err != nil {
			return err
		}
		for _, value := range values {
			observation := DayPLObservation{AccountID: value.AccountID, FetchedAt: value.FetchedAt.UTC(), Currency: value.BaseCurrency, Value: *value.DayPL}
			if err := tx.Clauses(onConflictDoNothing()).Create(&observation).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// PreviousSessionDayPL is a display alternative for a zero collected overnight.
// Keep the current broker figures intact. A real zero during/after a session,
// partial calendar coverage, or incompatible market dates must not be replaced.
func (d *DB) PreviousSessionDayPL(accountID string, current *BrokerValuation) (*SessionDayPL, error) {
	if current == nil || current.DayPL == nil || math.Abs(*current.DayPL) >= .005 {
		return nil, nil
	}
	plan, err := d.MarketSchedule(accountID)
	if err != nil || plan == nil {
		return nil, err
	}
	at := current.FetchedAt
	if plan.UnknownHoldings != 0 || len(plan.Markets) == 0 || !at.Before(plan.ValidUntil) {
		return nil, nil
	}
	var start, end time.Time
	result := &SessionDayPL{Currency: current.BaseCurrency}
	for _, venue := range plan.Markets {
		location, err := time.LoadLocation(venue.Timezone)
		if err != nil {
			return nil, nil
		}
		var previous *market.Session
		for i := range venue.Sessions {
			session := &venue.Sessions[i]
			// Include lunch breaks: a zero during the trading day is still a zero.
			if !at.Before(session.Open) && at.Before(session.Close) {
				return nil, nil
			}
			if !session.Close.After(at) && (previous == nil || session.Close.After(previous.Close)) {
				previous = session
			}
		}
		if previous == nil {
			return nil, nil
		}
		close := previous.Close.In(location)
		day := close.Format(time.DateOnly)
		if day == at.In(location).Format(time.DateOnly) || (result.Date != "" && result.Date != day) {
			return nil, nil
		}
		result.Date = day
		if previous.Open.After(start) {
			start = previous.Open
		}
		midnight := time.Date(close.Year(), close.Month(), close.Day()+1, 0, 0, 0, 0, location)
		if end.IsZero() || midnight.Before(end) {
			end = midnight
		}
	}
	var saved DayPLObservation
	query := d.Where("account_id = ? AND currency = ? AND fetched_at >= ? AND fetched_at < ?",
		accountID, current.BaseCurrency, start.UTC(), end.UTC()).Order("fetched_at DESC").Limit(1).Find(&saved)
	if query.Error != nil {
		return nil, query.Error
	}
	if query.RowsAffected != 0 {
		result.Value, result.RecordedAt = &saved.Value, &saved.FetchedAt
	}
	// An explicit missing result lets the UI show a dash, not a reset zero or an
	// invented result. Only the immediately preceding session can supply a value.
	return result, nil
}
