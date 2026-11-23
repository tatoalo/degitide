package db

import (
	"errors"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type Transaction struct {
	AccountID    string    `gorm:"primaryKey;default:default" json:"account_id"`
	ID           string    `gorm:"primaryKey" json:"id"`
	Type         string    `json:"type"`
	Date         time.Time `json:"date"`
	ProductID    string    `json:"product_id"`
	Symbol       string    `json:"symbol"`
	Quantity     float64   `json:"quantity"`
	Price        float64   `json:"price"`
	Currency     string    `json:"currency"`
	Fee          float64   `json:"fee"`
	Counterparty string    `json:"counterparty"`
	RawJSON      string    `json:"raw_json,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

type Holding struct {
	AccountID    string    `gorm:"primaryKey;default:default" json:"account_id"`
	SnapshotDate time.Time `gorm:"primaryKey" json:"snapshot_date"`
	ProductID    string    `gorm:"primaryKey" json:"product_id"`
	Symbol       string    `json:"symbol"`
	// Name and ISIN come from DEGIRO product info and identify the fund issuer.
	Name         string  `json:"name"`
	ISIN         string  `gorm:"column:isin" json:"isin"`
	ExchangeMIC  string  `json:"exchange_mic"`
	ExchangeName string  `json:"exchange_name"`
	Quantity     float64 `json:"quantity"`
	AvgPrice     float64 `json:"avg_price"`
	MarketValue  float64 `json:"market_value"`
	Currency     string  `json:"currency"`
	// Currency labels native prices; ValueCurrency labels MarketValue. Older
	// rows have no separate value currency and retain their original semantics.
	ValueCurrency   string   `json:"value_currency"`
	Price           *float64 `json:"price"`
	PriceAsOf       string   `json:"price_as_of"`
	ValuationSource string   `json:"valuation_source"`
}

type Meta struct {
	Key   string `gorm:"primaryKey"`
	Value string
}

type Admin struct {
	ID           uint   `gorm:"primaryKey;autoIncrement:false"`
	Username     string `gorm:"size:64;uniqueIndex;not null"`
	PasswordHash string `gorm:"not null"`
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

var ErrAdminAlreadyExists = errors.New("an administrator already exists")

type DB struct {
	*gorm.DB
}

func New(path string) (*DB, error) {
	if path != ":memory:" {
		dir := filepath.Dir(path)
		if dir != "." {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return nil, err
			}
			if err := os.Chmod(dir, 0o700); err != nil {
				return nil, err
			}
		}
	}
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	gdb, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, err
	}

	if err := migrateAccounts(gdb); err != nil {
		return nil, err
	}
	if err := gdb.AutoMigrate(&Transaction{}, &Holding{}, &Meta{}, &Admin{}, &Account{}, &SimpleFINConnection{}, &PortfolioCurrency{}, &HistoricalPosition{}, &PositionReport{}, &BrokerValuation{}, &HoldingSnapshotCurrency{}, &DayPLObservation{}); err != nil {
		return nil, err
	}
	// Preserve the one reading available on upgrade before the next sync replaces it.
	if err := (&DB{DB: gdb}).backfillDayPLObservations(); err != nil {
		return nil, err
	}
	if err := gdb.Exec("INSERT OR IGNORE INTO portfolio_currencies (account_id, currency) SELECT DISTINCT account_id, COALESCE(NULLIF(value_currency, ''), currency) FROM holdings WHERE COALESCE(NULLIF(value_currency, ''), currency) <> ''").Error; err != nil {
		return nil, err
	}
	created := gdb.Clauses(onConflictDoNothing()).Create(&Account{ID: DefaultAccountID, Name: "Main account"})
	if created.Error != nil {
		return nil, created.Error
	}
	if created.RowsAffected == 1 {
		lastPull, err := (&DB{DB: gdb}).GetMeta("last_pull")
		if err != nil {
			return nil, err
		}
		if lastPull != "" {
			if err := gdb.Model(&Account{}).Where("id = ?", DefaultAccountID).Updates(map[string]any{"last_pull": lastPull, "connected": true}).Error; err != nil {
				return nil, err
			}
		}
	}
	if err := (&DB{DB: gdb}).backfillHoldingSnapshotCurrencies(); err != nil {
		return nil, err
	}
	if path != ":memory:" {
		if err := os.Chmod(path, 0o600); err != nil {
			return nil, err
		}
	}

	if err := (&DB{DB: gdb}).BackfillTransactionMetadata(); err != nil {
		return nil, err
	}
	if err := (&DB{DB: gdb}).RepairHoldingLabels(); err != nil {
		return nil, err
	}
	log.Printf("database ready at %s", path)
	return &DB{DB: gdb}, nil
}

func (d *DB) Close() error {
	sqlDB, err := d.DB.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

func (d *DB) UpsertTransactions(txs []Transaction) error {
	if len(txs) == 0 {
		return nil
	}
	if err := d.DB.Clauses(onConflictDoUpdate()).Create(&txs).Error; err != nil {
		return err
	}
	return d.BackfillTransactionMetadata()
}

func (d *DB) ReplaceHoldings(snapshotDate time.Time, holdings []Holding, accountIDs ...string) error {
	accountID := DefaultAccountID
	if len(accountIDs) > 0 {
		accountID = accountIDs[0]
	}
	for i := range holdings {
		holdings[i].AccountID = accountID
		holdings[i].SnapshotDate = snapshotDate
	}
	return d.DB.Transaction(func(tx *gorm.DB) error {
		for _, holding := range holdings {
			if holding.MarketCurrency() == "" {
				continue
			}
			if err := tx.Clauses(onConflictDoNothing()).Create(&PortfolioCurrency{AccountID: accountID, Currency: holding.MarketCurrency()}).Error; err != nil {
				return err
			}
		}
		if err := saveHoldingSnapshotCurrencies(tx, accountID, snapshotDate, holdings); err != nil {
			return err
		}
		if err := tx.Where("snapshot_date = ? AND account_id = ?", snapshotDate, accountID).Delete(&Holding{}).Error; err != nil {
			return err
		}
		if len(holdings) == 0 {
			return nil
		}
		return tx.Create(&holdings).Error
	})
}

func (d *DB) SetMeta(key, value string) error {
	return d.DB.Save(&Meta{Key: key, Value: value}).Error
}

func (d *DB) GetMeta(key string) (string, error) {
	var m Meta
	if err := d.DB.Where("`key` = ?", key).First(&m).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", nil
		}
		return "", err
	}
	return m.Value, nil
}

func (d *DB) CountTransactions() (int64, error) {
	var count int64
	err := d.DB.Model(&Transaction{}).Count(&count).Error
	return count, err
}

func (d *DB) GetAdmin() (*Admin, error) {
	var admin Admin
	if err := d.DB.Where("id = ?", 1).First(&admin).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &admin, nil
}

func (d *DB) CreateAdmin(username, passwordHash string) (*Admin, error) {
	admin := &Admin{ID: 1, Username: username, PasswordHash: passwordHash}
	result := d.DB.Clauses(onConflictDoNothing()).Create(admin)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected != 1 {
		return nil, ErrAdminAlreadyExists
	}
	return admin, nil
}

// BackfillTransactionMetadata repairs older incomplete imports using the same
// product ID. Currency is filled only when all known trade currencies agree.
func (d *DB) BackfillTransactionMetadata() error {
	return d.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`UPDATE transactions AS old SET symbol = (
   SELECT known.symbol FROM transactions AS known
   WHERE known.account_id = old.account_id AND known.product_id = old.product_id AND known.symbol <> ''
    AND known.symbol <> known.product_id ORDER BY known.date DESC LIMIT 1)
   WHERE old.product_id <> '' AND (old.symbol = '' OR old.symbol = old.product_id)
   AND EXISTS (SELECT 1 FROM transactions AS known WHERE known.account_id = old.account_id AND known.product_id = old.product_id
    AND known.symbol <> '' AND known.symbol <> known.product_id)`).Error; err != nil {
			return err
		}
		return tx.Exec(`UPDATE transactions AS old SET currency = (
   SELECT MAX(known.currency) FROM transactions AS known
   WHERE known.account_id = old.account_id AND known.product_id = old.product_id AND known.currency <> '')
   WHERE old.product_id <> '' AND old.currency = ''
   AND (SELECT COUNT(DISTINCT known.currency) FROM transactions AS known
    WHERE known.account_id = old.account_id AND known.product_id = old.product_id AND known.currency <> '') = 1`).Error
	})
}

// RepairHoldingLabels replaces transport row names only when an unambiguous
// security label is already known for the exact product ID.
func (d *DB) RepairHoldingLabels() error {
	return d.Exec(`UPDATE holdings AS old SET symbol = (
 SELECT MAX(t.symbol) FROM transactions AS t WHERE t.account_id = old.account_id AND t.product_id = old.product_id
 AND t.symbol NOT IN ('', 'positionrow') AND t.symbol <> t.product_id)
 WHERE old.symbol = 'positionrow' AND old.product_id <> ''
 AND (SELECT COUNT(DISTINCT t.symbol) FROM transactions AS t
 WHERE t.account_id = old.account_id AND t.product_id = old.product_id AND t.symbol NOT IN ('', 'positionrow')
 AND t.symbol <> t.product_id) = 1`).Error
}
