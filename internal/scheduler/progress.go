package scheduler

import "time"

type AccountProgress struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	State     string `json:"state"`
	Stage     string `json:"stage"`
	Completed int    `json:"completed"`
	Error     string `json:"error,omitempty"`
}

type Progress struct {
	Running    bool              `json:"running"`
	Percent    int               `json:"percent"`
	Stage      string            `json:"stage"`
	StartedAt  time.Time         `json:"started_at"`
	FinishedAt time.Time         `json:"finished_at"`
	Accounts   []AccountProgress `json:"accounts"`
}

func (s *Scheduler) Progress() Progress {
	s.progressMu.RLock()
	defer s.progressMu.RUnlock()
	p := s.progress
	p.Accounts = append([]AccountProgress(nil), p.Accounts...)
	return p
}

func (s *Scheduler) stage(index, completed int, label string) {
	s.progressMu.Lock()
	defer s.progressMu.Unlock()
	a := &s.progress.Accounts[index]
	a.State, a.Stage, a.Completed = "running", label, completed
	s.progress.Stage = a.Name + " · " + label
	s.progress.Percent = (index*4 + completed) * 100 / (len(s.progress.Accounts) * 4)
}
