package server

import "net/http"

func (s *Server) handlePortfolioValuation(w http.ResponseWriter, r *http.Request) {
	account, ok := s.requestAccount(w, r)
	if !ok {
		return
	}
	valuation, err := s.db.GetBrokerValuation(account.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, valuation)
}
