package dbmigration

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"runtime"

	"github.com/pressly/goose/v3"
	"gorm.io/gorm"
)

func Up(db *gorm.DB) error {
	sqlDB, err := sqlDBFromGORM(db)
	if err != nil {
		return err
	}
	return UpSQL(sqlDB)
}

func UpSQL(db *sql.DB) error {
	if db == nil {
		return fmt.Errorf("database is required")
	}
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	return goose.Up(db, migrationsDir())
}

func migrationsDir() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "migrations"
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "migrations")
}

func sqlDBFromGORM(db *gorm.DB) (*sql.DB, error) {
	if db == nil {
		return nil, fmt.Errorf("database is required")
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	return sqlDB, nil
}
