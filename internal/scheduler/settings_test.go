package scheduler

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tatoalo/degitide/internal/db"
	"github.com/tatoalo/degitide/internal/market"
)

func TestSettingsPersistAcrossRestartAndOverrideDeployment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.db")
	database, err := db.New(path)
	if err != nil {
		t.Fatal(err)
	}
	s := New(database, nil, "CRON_TZ=Europe/Dublin 0 6 * * *")
	settings := s.Settings().Settings
	settings.Mode, settings.IntervalMinutes, settings.CloseDelayMinutes = "market", 90, 35
	settings.Maintenance = "CRON_TZ=America/New_York 30 1 * * *"
	if err := s.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	s.Stop()
	database.Close()
	database, err = db.New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	restarted := New(database, nil, "CRON_TZ=Europe/Dublin 0 6 * * *")
	defer restarted.Stop()
	if err := restarted.ConfigureMarket(2*time.Hour, "CRON_TZ=UTC 0 4 * * *"); err != nil {
		t.Fatal(err)
	}
	got := restarted.Settings()
	if got.Settings != settings || got.Source != "dashboard" || got.Defaults.IntervalMinutes != 120 {
		t.Fatalf("lost saved settings: %+v", got)
	}
	if err := restarted.ResetSettings(); err != nil {
		t.Fatal(err)
	}
	got = restarted.Settings()
	if got.Source != "environment" || got.Settings != got.Defaults || got.Settings.Mode != "cron" {
		t.Fatalf("did not restore defaults: %+v", got)
	}
	third := New(database, nil, "market")
	defer third.Stop()
	if got := third.Settings(); got.Source != "environment" || got.Settings.IntervalMinutes != 60 {
		t.Fatalf("reset was not persisted: %+v", got)
	}
}

func TestSettingsReplanOpenAndClosingSessions(t *testing.T) {
	s := marketScheduler(t)
	account := db.Account{ID: "default", Connected: true, LastPull: "2026-11-18T10:00:00Z"}
	plan := &market.Schedule{ValidUntil: marketTime("2026-11-30T00:00:00Z"), Markets: []market.Exchange{{MIC: "XETR", Name: "Xetra", Timezone: "Europe/Berlin", Sessions: []market.Session{{Open: marketTime("2026-11-18T08:00:00Z"), Close: marketTime("2026-11-18T16:30:00Z")}}}}}
	if err := s.database.SaveMarketSchedule(account.ID, plan); err != nil {
		t.Fatal(err)
	}
	settings := s.Settings().Settings
	settings.IntervalMinutes, settings.CloseDelayMinutes = 120, 45
	if err := s.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	got := s.ScheduleStatus(account, marketTime("2026-11-18T10:30:00Z"))
	if got.NextSync == nil || !got.NextSync.Equal(marketTime("2026-11-18T12:00:00Z")) {
		t.Fatalf("old cadence: %+v", got)
	}
	account.LastPull = "2026-11-18T16:00:00Z"
	got = s.ScheduleStatus(account, marketTime("2026-11-18T16:35:00Z"))
	if got.NextSync == nil || !got.NextSync.Equal(marketTime("2026-11-18T17:15:00Z")) {
		t.Fatalf("old close delay: %+v", got)
	}
	settings.Mode = "manual"
	if err := s.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"default", "second"} {
		account.ID = id
		got = s.ScheduleStatus(account, time.Now())
		if got.NextSync != nil || got.Mode != "manual" {
			t.Fatalf("pause must apply to every account: %+v", got)
		}
	}
}

func TestSettingsReplaceActiveCronAndInvalidateQueuedJobs(t *testing.T) {
	s := marketScheduler(t)
	settings := s.Settings().Settings
	settings.Mode, settings.Cron = "cron", "@every 1h"
	if err := s.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	s.Start()
	old := s.cron.Entries()
	if len(old) != 1 {
		t.Fatal("expected one custom job", len(old))
	}
	settings.Mode = "manual"
	if err := s.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	if len(s.cron.Entries()) != 0 {
		t.Fatal("paused job still registered")
	}
	// A stale callback must not contact the nil worker or start a sync.
	old[0].Job.Run()
	s.triggerDue()
	if s.IsRunning() {
		t.Fatal("pause started a sync")
	}
	settings.Mode = "market"
	if err := s.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	if len(s.cron.Entries()) != 1 || s.IsRunning() {
		t.Fatal("saving should replan without starting a pull")
	}
	if err := s.ConfigureMarket(time.Hour, "0 2 * * *"); err == nil {
		t.Fatal("deployment changed after Start")
	}
	s.Stop()
	if err := s.SaveSettings(settings); !errors.Is(err, ErrSchedulerStopped) {
		t.Fatal(err)
	}
}

func TestSettingsRejectInvalidAndFailedWritesWithoutChangingPlan(t *testing.T) {
	s := marketScheduler(t)
	before := s.Settings()
	for _, mutate := range []func(*Settings){
		func(c *Settings) { c.Mode = "unknown" }, func(c *Settings) { c.IntervalMinutes = 0 }, func(c *Settings) { c.IntervalMinutes = 241 },
		func(c *Settings) { c.CloseDelayMinutes = -1 }, func(c *Settings) { c.CloseDelayMinutes = 121 },
		func(c *Settings) { c.Maintenance = "CRON_TZ=Nowhere/Nothing 0 2 * * *" },
		func(c *Settings) { c.Maintenance = "0 2 30 2 *" },
		func(c *Settings) { c.Mode = "cron"; c.Cron = "not a cron" },
	} {
		candidate := before.Settings
		mutate(&candidate)
		if err := s.SaveSettings(candidate); !errors.Is(err, ErrInvalidSettings) {
			t.Fatalf("accepted invalid config: %+v (%v)", candidate, err)
		}
		if s.Settings() != before {
			t.Fatal("invalid save mutated settings")
		}
	}
	if err := s.database.Exec(`CREATE TRIGGER deny_settings BEFORE INSERT ON meta BEGIN SELECT RAISE(FAIL, 'test write failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	candidate := before.Settings
	candidate.Mode = "manual"
	if err := s.SaveSettings(candidate); err == nil {
		t.Fatal("expected persistence error")
	}
	if s.Settings() != before {
		t.Fatal("failed persistence mutated settings")
	}
}

func TestSettingsConcurrentReadAndUpdate(t *testing.T) {
	s := marketScheduler(t)
	var readers sync.WaitGroup
	for i := 0; i < 4; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for j := 0; j < 100; j++ {
				s.Settings()
				s.ScheduleStatus(db.Account{ID: "default"}, time.Now())
			}
		}()
	}
	for i := 0; i < 20; i++ {
		c := s.Settings().Settings
		c.IntervalMinutes = 60 + i
		if err := s.SaveSettings(c); err != nil {
			t.Fatal(err)
		}
	}
	readers.Wait()
}
