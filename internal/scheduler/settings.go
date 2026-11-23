package scheduler

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/robfig/cron/v3"
)

const settingsKey = "sync_settings:v1"

// Settings are workspace-wide. Each account still has its own calendar,
// last successful sync and persistent failure backoff.
type Settings struct {
	Mode              string `json:"mode"`
	IntervalMinutes   int    `json:"interval_minutes"`
	CloseDelayMinutes int    `json:"close_delay_minutes"`
	Maintenance       string `json:"maintenance"`
	Cron              string `json:"cron"`
}

type SettingsView struct {
	Settings Settings `json:"settings"`
	Defaults Settings `json:"defaults"`
	Source   string   `json:"source"`
}

var ErrInvalidSettings = errors.New("invalid sync settings")

func (c Settings) Validate() error {
	bad := func(message string) error { return fmt.Errorf("%w: %s", ErrInvalidSettings, message) }
	if c.Mode != "market" && c.Mode != "cron" && c.Mode != "manual" {
		return bad("choose market, cron or manual mode")
	}
	if c.IntervalMinutes < 5 || c.IntervalMinutes > 240 {
		return bad("market interval must be between 5 and 240 minutes")
	}
	if c.CloseDelayMinutes < 0 || c.CloseDelayMinutes > 120 {
		return bad("after-close delay must be between 0 and 120 minutes")
	}
	if len(c.Maintenance) > 256 || len(c.Cron) > 256 {
		return bad("schedule must be at most 256 characters")
	}
	if parsed, err := cron.ParseStandard(c.Maintenance); err != nil || parsed.Next(time.Now()).IsZero() {
		return bad("invalid daily safety schedule or timezone")
	}
	if c.Mode == "cron" {
		if parsed, err := cron.ParseStandard(c.Cron); err != nil || parsed.Next(time.Now()).IsZero() {
			return bad("invalid custom schedule or timezone")
		}
	}
	return nil
}

// ConfigureMarket supplies deployment defaults before Start. A saved dashboard
// override wins, including when a deployment still supplies the old daily cron.
func (s *Scheduler) ConfigureMarket(interval time.Duration, maintenance string) error {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	if s.started {
		return errors.New("configure deployment defaults before starting the scheduler")
	}
	defaults := s.defaults
	defaults.IntervalMinutes, defaults.Maintenance = int(interval/time.Minute), maintenance
	if err := defaults.Validate(); err != nil {
		return err
	}
	settings, saved := defaults, false
	if s.database != nil {
		raw, err := s.database.GetMeta(settingsKey)
		if err != nil {
			return err
		}
		if raw != "" {
			if err := json.Unmarshal([]byte(raw), &settings); err != nil {
				return fmt.Errorf("read saved sync settings: %w", err)
			}
			if err := settings.Validate(); err != nil {
				return err
			}
			saved = true
		}
	}
	s.defaults, s.settings, s.savedSettings = defaults, settings, saved
	return nil
}

func (s *Scheduler) Settings() SettingsView {
	s.settingsMu.RLock()
	defer s.settingsMu.RUnlock()
	source := "environment"
	if s.savedSettings {
		source = "dashboard"
	}
	return SettingsView{Settings: s.settings, Defaults: s.defaults, Source: source}
}

func (s *Scheduler) SaveSettings(settings Settings) error {
	if err := settings.Validate(); err != nil {
		return err
	}
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	return s.applySettingsLocked(settings, true)
}

func (s *Scheduler) ResetSettings() error {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	return s.applySettingsLocked(s.defaults, false)
}

func (s *Scheduler) applySettingsLocked(settings Settings, saved bool) error {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.stopped {
		return ErrSchedulerStopped
	}
	raw := ""
	if saved {
		value, err := json.Marshal(settings)
		if err != nil {
			return err
		}
		raw = string(value)
	}
	// Persist first: a failed write leaves the running plan unchanged.
	if err := s.database.SetMeta(settingsKey, raw); err != nil {
		return err
	}
	s.settings, s.savedSettings = settings, saved
	if s.started {
		s.installScheduleLocked()
	}
	return nil
}

// Every callback checks its generation under the settings lock. Jobs already
// queued by the old cron cannot begin a pull after a new plan has been saved.
// An in-flight pull may finish; saving settings never starts a worker request.
func (s *Scheduler) installScheduleLocked() {
	if s.entry != 0 {
		s.cron.Remove(s.entry)
		s.entry = 0
	}
	s.generation++
	generation := s.generation
	if s.settings.Mode == "manual" {
		return
	}
	spec := "* * * * *"
	if s.settings.Mode == "cron" {
		spec = s.settings.Cron
	}
	parsed, err := cron.ParseStandard(spec)
	if err != nil {
		log.Printf("scheduler: invalid schedule: %v", err)
		return
	}
	s.entry = s.cron.Schedule(parsed, cron.FuncJob(func() {
		s.settingsMu.RLock()
		defer s.settingsMu.RUnlock()
		if s.generation != generation {
			return
		}
		if s.settings.Mode == "market" {
			s.triggerDueLocked()
		} else if err := s.triggerAccounts(nil, true); err != nil {
			log.Printf("scheduler: %v", err)
		}
	}))
}
