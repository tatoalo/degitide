package db

import (
	"regexp"
	"time"

	"gorm.io/gorm"
)

const DefaultAccountID = "default"

var accountIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

func ValidAccountID(id string) bool { return accountIDPattern.MatchString(id) }

// Broker accounts share one administrator, but never share sessions or records.
type Account struct {
	ID           string     `gorm:"primaryKey" json:"id"`
	Name         string     `json:"name"`
	Connected    bool       `json:"connected"`
	LastPull     string     `json:"last_pull"`
	LastError    string     `json:"last_error"`
	SnapshotDate *time.Time `json:"snapshot_date"`
	CreatedAt    time.Time  `json:"created_at"`
}

func (d *DB) Accounts() ([]Account, error) {
	accounts := []Account{}
	err := d.Order("created_at ASC, id ASC").Find(&accounts).Error
	return accounts, err
}

// SQLite cannot ALTER a primary key. Rebuild both tables atomically, preserving
// the existing installation as the default account. The source remains intact
// if any part of the migration fails.
func migrateAccounts(database *gorm.DB) error {
	return database.Transaction(func(tx *gorm.DB) error {
		for _, table := range []struct {
			name, columns string
			model         any
		}{
			{"transactions", "id,type,date,product_id,symbol,quantity,price,currency,fee,counterparty,raw_json,created_at", &Transaction{}},
			{"holdings", "snapshot_date,product_id,symbol,quantity,avg_price,market_value,currency", &Holding{}},
		} {
			if !tx.Migrator().HasTable(table.name) || tx.Migrator().HasColumn(table.name, "account_id") {
				continue
			}
			legacy := table.name + "_before_accounts"
			if err := tx.Migrator().RenameTable(table.name, legacy); err != nil {
				return err
			}
			if err := tx.Migrator().CreateTable(table.model); err != nil {
				return err
			}
			if err := tx.Exec("INSERT INTO " + table.name + " (account_id," + table.columns + ") SELECT 'default'," + table.columns + " FROM " + legacy).Error; err != nil {
				return err
			}
			if err := tx.Migrator().DropTable(legacy); err != nil {
				return err
			}
		}
		return nil
	})
}
