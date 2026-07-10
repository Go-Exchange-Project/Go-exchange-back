package testdb

import (
	"os"
	"testing"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/dbmigration"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func OpenIntegrationDB(t testing.TB) *gorm.DB {
	t.Helper()

	dsn := os.Getenv("GOEXCHANGE_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("GOEXCHANGE_TEST_DATABASE_DSN is not set; skipping Postgres integration test")
	}

	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	fatalIfErr(t, err)

	sqlDB, err := db.DB()
	fatalIfErr(t, err)
	fatalIfErr(t, sqlDB.Ping())
	t.Cleanup(func() {
		fatalIfErr(t, sqlDB.Close())
	})

	fatalIfErr(t, db.AutoMigrate(&model.User{}, &model.Order{}, &model.Wallet{}, &model.Trade{}, &model.FailedSettlement{}, &model.FailedMarketCompletion{}, &model.LedgerEntry{}, &model.ReconciliationViolation{}))
	fatalIfErr(t, dbmigration.Up(db))
	return db
}

func fatalIfErr(t testing.TB, err error) {
	t.Helper()

	if err != nil {
		t.Fatal(err)
	}
}
