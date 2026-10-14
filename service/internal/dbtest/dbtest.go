// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

// Package dbtest gives integration tests their own database on NEOCLOUD_CATALOG_TEST_DSN; tests
// skip without it.
package dbtest

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/fishman/neocloud-catalog/service/internal/db"
	"github.com/fishman/neocloud-catalog/service/internal/store"
)

// Provenance is the effective provenance of kind/key, local row first; the test fails without any.
func Provenance(t testing.TB, s *store.Store, kind, key string) []store.ProvenanceEntry {
	t.Helper()
	_, all, err := s.EffectiveProvenance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p := all[kind][key]
	if len(p) == 0 {
		t.Fatalf("no provenance of %s %q", kind, key)
	}
	return p
}

// Fresh creates the empty database <DSN database>_<suffix>, dropped when the test ends. Its GORM
// handle logs nothing: tests provoke errors on purpose.
func Fresh(t testing.TB, suffix string) (*sql.DB, *gorm.DB) {
	t.Helper()
	dsn := os.Getenv("NEOCLOUD_CATALOG_TEST_DSN")
	if dsn == "" {
		t.Skip("NEOCLOUD_CATALOG_TEST_DSN unset; skipping integration")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := strings.TrimPrefix(u.Path, "/") + "_" + suffix
	admin := *u
	admin.Path = "/postgres"
	adminDB, err := sql.Open("pgx", admin.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { adminDB.Close() })
	for _, stmt := range []string{`DROP DATABASE IF EXISTS "` + name + `" WITH (FORCE)`, `CREATE DATABASE "` + name + `"`} {
		if _, err := adminDB.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	u.Path = "/" + name
	sqlDB, gormDB, err := db.Open(u.String(), 4)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		sqlDB.Close()
		if _, err := adminDB.Exec(`DROP DATABASE IF EXISTS "` + name + `" WITH (FORCE)`); err != nil {
			t.Errorf("drop %s: %v", name, err)
		}
	})
	return sqlDB, gormDB.Session(&gorm.Session{Logger: logger.Discard})
}

// Migrate applies every versioned migration in file order, as `atlas migrate apply` would.
func Migrate(t testing.TB, sqlDB *sql.DB) {
	t.Helper()
	_, self, _, _ := runtime.Caller(0)
	files, err := filepath.Glob(filepath.Join(filepath.Dir(self), "../../migrations/*.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no migrations in service/migrations")
	}
	slices.Sort(files)
	for _, f := range files {
		stmts, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := sqlDB.Exec(string(stmts)); err != nil {
			t.Fatalf("apply %s: %v", filepath.Base(f), err)
		}
	}
}
