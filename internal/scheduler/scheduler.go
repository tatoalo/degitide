package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	_ "time/tzdata"

	"github.com/robfig/cron/v3"
	"github.com/tatoalo/degitide/internal/browser"
	"github.com/tatoalo/degitide/internal/db"
	"gorm.io/gorm"
)

type Scheduler struct {
	cron          *cron.Cron
	database      *db.DB
	worker        *browser.Client
	running       atomic.Bool
	lastErr       atomic.Value
	ctx           context.Context
	cancel        context.CancelFunc
	stateMu       sync.Mutex
	stopped       bool
	runs          sync.WaitGroup
	progressMu    sync.RWMutex
	progress      Progress
	OnResult      func(db.Account, error)
	settingsMu    sync.RWMutex
	settings      Settings
	defaults      Settings
	savedSettings bool
	started       bool
	entry         cron.EntryID
	generation    uint64
}

var ErrPullInProgress = errors.New("a pull is already in progress")
var ErrSchedulerStopped = errors.New("scheduler is stopped")

func New(database *db.DB, worker *browser.Client, schedule string) *Scheduler {
	ctx, cancel := context.WithCancel(context.Background())
	scheduler := &Scheduler{
		cron:     cron.New(),
		database: database,
		worker:   worker,
		ctx:      ctx,
		cancel:   cancel,
	}
	scheduler.defaults = Settings{Mode: "market", IntervalMinutes: 60, CloseDelayMinutes: 20, Maintenance: "CRON_TZ=Europe/Dublin 0 2 * * *"}
	if schedule != "" && schedule != "market" {
		scheduler.defaults.Mode, scheduler.defaults.Cron = "cron", schedule
	}
	scheduler.lastErr.Store("")
	_ = scheduler.ConfigureMarket(time.Hour, "CRON_TZ=Europe/Dublin 0 2 * * *")
	return scheduler
}

func (s *Scheduler) Start() {
	s.settingsMu.Lock()
	if s.started {
		s.settingsMu.Unlock()
		return
	}
	s.started = true
	s.installScheduleLocked()
	s.cron.Start()
	s.settingsMu.Unlock()
	s.triggerDue() // Catch up overdue market sessions, not an unconditional boot pull.
	log.Printf("scheduler started")
}

func (s *Scheduler) Stop() {
	s.stateMu.Lock()
	if s.stopped {
		s.stateMu.Unlock()
		return
	}
	s.stopped = true
	s.cancel()
	s.stateMu.Unlock()

	<-s.cron.Stop().Done()
	s.runs.Wait()
}

// transactionHistoryStart predates every DEGIRO account. The transaction report
// accepts it and returns the whole history in a single response.
var transactionHistoryStart = time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)

// transactionOverlapDays is how far each incremental pull reaches back before
// the previous successful pull. Transactions upsert by stable ID, so the
// overlap is idempotent.
const transactionOverlapDays = 7

func transactionBackfillKey(accountID string) string {
	return "transactions_backfilled:" + accountID
}

// Keep the key so existing performance history stays available, but only a
// v2 marker proves that the worker replayed and validated the complete report.
// Older pulls could silently substitute the page's shorter date window.
func transactionBackfillValue(completed time.Time) string {
	return "v2:" + completed.Format(time.RFC3339)
}

func verifiedTransactionBackfill(value string) bool {
	stamp, ok := strings.CutPrefix(value, "v2:")
	if !ok {
		return false
	}
	_, err := time.Parse(time.RFC3339, stamp)
	return err == nil
}

// transactionWindowStart returns the first day of the transaction window and
// whether it is a full-history backfill. An account's first pull requests its
// whole history, and so does its first pull after upgrading from an unverified
// import. Later pulls resume a week before the last successful pull,
// so a sync outage longer than a week no longer leaves a permanent gap.
func (s *Scheduler) transactionWindowStart(account db.Account, now time.Time) (time.Time, bool, error) {
	done, err := s.database.GetMeta(transactionBackfillKey(account.ID))
	if err != nil {
		return time.Time{}, false, err
	}
	if !verifiedTransactionBackfill(done) {
		return transactionHistoryStart, true, nil
	}
	start := now.AddDate(0, 0, -transactionOverlapDays)
	if last, err := time.Parse(time.RFC3339, account.LastPull); err == nil {
		if resume := last.AddDate(0, 0, -transactionOverlapDays); resume.Before(start) {
			start = resume
		}
	}
	return start, false, nil
}

// Each refresh keeps transactions, holdings and broker totals consistent.
func (s *Scheduler) runAccount(account db.Account, index int, automatic bool) error {
	worker := s.worker.ForAccount(account.ID)
	s.stage(index, 0, "Checking connection")
	log.Println("scheduler: starting portfolio sync")

	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Minute)
	defer cancel()

	// Check session validity
	session, err := worker.CheckSession(ctx)
	if err != nil {
		return fmt.Errorf("session check: %w", err)
	}
	if !session.Valid {
		return errors.New("DEGIRO session expired; login required")
	}

	s.stage(index, 1, "Fetching transactions")
	windowEnd := time.Now()
	windowStart, backfill, err := s.transactionWindowStart(account, windowEnd)
	if err != nil {
		return fmt.Errorf("transaction window: %w", err)
	}
	if backfill {
		log.Printf("scheduler: backfilling the full transaction history for account %s", account.ID)
	}
	from := windowStart.Format("2006-01-02")
	to := windowEnd.Format("2006-01-02")

	transResp, err := worker.FetchTransactions(ctx, from, to)
	if err != nil {
		return fmt.Errorf("fetch transactions: %w", err)
	}

	txs := make([]db.Transaction, 0, len(transResp.Transactions))
	for _, t := range transResp.Transactions {
		var transactionDate time.Time
		if parsed, err := time.Parse("2006-01-02", t.Date); err == nil {
			transactionDate = parsed
		} else if parsed, err := time.Parse(time.RFC3339, t.Date); err == nil {
			transactionDate = parsed
		} else {
			return fmt.Errorf("transaction %q has invalid date %q", t.ID, t.Date)
		}
		txs = append(txs, db.Transaction{
			AccountID:    account.ID,
			ID:           t.ID,
			Type:         t.Type,
			Date:         transactionDate,
			ProductID:    t.ProductID,
			Symbol:       t.Symbol,
			Quantity:     t.Quantity,
			Price:        t.Price,
			Currency:     t.Currency,
			Fee:          t.Fee,
			Counterparty: t.Counterparty,
			RawJSON:      t.RawJSON,
			CreatedAt:    time.Now(),
		})
	}

	s.stage(index, 2, "Fetching holdings")
	// Fetch holdings snapshot
	holdResp, err := worker.FetchHoldings(ctx)
	if err != nil {
		return fmt.Errorf("fetch holdings: %w", err)
	}

	if holdResp.Holdings == nil {
		return errors.New("worker returned no complete holdings array")
	}
	var valuation *db.BrokerValuation
	if v := holdResp.Valuation; v != nil {
		fetchedAt, err := time.Parse(time.RFC3339Nano, v.FetchedAt)
		if err != nil {
			return errors.New("worker returned an invalid valuation timestamp")
		}
		valuation = &db.BrokerValuation{
			BaseCurrency: v.BaseCurrency, PortfolioValue: v.PortfolioValue,
			CashBalance: v.CashBalance, AccountValue: v.AccountValue,
			TotalPL: v.TotalPL, DayPL: v.DayPL, AvailableToTrade: v.AvailableToTrade,
			FetchedAt: fetchedAt, Source: v.Source, PriceStatus: v.PriceStatus,
			ReconciliationDifference: v.ReconciliationDifference, Warning: v.Warning,
		}
		if err := valuation.Validate(); err != nil {
			return err
		}
	}
	now := time.Now()
	plan := holdResp.MarketSchedule
	if plan != nil {
		if err := plan.Validate(now, len(holdResp.Holdings)); err != nil {
			log.Printf("scheduler: %v; using daily fallback", err)
			plan = nil
		}
	}
	snapshotDate := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	holdings := make([]db.Holding, 0, len(holdResp.Holdings))
	seenHoldings := map[string]bool{}
	for _, h := range holdResp.Holdings {
		if h.ProductID == "" || seenHoldings[h.ProductID] || h.Quantity == 0 {
			return errors.New("worker returned an invalid or duplicate holding")
		}
		seenHoldings[h.ProductID] = true
		for _, value := range []float64{h.Quantity, h.AvgPrice, h.MarketValue} {
			if math.IsNaN(value) || math.IsInf(value, 0) {
				return errors.New("worker returned a non-finite holding")
			}
		}
		holdings = append(holdings, db.Holding{
			AccountID:    account.ID,
			SnapshotDate: snapshotDate,
			ProductID:    h.ProductID,
			Symbol:       h.Symbol,
			Name:         h.Name,
			ISIN:         h.ISIN,
			ExchangeMIC:  h.ExchangeMIC, ExchangeName: h.ExchangeName,
			Quantity:      h.Quantity,
			AvgPrice:      h.AvgPrice,
			MarketValue:   h.MarketValue,
			Currency:      h.Currency,
			ValueCurrency: h.ValueCurrency, Price: h.Price,
			PriceAsOf: h.PriceAsOf, ValuationSource: h.ValuationSource,
		})
	}
	s.stage(index, 3, "Saving snapshot")
	// Publish one consistent account snapshot to both dashboard and connectors.
	// A failed holdings fetch or database write must not expose half a pull.
	if err := s.database.DB.Transaction(func(tx *gorm.DB) error {
		saved := &db.DB{DB: tx}
		if err := saved.RepairTransactionIdentities(account.ID); err != nil {
			return err
		}
		if err := saved.UpsertTransactions(txs); err != nil {
			return fmt.Errorf("upsert transactions: %w", err)
		}
		if backfill {
			if err := saved.SetMeta(transactionBackfillKey(account.ID), transactionBackfillValue(now)); err != nil {
				return fmt.Errorf("record transaction backfill: %w", err)
			}
		}
		if err := saved.ReplaceHoldings(snapshotDate, holdings, account.ID); err != nil {
			return fmt.Errorf("replace holdings: %w", err)
		}
		if err := saved.SaveBrokerValuation(account.ID, valuation); err != nil {
			return err
		}
		// Clear an obsolete plan when metadata is missing; never keep scheduling
		// venues from a portfolio that has since been sold or replaced.
		if err := saved.SaveMarketSchedule(account.ID, plan); err != nil {
			return err
		}
		if err := saved.SetMeta("last_pull", now.Format(time.RFC3339)); err != nil {
			return err
		}
		return saved.Model(&db.Account{}).Where("id = ?", account.ID).Updates(map[string]any{"connected": true, "last_pull": now.Format(time.RFC3339), "last_error": "", "snapshot_date": snapshotDate}).Error
	}); err != nil {
		return err
	}
	log.Printf("scheduler: saved %d transactions and %d holdings", len(txs), len(holdings))
	if s.historyDue(account.ID, now, automatic) {
		s.stage(index, 3, "Updating value history")
		s.backfillValueHistory(account, worker)
	}
	log.Println("scheduler: portfolio sync complete")
	return nil
}

// valueHistoryBatch bounds the position reports one sync requests. At about
// half a second per report, five years of weekly history fill in over three
// syncs without a long burst against DEGIRO.
const valueHistoryBatch = 120

// valueHistoryDates lists the weekly (Friday) dates from the account's first
// transaction up to yesterday that have neither a fetched report nor a synced
// snapshot. Newest first, so recent history appears before old history.
func valueHistoryDates(first, now time.Time, covered map[string]bool) []string {
	day := db.Day(first)
	for day.Weekday() != time.Friday {
		day = day.AddDate(0, 0, 1)
	}
	today := db.Day(now)
	var dates []string
	for ; day.Before(today); day = day.AddDate(0, 0, 7) {
		if key := day.Format("2006-01-02"); !covered[key] {
			dates = append(dates, key)
		}
	}
	for i, j := 0, len(dates)-1; i < j; i, j = i+1, j-1 {
		dates[i], dates[j] = dates[j], dates[i]
	}
	return dates
}

// backfillValueHistory rebuilds past portfolio value from DEGIRO position
// reports. It is best effort: a failure is logged and retried on the next sync,
// and never fails the pull that already saved its snapshot.
func (s *Scheduler) backfillValueHistory(account db.Account, worker *browser.Client) {
	first, ok, err := s.database.FirstTransactionDate(account.ID)
	if err != nil || !ok {
		if err != nil {
			log.Printf("scheduler: value history for account %s: %v", account.ID, err)
		}
		return
	}
	covered, err := s.database.ValueHistoryCoverage(account.ID)
	if err != nil {
		log.Printf("scheduler: value history for account %s: %v", account.ID, err)
		return
	}
	missing := valueHistoryDates(first, time.Now(), covered)
	if len(missing) == 0 {
		return
	}
	dates := missing
	if len(dates) > valueHistoryBatch {
		dates = dates[:valueHistoryBatch]
	}
	ctx, cancel := context.WithTimeout(s.ctx, 4*time.Minute)
	defer cancel()
	resp, err := worker.FetchPositions(ctx, dates)
	if err != nil {
		log.Printf("scheduler: value history for account %s: %v", account.ID, err)
		return
	}
	requested := map[string]bool{}
	for _, day := range dates {
		requested[day] = true
	}
	results := make([]db.PositionReportResult, 0, len(resp.Snapshots))
	for _, snapshot := range resp.Snapshots {
		day, err := time.Parse("2006-01-02", snapshot.Date)
		if err != nil || !requested[snapshot.Date] {
			continue
		}
		positions := make([]db.HistoricalPosition, 0, len(snapshot.Positions))
		for _, p := range snapshot.Positions {
			positions = append(positions, db.HistoricalPosition{
				Identifier: p.Identifier, Name: p.Name, Quantity: p.Quantity,
				Close: p.Close, Currency: p.Currency, Value: p.Value, BaseCurrency: snapshot.BaseCurrency, BaseValue: p.BaseValue,
			})
		}
		results = append(results, db.PositionReportResult{Date: day, Available: true, BaseCurrency: snapshot.BaseCurrency, Positions: positions})
	}
	// Unavailable dates are not recorded: every requested date follows the first
	// trade, so a refusal there is transient and the next sync retries it.
	if err := s.database.SavePositionReports(account.ID, results, time.Now()); err != nil {
		log.Printf("scheduler: value history for account %s: %v", account.ID, err)
		return
	}
	log.Printf("scheduler: value history for account %s: saved %d weekly reports, %d unavailable, %d still missing",
		account.ID, len(results), len(resp.Unavailable), len(missing)-len(results))
}

// TriggerNow syncs all configured accounts, including scheduled headless runs.
func (s *Scheduler) TriggerNow() error { return s.TriggerAccounts(nil) }

func (s *Scheduler) TriggerAccounts(ids []string) error {
	return s.triggerAccounts(ids, false)
}

func (s *Scheduler) triggerAccounts(ids []string, automatic bool) error {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.stopped {
		return ErrSchedulerStopped
	}
	if s.running.Load() {
		return ErrPullInProgress
	}
	accounts, err := s.database.Accounts()
	if err != nil {
		return err
	}
	if len(ids) > 0 {
		selected := []db.Account{}
		for _, id := range ids {
			found := false
			for _, account := range accounts {
				if account.ID == id {
					selected = append(selected, account)
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("unknown account %q", id)
			}
		}
		accounts = selected
	}
	if len(accounts) == 0 {
		return errors.New("no accounts configured")
	}
	s.running.Store(true)
	s.lastErr.Store("")
	progress := Progress{Running: true, StartedAt: time.Now(), Stage: "Starting sync", Accounts: []AccountProgress{}}
	for _, account := range accounts {
		progress.Accounts = append(progress.Accounts, AccountProgress{ID: account.ID, Name: account.Name, State: "queued"})
	}
	s.progressMu.Lock()
	s.progress = progress
	s.progressMu.Unlock()
	s.runs.Add(1)
	go func() {
		defer s.runs.Done()
		defer s.running.Store(false)
		failures := []string{}
		interrupted := false
		for index, account := range accounts {
			err := s.recordAttempt(account.ID, time.Now())
			if err == nil {
				err = s.runAccount(account, index, automatic)
			}
			if s.ctx.Err() != nil {
				// An operator stopping the server is not an account failure.
				interrupted = true
				s.progressMu.Lock()
				for pending := index; pending < len(accounts); pending++ {
					s.progress.Accounts[pending].State = "cancelled"
				}
				s.progressMu.Unlock()
				break
			}
			s.progressMu.Lock()
			result := &s.progress.Accounts[index]
			result.State = "complete"
			result.Completed = 4
			if err != nil {
				result.State = "failed"
				result.Error = err.Error()
				failures = append(failures, account.Name+": "+err.Error())
			}
			s.progress.Percent = (index + 1) * 100 / len(accounts)
			s.progressMu.Unlock()
			if err != nil {
				// Connection checks happen on the next sync, never because a UI is open.
				_ = s.database.Model(&db.Account{}).Where("id = ?", account.ID).Updates(map[string]any{"last_error": err.Error(), "connected": false}).Error
				log.Printf("scheduler: account %s failed: %v", account.ID, err)
			} else {
				if resetErr := s.database.SetMeta("sync_retry:"+account.ID, ""); resetErr != nil {
					log.Printf("scheduler: reset retry: %v", resetErr)
				}
			}
			if s.OnResult != nil {
				s.OnResult(account, err)
			}
		}
		s.lastErr.Store(strings.Join(failures, "; "))
		s.progressMu.Lock()
		s.progress.Running = false
		s.progress.FinishedAt = time.Now()
		s.progress.Stage = "Sync complete"
		if len(failures) > 0 {
			s.progress.Stage = "Sync finished with errors"
		}
		if interrupted {
			s.progress.Stage = "Sync stopped"
		}
		s.progressMu.Unlock()
	}()
	return nil
}

func (s *Scheduler) IsRunning() bool {
	return s.running.Load()
}

func (s *Scheduler) LastError() string {
	value, _ := s.lastErr.Load().(string)
	return value
}
