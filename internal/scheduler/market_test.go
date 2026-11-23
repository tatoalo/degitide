package scheduler

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/tatoalo/degitide/internal/browser"
	"github.com/tatoalo/degitide/internal/db"
	"github.com/tatoalo/degitide/internal/market"
)

func TestStartupSyncsOnlyDueAccountsAndPersistsBackoff(t *testing.T) {
	s := marketScheduler(t)
	now := time.Now()
	if err := s.database.Model(&db.Account{}).Where("id = ?", "default").Updates(map[string]any{"connected": true, "last_pull": now.Format(time.RFC3339)}).Error; err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"due", "retrying"} {
		if err := s.database.Create(&db.Account{ID: id, Name: id, LastPull: now.Add(-48 * time.Hour).Format(time.RFC3339)}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := s.recordAttempt("retrying", now); err != nil {
		t.Fatal(err)
	}
	requests := make(chan string, 4)
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.URL.Query().Get("account")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"valid":false}`))
	}))
	defer worker.Close()
	s.worker = browser.NewClient(worker.URL)
	s.Start()
	select {
	case id := <-requests:
		if id != "due" {
			t.Fatal("synced an account that was not due", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("missed startup catch-up")
	}
	deadline := time.Now().Add(2 * time.Second)
	for s.IsRunning() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if s.IsRunning() {
		t.Fatal("pull did not complete")
	}
	s.triggerDue()
	if s.IsRunning() || len(requests) != 0 {
		t.Fatal("retried failure immediately")
	}
	state, err := s.retry("due")
	if err != nil || state.Failures != 1 || !state.After.After(now) {
		t.Fatalf("retry not persisted: %+v %v", state, err)
	}
}

func marketTime(s string) time.Time {
	at, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return at
}

func marketScheduler(t *testing.T) *Scheduler {
	t.Helper()
	database, err := db.New(filepath.Join(t.TempDir(), "market.db"))
	if err != nil {
		t.Fatal(err)
	}
	s := New(database, nil, "market")
	t.Cleanup(func() { s.Stop(); database.Close() })
	return s
}

func TestMarketScheduleSafetyRefreshAndStartup(t *testing.T) {
	s := marketScheduler(t)
	account := db.Account{ID: "default", Connected: true, LastPull: "2026-11-20T16:50:00Z"}
	plan := &market.Schedule{GeneratedAt: marketTime(account.LastPull), ValidUntil: marketTime("2026-12-10T00:00:00Z"), HoldingCount: 1, Markets: []market.Exchange{{MIC: "XETR", Name: "Xetra", Timezone: "Europe/Berlin", Sessions: []market.Session{
		{Open: marketTime("2026-11-20T08:00:00Z"), Close: marketTime("2026-11-20T16:30:00Z")},
		{Open: marketTime("2026-11-23T08:00:00Z"), Close: marketTime("2026-11-23T16:30:00Z")},
	}}}}
	if err := s.database.SaveMarketSchedule(account.ID, plan); err != nil {
		t.Fatal(err)
	}
	got := s.ScheduleStatus(account, marketTime("2026-11-21T03:00:00Z"))
	if got.NextSync == nil || !got.NextSync.Equal(marketTime("2026-11-22T02:00:00Z")) || got.Reason != "Daily safety refresh" || !got.CalendarAvailable {
		t.Fatal(got)
	}
	// Restart after the safety check catches up once, without losing the plan.
	got = s.ScheduleStatus(account, marketTime("2026-11-22T02:30:00Z"))
	if !got.NextSync.Equal(marketTime("2026-11-22T02:30:00Z")) {
		t.Fatal(got)
	}
	// A normal pre-open boot keeps the useful previous close, avoiding reset day P/L.
	account.LastPull = "2026-11-23T16:50:00Z"
	got = s.ScheduleStatus(account, marketTime("2026-11-24T03:00:00Z"))
	if !got.NextSync.After(marketTime("2026-11-24T03:00:00Z")) {
		t.Fatal(got)
	}
	if got := s.ScheduleStatus(db.Account{ID: "new"}, time.Now()); got.NextSync != nil {
		t.Fatal("unconnected account auto-synced")
	}
}

func TestFallbackDublinDSTAndLegacyCron(t *testing.T) {
	s := marketScheduler(t)
	for _, tc := range []struct{ last, now, want string }{
		{"2026-11-17T06:00:00Z", "2026-11-17T10:00:00Z", "2026-11-18T02:00:00Z"},
		{"2026-08-17T05:00:00Z", "2026-08-17T10:00:00Z", "2026-08-18T01:00:00Z"},
	} {
		if err := s.database.SaveMarketSchedule("default", nil); err != nil {
			t.Fatal(err)
		}
		got := s.ScheduleStatus(db.Account{ID: "default", LastPull: tc.last}, marketTime(tc.now))
		if got.CalendarAvailable || got.NextSync == nil || !got.NextSync.Equal(marketTime(tc.want)) {
			t.Fatal(got)
		}
	}
	settings := s.Settings("").Settings
	settings.Mode, settings.Cron = "cron", "CRON_TZ=Europe/Dublin 0 6 * * *"
	if err := s.SaveSettings("", settings); err != nil {
		t.Fatal(err)
	}
	got := s.ScheduleStatus(db.Account{ID: "default"}, marketTime("2026-08-17T00:00:00Z"))
	if got.Mode != "cron" || !got.NextSync.Equal(marketTime("2026-08-17T05:00:00Z")) {
		t.Fatal(got)
	}
}

func TestUpgradeDiscoversMarketsOnceThenUsesFallbackForOldWorker(t *testing.T) {
	s := marketScheduler(t)
	now := marketTime("2026-11-18T10:00:00Z")
	a := db.Account{ID: "default", Connected: true, LastPull: "2026-11-18T06:00:00Z"}
	first := s.ScheduleStatus(a, now)
	if first.Reason != "Identify held markets" || !first.NextSync.Equal(now) {
		t.Fatal(first)
	}
	if err := s.database.SaveMarketSchedule(a.ID, nil); err != nil {
		t.Fatal(err)
	}
	a.LastPull = now.Format(time.RFC3339)
	later := s.ScheduleStatus(a, now.Add(time.Hour))
	if later.Reason != "Daily safety refresh" || !later.NextSync.After(now.Add(time.Hour)) {
		t.Fatal(later)
	}
}

func TestDefaultHourlyRefreshStillCapturesClose(t *testing.T) {
	s := marketScheduler(t)
	last := marketTime("2026-11-18T10:05:00Z")
	a := db.Account{ID: "default", Connected: true, LastPull: last.Format(time.RFC3339)}
	plan := &market.Schedule{GeneratedAt: last, ValidUntil: last.AddDate(0, 0, 28), HoldingCount: 1, Markets: []market.Exchange{{MIC: "XETR", Name: "Xetra", Timezone: "Europe/Berlin", Sessions: []market.Session{{Open: marketTime("2026-11-18T08:00:00Z"), Close: marketTime("2026-11-18T16:30:00Z")}}}}}
	if err := s.database.SaveMarketSchedule(a.ID, plan); err != nil {
		t.Fatal(err)
	}
	got := s.ScheduleStatus(a, last.Add(20*time.Minute))
	if got.IntervalMinutes != 60 || !got.NextSync.Equal(last.Add(time.Hour)) {
		t.Fatal(got)
	}
	a.LastPull = "2026-11-18T16:05:00Z"
	got = s.ScheduleStatus(a, marketTime("2026-11-18T16:10:00Z"))
	if got.Reason != "After market close" || !got.NextSync.Equal(marketTime("2026-11-18T16:50:00Z")) {
		t.Fatal(got)
	}
}

func TestRetrySurvivesRestartAndDoesNotDelayOtherAccounts(t *testing.T) {
	s := marketScheduler(t)
	now := marketTime("2026-11-18T10:00:00Z")
	a := db.Account{ID: "default", LastPull: "2026-11-17T06:00:00Z", LastError: "worker unavailable"}
	for _, minutes := range []int{15, 30, 60, 120, 240, 240} {
		if err := s.recordAttempt(a.ID, now); err != nil {
			t.Fatal(err)
		}
		restarted := New(s.database, nil, "market")
		got := restarted.ScheduleStatus(a, now)
		restarted.Stop()
		if got.NextSync == nil || !got.NextSync.Equal(now.Add(time.Duration(minutes)*time.Minute)) || got.Reason != "Retry after sync failure" {
			t.Fatal(got)
		}
		if other := s.ScheduleStatus(db.Account{ID: "other", Connected: true}, now); !other.NextSync.Equal(now) {
			t.Fatal("another account was delayed", other)
		}
		now = now.Add(time.Duration(minutes) * time.Minute)
	}
	if err := s.recordAttempt("first-sync-failed", now); err != nil {
		t.Fatal(err)
	}
	got := s.ScheduleStatus(db.Account{ID: "first-sync-failed", LastError: "worker starting"}, now)
	if got.NextSync == nil || got.Reason != "Retry after sync failure" {
		t.Fatal("initial failure was never retried", got)
	}
}

func TestAutomaticHistoryBackfillIsBoundedButManualCanContinue(t *testing.T) {
	s := marketScheduler(t)
	now := time.Now()
	if !s.historyDue("default", now, true) || s.historyDue("default", now.Add(15*time.Minute), true) {
		t.Fatal("intraday sync repeated history work")
	}
	if !s.historyDue("other", now, true) || !s.historyDue("default", now.Add(20*time.Minute), false) || !s.historyDue("default", now.Add(25*time.Hour), true) {
		t.Fatal("history cannot progress")
	}
}

func TestCustomScheduleShowsActualUpcomingRunsAndTimezone(t *testing.T) {
	s := marketScheduler(t)
	settings := s.Settings("").Settings
	settings.Mode = "cron"
	settings.Cron = "CRON_TZ=Europe/Dublin 0 6 * * *"
	if err := s.SaveSettings("", settings); err != nil {
		t.Fatal(err)
	}
	// The spring DST transition changes UTC time, but the configured wall clock stays 06:00.
	status := s.ScheduleStatus(db.Account{ID: "default"}, marketTime("2026-03-28T07:00:00Z"))
	if status.Cron != settings.Cron || status.Timezone != "Europe/Dublin" || len(status.UpcomingRuns) != 3 {
		t.Fatalf("missing schedule detail: %+v", status)
	}
	for i, at := range status.UpcomingRuns {
		if at.Hour() != 6 || at.UTC().Hour() != 5 || (i > 0 && !at.After(status.UpcomingRuns[i-1])) {
			t.Fatalf("incorrect scheduled wall clock: %v", status.UpcomingRuns)
		}
	}
	if !status.NextSync.Equal(status.UpcomingRuns[0]) {
		t.Fatal("next sync differs from preview")
	}
	settings.Cron = "CRON_TZ=America/New_York 0 9-17 * * 1-5"
	if err := s.SaveSettings("", settings); err != nil {
		t.Fatal(err)
	}
	status = s.ScheduleStatus(db.Account{}, marketTime("2026-11-20T22:00:00Z"))
	if status.UpcomingRuns[0].Weekday() != time.Monday || status.UpcomingRuns[0].Hour() != 9 || status.Timezone != "America/New_York" {
		t.Fatalf("weekend not skipped: %+v", status)
	}
}

func TestServerTimezoneNamesTheProcessZone(t *testing.T) {
	for tz, want := range map[string]string{"Europe/Dublin": "Europe/Dublin", ":America/New_York": "America/New_York"} {
		t.Setenv("TZ", tz)
		if got := serverTimezone(); got != want {
			t.Fatalf("TZ=%q: got %q, want %q", tz, got, want)
		}
	}
	t.Setenv("TZ", "Not/AZone")
	if got := serverTimezone(); got != "" {
		if _, err := time.LoadLocation(got); err != nil {
			t.Fatalf("invalid TZ produced an unusable zone %q", got)
		}
	}
}
