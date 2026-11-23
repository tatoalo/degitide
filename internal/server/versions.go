package server

import (
	"context"
	"net/http"
	"time"

	"github.com/tatoalo/degitide/internal/browser"
)

// Version checks are independent of account refreshes so an unavailable worker
// never delays the saved portfolio. No browser session is opened.
func (s *Server) handleVersions(w http.ResponseWriter, r *http.Request) {
	version := s.options.Version
	if version == "" {
		version = "dev"
	}
	response := struct {
		App    string              `json:"app"`
		Worker *browser.WorkerInfo `json:"worker"`
	}{App: version}
	if s.worker != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if info, err := s.worker.Info(ctx); err == nil {
			response.Worker = info
		}
	}
	writeJSON(w, response)
}
