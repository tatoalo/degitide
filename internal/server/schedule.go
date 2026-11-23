package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/tatoalo/degitide/internal/scheduler"
)

func (s *Server) handleSchedule(w http.ResponseWriter, r *http.Request) {
	planner, ok := s.scheduler.(interface {
		Settings() scheduler.SettingsView
		SaveSettings(scheduler.Settings) error
		ResetSettings() error
	})
	if !ok {
		writeError(w, http.StatusServiceUnavailable, errors.New("sync settings unavailable"))
		return
	}
	var err error
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
		err = planner.SaveSettings(settings)
	case http.MethodDelete:
		err = planner.ResetSettings()
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
	writeJSON(w, planner.Settings())
}
