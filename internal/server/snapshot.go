package server

import (
	"net/http"

	"github.com/tatoalo/degitide/internal/db"
	"gorm.io/gorm"
)

// Read the dashboard's financial data in one SQLite snapshot. Separate API
// requests can otherwise straddle a sync and combine old and new amounts.
func (s *Server) handlePortfolioSnapshot(w http.ResponseWriter, r *http.Request) {
	account, ok := s.requestAccount(w, r)
	if !ok {
		return
	}
	result := struct {
		Holdings             []db.Holding        `json:"holdings"`
		Transactions         []db.Transaction    `json:"transactions"`
		History              []db.ValuePoint     `json:"history"`
		Valuation            *db.BrokerValuation `json:"valuation"`
		PreviousSessionDayPL *db.SessionDayPL    `json:"previous_session_day_pl,omitempty"`
	}{Holdings: []db.Holding{}, Transactions: []db.Transaction{}, History: []db.ValuePoint{}}
	err := s.db.Transaction(func(tx *gorm.DB) error {
		data := &db.DB{DB: tx}
		if err := data.Where("id = ?", account.ID).First(&account).Error; err != nil {
			return err
		}
		if account.SnapshotDate != nil {
			if err := data.Where("account_id = ? AND snapshot_date = ?", account.ID, *account.SnapshotDate).
				Order("symbol ASC").Find(&result.Holdings).Error; err != nil {
				return err
			}
		} else {
			var latest db.Holding
			if err := data.Where("account_id = ?", account.ID).Order("snapshot_date DESC").Limit(1).Find(&latest).Error; err != nil {
				return err
			}
			if !latest.SnapshotDate.IsZero() {
				if err := data.Where("account_id = ? AND snapshot_date = ?", account.ID, latest.SnapshotDate).
					Order("symbol ASC").Find(&result.Holdings).Error; err != nil {
					return err
				}
			}
		}
		if err := data.Where("account_id = ?", account.ID).Order("date DESC").Limit(100).Find(&result.Transactions).Error; err != nil {
			return err
		}
		var err error
		if result.History, err = data.ValueHistory(account.ID); err != nil {
			return err
		}
		result.Valuation, err = data.GetBrokerValuation(account.ID)
		if err != nil {
			return err
		}
		result.PreviousSessionDayPL, err = data.PreviousSessionDayPL(account.ID, result.Valuation)
		return err
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, result)
}
