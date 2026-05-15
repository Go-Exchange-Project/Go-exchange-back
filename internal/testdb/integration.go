package testdb

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

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

	fatalIfErr(t, db.AutoMigrate(&model.User{}, &model.Order{}, &model.Wallet{}, &model.Trade{}, &model.FailedSettlement{}))
	ApplyConstraints(t, db)
	return db
}

func ApplyConstraints(t testing.TB, db *gorm.DB) {
	t.Helper()

	sqlBytes, err := os.ReadFile(constraintsMigrationPath(t))
	fatalIfErr(t, err)
	fatalIfErr(t, db.Exec(string(sqlBytes)).Error)
}

func constraintsMigrationPath(t testing.TB) string {
	t.Helper()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("failed to locate integration test helper")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "migrations", "001_constraints.sql")
}

func fatalIfErr(t testing.TB, err error) {
	t.Helper()

	if err != nil {
		t.Fatal(err)
	}
}
