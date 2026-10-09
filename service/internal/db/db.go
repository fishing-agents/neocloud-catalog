// Package db opens the pgx *sql.DB and the GORM handle over it. The schema
// comes from the versioned Atlas migrations (migrations/), diffed from the GORM
// models in internal/model; serve never migrates.
package db

import (
	"database/sql"
	"fmt"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" driver
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// Open returns the *sql.DB and the GORM handle built on it; maxConns > 0
// bounds the pool.
func Open(dsn string, maxConns int) (*sql.DB, *gorm.DB, error) {
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("open pgx: %w", err)
	}
	if maxConns > 0 {
		sqlDB.SetMaxOpenConns(maxConns)
	}
	sqlDB.SetConnMaxLifetime(30 * time.Minute)

	gormDB, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{})
	if err != nil {
		sqlDB.Close()
		return nil, nil, fmt.Errorf("open gorm: %w", err)
	}
	return sqlDB, gormDB, nil
}
