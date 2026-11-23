package main

import "testing"

func TestMarketScheduleDefaultsAndOverrides(t *testing.T) {
	t.Setenv("DEGITIDE_SCHEDULE", "")
	t.Setenv("DEGITIDE_SYNC_INTERVAL_MINUTES", "")
	t.Setenv("DEGITIDE_MAINTENANCE_SCHEDULE", "")
	cfg := loadConfig()
	if cfg.Schedule.Daily != "market" || cfg.Schedule.IntervalMinutes != 60 || cfg.Schedule.Maintenance != "CRON_TZ=Europe/Dublin 0 2 * * *" {
		t.Fatal(cfg.Schedule)
	}
	t.Setenv("DEGITIDE_SCHEDULE", "0 8 * * *")
	t.Setenv("DEGITIDE_SYNC_INTERVAL_MINUTES", "30")
	t.Setenv("DEGITIDE_MAINTENANCE_SCHEDULE", "CRON_TZ=Europe/London 0 1 * * *")
	cfg = loadConfig()
	if cfg.Schedule.Daily != "0 8 * * *" || cfg.Schedule.IntervalMinutes != 30 || cfg.Schedule.Maintenance != "CRON_TZ=Europe/London 0 1 * * *" {
		t.Fatal(cfg.Schedule)
	}
}

func TestIsLoopbackBind(t *testing.T) {
	for _, bind := range []string{"127.0.0.1", "::1", "[::1]", "localhost"} {
		if !isLoopbackBind(bind) {
			t.Errorf("expected %q to be loopback", bind)
		}
	}
	for _, bind := range []string{"0.0.0.0", "::", "192.0.2.1", "example.test"} {
		if isLoopbackBind(bind) {
			t.Errorf("expected %q not to be loopback", bind)
		}
	}
}
