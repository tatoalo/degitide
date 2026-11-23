package scheduler

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"github.com/tatoalo/degitide/internal/db"
)

const settingsKey = "sync_settings:v1"

// Accounts inherit the workspace settings until they have a saved override.
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
	overrides := make(map[string]Settings)
	if s.database != nil {
		var rows []db.Meta
		if err := s.database.Where("key = ? OR key GLOB ?", settingsKey, settingsKey+":*").Find(&rows).Error; err != nil {
			return err
		}
		for _, row := range rows {
			if row.Value == "" {
				continue
			}
			_, accountID, _ := strings.Cut(row.Key, settingsKey+":")
			settings := defaults
			if err := json.Unmarshal([]byte(row.Value), &settings); err != nil {
				return fmt.Errorf("read saved sync settings: %w", err)
			}
			if err := settings.Validate(); err != nil {
				return err
			}
			overrides[accountID] = settings
		}
	}
	s.defaults, s.overrides = defaults, overrides
	return nil
}

func (s *Scheduler) settingsLocked(accountID string) SettingsView {
	view := SettingsView{Settings: s.defaults, Defaults: s.defaults, Source: "environment"}
	if workspace, ok := s.overrides[""]; ok {
		view.Settings, view.Source = workspace, "dashboard"
	}
	if accountID != "" {
		view.Defaults, view.Source = view.Settings, "workspace"
		if account, ok := s.overrides[accountID]; ok {
			view.Settings, view.Source = account, "account"
		}
	}
	return view
}

func (s *Scheduler) Settings(accountID string) SettingsView {
	s.settingsMu.RLock()
	defer s.settingsMu.RUnlock()
	return s.settingsLocked(accountID)
}

func (s *Scheduler) SaveSettings(accountID string, settings Settings) error {
	if err := settings.Validate(); err != nil {
		return err
	}
	return s.applySettings(accountID, &settings)
}

func (s *Scheduler) ResetSettings(accountID string) error {
	return s.applySettings(accountID, nil)
}

func (s *Scheduler) applySettings(accountID string, settings *Settings) error {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.stopped {
		return ErrSchedulerStopped
	}
	key := settingsKey
	if accountID != "" {
		key += ":" + accountID
	}
	if settings == nil {
		if err := s.database.Delete(&db.Meta{}, "key = ?", key).Error; err != nil {
			return err
		}
		delete(s.overrides, accountID)
	} else {
		value, err := json.Marshal(settings)
		if err != nil {
			return err
		}
		if err := s.database.SetMeta(key, string(value)); err != nil {
			return err
		}
		s.overrides[accountID] = *settings
	}
	if s.started {
		s.installScheduleLocked()
	}
	return nil
}

// Market accounts share a minute tick. Custom jobs share a timer only when
// their cron expressions match. Keep unchanged timers (including @every) in phase.
func (s *Scheduler) installScheduleLocked() {
	rules := make(map[string]string)
	add := func(settings Settings) {
		switch settings.Mode {
		case "market":
			rules["market"] = "* * * * *"
		case "cron":
			rules[settings.Cron] = settings.Cron
		}
	}
	add(s.settingsLocked("").Settings)
	for accountID, settings := range s.overrides {
		if accountID != "" {
			add(settings)
		}
	}
	for rule, entry := range s.entries {
		if _, keep := rules[rule]; !keep {
			s.cron.Remove(entry)
			delete(s.entries, rule)
		}
	}
	for id, spec := range s.pendingCron {
		settings := s.settingsLocked(id).Settings
		if settings.Mode != "cron" || settings.Cron != spec {
			delete(s.pendingCron, id)
		}
	}
	for rule, spec := range rules {
		if _, exists := s.entries[rule]; exists {
			continue
		}
		var entry cron.EntryID
		var err error
		entry, err = s.cron.AddFunc(spec, func() {
			s.settingsMu.Lock()
			defer s.settingsMu.Unlock()
			if s.entries[rule] != entry {
				return
			}
			s.runRuleLocked(rule)
		})
		if err != nil {
			log.Printf("scheduler: invalid schedule: %v", err)
			continue
		}
		s.entries[rule] = entry
	}
}

func (s *Scheduler) runRuleLocked(rule string) {
	if rule == "market" {
		s.triggerDueLocked()
		return
	}
	accounts, err := s.database.Accounts()
	if err != nil {
		log.Printf("scheduler: list accounts: %v", err)
		return
	}
	for _, account := range accounts {
		settings := s.settingsLocked(account.ID).Settings
		if settings.Mode == "cron" && settings.Cron == rule {
			s.pendingCron[account.ID] = rule
		}
	}
	if progress := s.Progress(); progress.Running {
		for _, active := range progress.Accounts {
			delete(s.pendingCron, active.ID)
		}
	}
	s.startPendingCronLocked()
}

// A second timer may fire while the worker is syncing another account.
// Coalesce repeated ticks for each account and drain them when the worker is free.
func (s *Scheduler) startPendingCronLocked() {
	if len(s.pendingCron) == 0 || s.IsRunning() {
		return
	}
	ids := make([]string, 0, len(s.pendingCron))
	for id := range s.pendingCron {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	err := s.triggerAccounts(ids, true)
	if errors.Is(err, ErrPullInProgress) {
		return
	}
	if err != nil && !errors.Is(err, ErrSchedulerStopped) {
		log.Printf("scheduler: start scheduled accounts: %v", err)
	}
	clear(s.pendingCron)
}
