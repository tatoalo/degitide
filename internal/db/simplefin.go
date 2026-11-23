package db

import "time"

// Retain currency identities even if the last position is sold within the same
// daily snapshot. Connectors must still be able to report a zero balance.
type PortfolioCurrency struct {
	AccountID string `gorm:"primaryKey"`
	Currency  string `gorm:"primaryKey"`
}

// SimpleFINConnection grants access to saved portfolio data only. Secrets are
// stored as hashes; neither the setup token nor access password can be recovered.
type SimpleFINConnection struct {
	ID                  string     `gorm:"primaryKey" json:"id"`
	Name                string     `json:"name"`
	BaseURL             string     `json:"base_url"`
	AccountIDs          string     `json:"-"`
	IncludeTransactions bool       `json:"include_transactions"`
	ClaimHash           string     `gorm:"uniqueIndex" json:"-"`
	AccessHash          string     `json:"-"`
	CreatedAt           time.Time  `json:"created_at"`
	ExpiresAt           time.Time  `json:"expires_at"`
	ClaimedAt           *time.Time `json:"claimed_at"`
	LastUsedAt          *time.Time `json:"last_used_at"`
	RevokedAt           *time.Time `json:"revoked_at"`
}
