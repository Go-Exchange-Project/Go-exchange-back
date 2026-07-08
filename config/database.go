package config

import (
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

var DB *gorm.DB

const (
	EnvDatabaseDSN       = "GOEXCHANGE_DATABASE_DSN"
	EnvDBHost            = "GOEXCHANGE_DB_HOST"
	EnvDBUser            = "GOEXCHANGE_DB_USER"
	EnvDBPassword        = "GOEXCHANGE_DB_PASSWORD"
	EnvDBName            = "GOEXCHANGE_DB_NAME"
	EnvDBPort            = "GOEXCHANGE_DB_PORT"
	EnvDBSSLMode         = "GOEXCHANGE_DB_SSLMODE"
	EnvDBTimeout         = "GOEXCHANGE_DB_CONNECT_TIMEOUT"
	EnvDBMaxOpenConns    = "GOEXCHANGE_DB_MAX_OPEN_CONNS"
	EnvDBMaxIdleConns    = "GOEXCHANGE_DB_MAX_IDLE_CONNS"
	EnvDBConnMaxLifetime = "GOEXCHANGE_DB_CONN_MAX_LIFETIME"
)

const (
	defaultDBMaxOpenConns    = 25
	defaultDBMaxIdleConns    = 25
	defaultDBConnMaxLifetime = 30 * time.Minute
)

type DatabaseDSNConfig struct {
	Host     string
	User     string
	Password string
	Name     string
	Port     string
	SSLMode  string
	Timeout  string
}

func MaxOpenConnsFromEnv() int {
	return parsePositiveIntEnv(EnvDBMaxOpenConns, defaultDBMaxOpenConns)
}

func MaxIdleConnsFromEnv() int {
	return parsePositiveIntEnv(EnvDBMaxIdleConns, defaultDBMaxIdleConns)
}

func ConnMaxLifetimeFromEnv() time.Duration {
	value := strings.TrimSpace(os.Getenv(EnvDBConnMaxLifetime))
	if value == "" {
		return defaultDBConnMaxLifetime
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return defaultDBConnMaxLifetime
	}
	return parsed
}

func parsePositiveIntEnv(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func ConnectDB() {
	dsn := DatabaseDSNFromEnv()

	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		log.Fatal("DB connection failed: ", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		log.Fatal("DB handle retrieval failed: ", err)
	}
	sqlDB.SetMaxOpenConns(MaxOpenConnsFromEnv())
	sqlDB.SetMaxIdleConns(MaxIdleConnsFromEnv())
	sqlDB.SetConnMaxLifetime(ConnMaxLifetimeFromEnv())
	prometheus.MustRegister(collectors.NewDBStatsCollector(sqlDB, "goexchange"))

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
		Timeout:  envOrDefault(EnvDBTimeout, "5"),
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
	if timeout := strings.TrimSpace(cfg.Timeout); timeout != "" {
		parts = append(parts, "connect_timeout="+timeout)
	}

	return strings.Join(parts, " ")
}

func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
