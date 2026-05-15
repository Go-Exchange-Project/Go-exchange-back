package config

import (
	"log"
	"os"
	"strings"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

var DB *gorm.DB

const (
	EnvDatabaseDSN = "GOEXCHANGE_DATABASE_DSN"
	EnvDBHost      = "GOEXCHANGE_DB_HOST"
	EnvDBUser      = "GOEXCHANGE_DB_USER"
	EnvDBPassword  = "GOEXCHANGE_DB_PASSWORD"
	EnvDBName      = "GOEXCHANGE_DB_NAME"
	EnvDBPort      = "GOEXCHANGE_DB_PORT"
	EnvDBSSLMode   = "GOEXCHANGE_DB_SSLMODE"
)

type DatabaseDSNConfig struct {
	Host     string
	User     string
	Password string
	Name     string
	Port     string
	SSLMode  string
}

func ConnectDB() {
	dsn := DatabaseDSNFromEnv()

	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		log.Fatal("DB connection failed: ", err)
	}

	DB = db
	log.Println("DB connection established")
}

func DatabaseDSNFromEnv() string {
	if dsn := strings.TrimSpace(os.Getenv(EnvDatabaseDSN)); dsn != "" {
		return dsn
	}

	return BuildDatabaseDSN(DatabaseDSNConfig{
		Host:     envOrDefault(EnvDBHost, "localhost"),
		User:     envOrDefault(EnvDBUser, "postgres"),
		Password: strings.TrimSpace(os.Getenv(EnvDBPassword)),
		Name:     envOrDefault(EnvDBName, "goexchange"),
		Port:     envOrDefault(EnvDBPort, "5432"),
		SSLMode:  envOrDefault(EnvDBSSLMode, "disable"),
	})
}

func BuildDatabaseDSN(cfg DatabaseDSNConfig) string {
	parts := []string{
		"host=" + strings.TrimSpace(cfg.Host),
		"user=" + strings.TrimSpace(cfg.User),
	}

	if password := strings.TrimSpace(cfg.Password); password != "" {
		parts = append(parts, "password="+password)
	}

	parts = append(parts,
		"dbname="+strings.TrimSpace(cfg.Name),
		"port="+strings.TrimSpace(cfg.Port),
		"sslmode="+strings.TrimSpace(cfg.SSLMode),
	)

	return strings.Join(parts, " ")
}

func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
