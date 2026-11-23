package scheduler

import (
	"encoding/json"
	"log"
	"os"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"github.com/tatoalo/degitide/internal/db"
	"github.com/tatoalo/degitide/internal/market"
)

type MarketStatus struct {
	MIC      string `json:"mic"`
	Name     string `json:"name"`
	Timezone string `json:"timezone"`
	Open     bool   `json:"open"`
}

type ScheduleStatus struct {
	Mode              string         `json:"mode"`
	CloseDelayMinutes int            `json:"close_delay_minutes"`
	IntervalMinutes   int            `json:"interval_minutes,omitempty"`
	NextSync          *time.Time     `json:"next_sync,omitempty"`
	Reason            string         `json:"reason"`
	Markets           []MarketStatus `json:"markets"`
	UnknownHoldings   int            `json:"unknown_holdings"`
	CalendarAvailable bool           `json:"calendar_available"`
	Cron              string         `json:"cron,omitempty"`
	Timezone          string         `json:"timezone,omitempty"`
	ServerTimezone    string         `json:"server_timezone,omitempty"`
	UpcomingRuns      []time.Time    `json:"upcoming_runs,omitempty"`
}

// serverTimezone names the process time zone so the dashboard can show server
// time next to browser time. Go names a zone loaded from /etc/localtime
// "Local", so resolve the IANA name from TZ or the symlink. Empty means unknown.
func serverTimezone() string {
	candidates := []string{strings.TrimPrefix(os.Getenv("TZ"), ":")}
	if target, err := os.Readlink("/etc/localtime"); err == nil {
		if _, name, ok := strings.Cut(target, "zoneinfo/"); ok {
			candidates = append(candidates, name)
		}
	}
	for _, name := range candidates {
		if name == "" || name == "Local" {
			continue
		}
		if _, err := time.LoadLocation(name); err == nil {
			return name
		}
	}
	if _, offset := time.Now().Zone(); offset == 0 {
		return "UTC"
	}
	return ""
}

type retryState struct {
	Attempt  time.Time `json:"attempt"`
	After    time.Time `json:"after"`
	Failures int       `json:"failures"`
}

func (s *Scheduler) retry(accountID string) (retryState, error) {
	var state retryState
	value, err := s.database.GetMeta("sync_retry:" + accountID)
	if err != nil || value == "" {
		return state, err
	}
	err = json.Unmarshal([]byte(value), &state)
	return state, err
}

func (s *Scheduler) recordAttempt(accountID string, now time.Time) error {
	state, err := s.retry(accountID)
	if err != nil {
		return err
	}
	state.Failures++
	if state.Failures > 5 {
		state.Failures = 5
	}
	state.Attempt, state.After = now, now.Add(15*time.Minute*time.Duration(1<<(state.Failures-1)))
	value, err := json.Marshal(state)
	if err != nil {
		return err
	}
	// Save before contacting the worker: a restart mid-sync cannot cause a
	// tight retry loop, and independent accounts keep independent backoffs.
	return s.database.SetMeta("sync_retry:"+accountID, string(value))
}

func (s *Scheduler) ScheduleStatus(account db.Account, now time.Time) ScheduleStatus {
	s.settingsMu.RLock()
	defer s.settingsMu.RUnlock()
	return s.scheduleStatusLocked(account, now)
}

func (s *Scheduler) scheduleStatusLocked(account db.Account, now time.Time) ScheduleStatus {
	settings := s.settingsLocked(account.ID).Settings
	interval := time.Duration(settings.IntervalMinutes) * time.Minute
	status := ScheduleStatus{Mode: settings.Mode, IntervalMinutes: settings.IntervalMinutes, CloseDelayMinutes: settings.CloseDelayMinutes, Markets: []MarketStatus{}, ServerTimezone: serverTimezone()}
	if settings.Mode == "manual" {
		status.Reason = "Automatic sync paused; manual sync is available"
		return status
	}
	if settings.Mode == "cron" {
		status.Mode, status.Reason = "cron", "Custom schedule"
		status.Cron = settings.Cron
		if parsed, err := cron.ParseStandard(settings.Cron); err == nil {
			location := time.Local
			if spec, ok := parsed.(*cron.SpecSchedule); ok {
				location = spec.Location
			}
			status.Timezone = location.String()
			if location == time.Local {
				status.Timezone = "Server time"
			}
			next := parsed.Next(now)
			// An active @every timer is anchored to its start, not to the status request.
			if _, relative := parsed.(cron.ConstantDelaySchedule); relative && s.started {
				if scheduled := s.cron.Entry(s.entries[settings.Cron]).Next; scheduled.After(now) {
					next = scheduled
				}
			}
			for i := 0; !next.IsZero() && i < 3; next, i = parsed.Next(next), i+1 {
				status.UpcomingRuns = append(status.UpcomingRuns, next.In(location))
			}
			if len(status.UpcomingRuns) > 0 {
				status.NextSync = &status.UpcomingRuns[0]
			}
		}
		return status
	}
	retry, err := s.retry(account.ID)
	if err != nil {
		status.Reason = "Cannot read sync retry state"
		return status
	}
	if !account.Connected && account.LastPull == "" && retry.Failures == 0 {
		status.Reason = "Connect DEGIRO to start automatic sync"
		return status
	}
	last, _ := time.Parse(time.RFC3339, account.LastPull)
	plan, err := s.database.MarketSchedule(account.ID)
	if err != nil {
		log.Printf("scheduler: read calendar: %v", err)
	}
	decision := market.Decision{At: now, Reason: "Initial portfolio sync"}
	if !last.IsZero() {
		// At the default 02:00 daily safety check, skip snapshots less than 10h
		// old. This leaves room within Sure's 36h limit for a 25-hour DST day
		// and sync runtime. An evening close already covers the next 03:00 import.
		maintenance, _ := cron.ParseStandard(settings.Maintenance)
		decision = market.Decision{At: maintenance.Next(last.Add(10*time.Hour - time.Nanosecond)), Reason: "Daily safety refresh"}
		if decision.At.Before(now) {
			decision.At = now
		}
	}
	// Existing installations have no calendar record until their first upgraded
	// sync. Discover once promptly. A saved null means the worker could not supply
	// metadata, so older/mixed-version workers still use the daily fallback.
	if plan == nil && err == nil {
		if saved, readErr := s.database.GetMeta("market_schedule:" + account.ID); readErr == nil && saved == "" {
			at := last.Add(interval)
			if at.Before(now) {
				at = now
			}
			decision = market.Decision{At: at, Reason: "Identify held markets"}
		}
	}
	if plan != nil && now.Before(plan.ValidUntil) {
		status.CalendarAvailable = true
		status.UnknownHoldings = plan.UnknownHoldings
		for _, m := range plan.Markets {
			status.Markets = append(status.Markets, MarketStatus{m.MIC, m.Name, m.Timezone, m.IsOpen(now)})
		}
		if next := plan.NextWithCloseDelay(last, now, interval, time.Duration(settings.CloseDelayMinutes)*time.Minute); !next.At.IsZero() && next.At.Before(decision.At) {
			decision = next
		}
	}
	if retry.Failures > 0 && retry.Attempt.After(last) {
		decision = market.Decision{At: retry.After, Reason: "Retry after sync failure"}
		if decision.At.Before(now) {
			decision.At = now
		}
	}
	status.NextSync, status.Reason = &decision.At, decision.Reason
	return status
}

func (s *Scheduler) triggerDue() {
	s.settingsMu.RLock()
	defer s.settingsMu.RUnlock()
	s.triggerDueLocked()
}

func (s *Scheduler) triggerDueLocked() {
	if s.IsRunning() {
		return
	}
	accounts, err := s.database.Accounts()
	if err != nil {
		log.Printf("scheduler: list accounts: %v", err)
		return
	}
	now := time.Now()
	var ids []string
	for _, account := range accounts {
		if s.settingsLocked(account.ID).Settings.Mode != "market" {
			continue
		}
		status := s.scheduleStatusLocked(account, now)
		if status.NextSync != nil && !status.NextSync.After(now) {
			ids = append(ids, account.ID)
		}
	}
	if len(ids) > 0 {
		if err := s.triggerAccounts(ids, true); err != nil {
			log.Printf("scheduler: %v", err)
		}
	}
}

func (s *Scheduler) historyDue(accountID string, now time.Time, automatic bool) bool {
	key := "history_attempt:" + accountID
	value, err := s.database.GetMeta(key)
	if err != nil {
		log.Printf("scheduler: history marker: %v", err)
		return false
	}
	last, _ := time.Parse(time.RFC3339, value)
	if automatic && now.Sub(last) < 24*time.Hour {
		return false
	}
	if err := s.database.SetMeta(key, now.Format(time.RFC3339)); err != nil {
		log.Printf("scheduler: history marker: %v", err)
		return false
	}
	return true
}
