package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/tatoalo/degitide/internal/scheduler"
)

// handleSyncEvents observes the global run, regardless of which account or
// client started it. Sampling only in-memory progress never wakes the worker.
func (s *Server) handleSyncEvents(w http.ResponseWriter, r *http.Request) {
	reporter, ok := s.scheduler.(interface{ Progress() scheduler.Progress })
	if !ok {
		writeError(w, http.StatusNotImplemented, errors.New("live sync progress unavailable"))
		return
	}
	if _, ok := w.(http.Flusher); !ok {
		writeError(w, http.StatusNotImplemented, errors.New("streaming unavailable"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	controller := http.NewResponseController(w)
	defer controller.SetWriteDeadline(time.Time{})
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	// Periodic reconnects recheck dashboard authentication.
	lifetime := time.NewTimer(time.Minute)
	defer lifetime.Stop()
	var previous []byte
	lastWrite := time.Time{}
	for {
		progress := reporter.Progress()
		payload, err := json.Marshal(struct {
			Running  bool               `json:"pull_running"`
			Progress scheduler.Progress `json:"progress"`
		}{progress.Running, progress})
		if err != nil {
			return
		}
		changed := !bytes.Equal(payload, previous)
		if changed || time.Since(lastWrite) >= 15*time.Second {
			// Bound writes to disconnected or stalled clients.
			_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if changed {
				_, err = fmt.Fprintf(w, "data: %s\n\n", payload)
			} else {
				_, err = fmt.Fprint(w, ": keepalive\n\n")
			}
			if err != nil || controller.Flush() != nil {
				return
			}
			_ = controller.SetWriteDeadline(time.Time{})
			previous, lastWrite = payload, time.Now()
		}
		select {
		case <-r.Context().Done():
			return
		case <-lifetime.C:
			return
		case <-ticker.C:
		}
	}
}
