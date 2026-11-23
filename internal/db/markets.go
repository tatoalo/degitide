package db

import (
	"encoding/json"
	"github.com/tatoalo/degitide/internal/market"
)

func (d *DB) SaveMarketSchedule(accountID string, schedule *market.Schedule) error {
	encoded, err := json.Marshal(schedule)
	if err != nil {
		return err
	}
	return d.SetMeta("market_schedule:"+accountID, string(encoded))
}

func (d *DB) MarketSchedule(accountID string) (*market.Schedule, error) {
	value, err := d.GetMeta("market_schedule:" + accountID)
	if err != nil || value == "" {
		return nil, err
	}
	var schedule *market.Schedule
	err = json.Unmarshal([]byte(value), &schedule)
	return schedule, err
}
