package db

import "gorm.io/gorm/clause"

func onConflictDoUpdate() clause.OnConflict {
	return clause.OnConflict{
		Columns: []clause.Column{{Name: "account_id"}, {Name: "id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"type", "date", "product_id", "symbol", "quantity", "price",
			"currency", "fee", "counterparty", "raw_json",
		}),
	}
}

func onConflictDoNothing() clause.OnConflict {
	return clause.OnConflict{DoNothing: true}
}
