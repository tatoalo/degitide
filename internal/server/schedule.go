package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"

	"github.com/tatoalo/degitide/internal/scheduler"
)

func (s *Server) handleSchedule(w http.ResponseWriter, r *http.Request) {
	planner, ok := s.scheduler.(interface {
		Settings(string) scheduler.SettingsView
		SaveSettings(string, scheduler.Settings) error
		ResetSettings(string) error
	})
	if !ok {
		writeError(w, http.StatusServiceUnavailable, errors.New("sync settings unavailable"))
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("invalid schedule query"))
		return
	}
	accountID := ""
	if values, scoped := query["account"]; scoped {
		if len(values) != 1 || values[0] == "" {
			writeError(w, http.StatusBadRequest, errors.New("expected one account ID"))
			return
		}
		account, ok := s.requestAccount(w, r)
		if !ok {
			return
		}
		accountID = account.ID
	}
	switch r.Method {
	case http.MethodPut:
		var settings scheduler.Settings
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2048))
		decoder.DisallowUnknownFields()
		if err = decoder.Decode(&settings); err != nil {
			writeError(w, http.StatusBadRequest, errors.New("invalid sync settings request"))
			return
		}
		if decoder.Decode(new(any)) != io.EOF {
			writeError(w, http.StatusBadRequest, errors.New("expected one settings object"))
			return
		}
		err = planner.SaveSettings(accountID, settings)
	case http.MethodDelete:
		err = planner.ResetSettings(accountID)
	}
	if err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, scheduler.ErrInvalidSettings) {
			code = http.StatusBadRequest
		} else if errors.Is(err, scheduler.ErrSchedulerStopped) {
			code = http.StatusConflict
		}
		writeError(w, code, err)
		return
	}
	writeJSON(w, planner.Settings(accountID))
}
