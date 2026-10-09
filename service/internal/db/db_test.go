package db_test

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/fishman/neocloud-catalog/service/internal/db"
	"github.com/fishman/neocloud-catalog/service/internal/model"
)

// freshDB creates the empty database <NEOCLOUD_CATALOG_TEST_DSN's database>_<suffix>
// and drops it when the test ends. Tests skip without the env (unit runs stay
// DB-free).
func freshDB(t *testing.T, suffix string) (*sql.DB, *gorm.DB) {
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
	// The constraint tests provoke errors on purpose; GORM would log each one.
	return sqlDB, gormDB.Session(&gorm.Session{Logger: logger.Discard})
}

// applyMigrations runs every versioned migration in file order, as `atlas
// migrate apply` would.
func applyMigrations(t *testing.T, sqlDB *sql.DB) {
	t.Helper()
	files, err := filepath.Glob("../../migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no migrations in ../../migrations")
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

// ddlRecorder collects the schema-changing statements GORM issues.
type ddlRecorder struct {
	mu    sync.Mutex
	stmts []string
}

func (r *ddlRecorder) LogMode(logger.LogLevel) logger.Interface { return r }
func (r *ddlRecorder) Info(context.Context, string, ...any)     {}
func (r *ddlRecorder) Warn(context.Context, string, ...any)     {}
func (r *ddlRecorder) Error(context.Context, string, ...any)    {}
func (r *ddlRecorder) Trace(_ context.Context, _ time.Time, fc func() (string, int64), _ error) {
	stmt, _ := fc()
	switch strings.ToUpper(strings.SplitN(strings.TrimSpace(stmt), " ", 2)[0]) {
	case "CREATE", "ALTER", "DROP", "COMMENT":
		r.mu.Lock()
		r.stmts = append(r.stmts, stmt)
		r.mu.Unlock()
	}
}

func TestMigrationsApplyFromScratch(t *testing.T) {
	sqlDB, gormDB := freshDB(t, "scratch")
	applyMigrations(t, sqlDB)

	// AutoMigrate in a rolled-back transaction: any DDL it wants to issue is a
	// drift between the models and the migrations.
	rec := &ddlRecorder{}
	tx := gormDB.Session(&gorm.Session{Logger: rec}).Begin()
	err := tx.AutoMigrate(model.All()...)
	tx.Rollback()
	if err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	if len(rec.stmts) > 0 {
		t.Errorf("models drift from migrations; AutoMigrate wants:\n%s", strings.Join(rec.stmts, "\n"))
	}
}

func pgError(err error) *pgconn.PgError {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr
	}
	return nil
}

func TestNaturalKeyUniquePerLayer(t *testing.T) {
	sqlDB, gormDB := freshDB(t, "natural")
	applyMigrations(t, sqlDB)

	src := model.ModelSource{Provenance: model.Provenance{Layer: model.LayerUpstream}, ModelSlug: "m", Quantization: "q", Type: "huggingface", Repo: "org/m"}
	if err := gormDB.Create(&src).Error; err != nil {
		t.Fatal(err)
	}
	p := func(layer string) model.Provenance { return model.Provenance{Layer: layer, Source: "test"} }
	cases := []struct {
		table string
		row   func(layer string) any
	}{
		{"families", func(l string) any { return &model.Family{Provenance: p(l), Name: "H100"} }},
		{"node_types", func(l string) any { return &model.NodeType{Provenance: p(l), NodeTypeID: "aws/p5.48xlarge"} }},
		{"node_type_prices", func(l string) any {
			return &model.NodeTypePrice{Provenance: p(l), NodeTypeID: "azure/nd96", Region: "eastus", Billing: model.BillingSpot}
		}},
		{"models", func(l string) any { return &model.Model{Provenance: p(l), Slug: "llama"} }},
		{"model_quantizations", func(l string) any {
			return &model.ModelQuantization{Provenance: p(l), ModelSlug: "llama", Name: "bf16"}
		}},
		{"model_sources", func(l string) any {
			return &model.ModelSource{Provenance: p(l), ModelSlug: "llama", Quantization: "bf16", Type: "huggingface", Repo: "meta/llama"}
		}},
		{"model_files", func(l string) any {
			return &model.ModelFile{Provenance: p(l), ModelSourceID: src.ID, Path: "config.json"}
		}},
		{"backends", func(l string) any { return &model.Backend{Provenance: p(l), Name: "vllm"} }},
		{"backend_versions", func(l string) any {
			return &model.BackendVersion{Provenance: p(l), Backend: "vllm", Version: "v0.31.0"}
		}},
		{"flag_tables", func(l string) any {
			return &model.FlagTable{Provenance: p(l), Backend: "vllm", BackendVersion: "v0.31.0"}
		}},
		{"presets", func(l string) any { return &model.Preset{Provenance: p(l), ModelSlug: "llama", Name: "vllm-bf16"} }},
		{"draft_models", func(l string) any { return &model.DraftModel{Provenance: p(l), Slug: "eagle"} }},
	}
	for _, c := range cases {
		t.Run(c.table, func(t *testing.T) {
			if err := gormDB.Create(c.row(model.LayerUpstream)).Error; err != nil {
				t.Fatalf("upstream: %v", err)
			}
			if err := gormDB.Create(c.row(model.LayerLocal)).Error; err != nil {
				t.Fatalf("same key in local: %v", err)
			}
			err := gormDB.Create(c.row(model.LayerUpstream)).Error
			if pe := pgError(err); pe == nil || pe.Code != "23505" || pe.ConstraintName != "idx_"+c.table+"_natural" {
				t.Fatalf("same key twice in upstream: got %v, want unique violation of idx_%s_natural", err, c.table)
			}
		})
	}
}

func TestConstraintsRefuseInvalidRows(t *testing.T) {
	sqlDB, gormDB := freshDB(t, "constraints")
	applyMigrations(t, sqlDB)

	run := model.GeneratorRun{Status: model.RunSucceeded}
	if err := gormDB.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	fit := func() any {
		return &model.Fit{Derivation: model.Derivation{GeneratorRunID: run.ID}, FitKey: "llama/H100/-/bf16/bf16", ModelSlug: "llama"}
	}
	if err := gormDB.Create(fit()).Error; err != nil {
		t.Fatal(err)
	}
	up := model.Provenance{Layer: model.LayerUpstream}
	cases := []struct {
		name, code, constraint string
		row                    any
	}{
		{"unknown layer", "23514", "chk_models_layer", &model.Model{Provenance: model.Provenance{Layer: "staging"}, Slug: "x"}},
		{"extension outside overlay", "23514", "chk_extension_payloads_overlay",
			&model.ExtensionPayload{Provenance: model.Provenance{Layer: model.LayerLocal}, ObjectKind: "models", ObjectKey: "x", Namespace: "neocloud.io/x", Payload: datatypes.JSON(`{}`)}},
		{"unknown extension kind", "23514", "chk_extension_payloads_object_kind",
			&model.ExtensionPayload{Provenance: model.Provenance{Layer: model.LayerOverlay}, ObjectKind: "drafts", ObjectKey: "x", Namespace: "neocloud.io/x", Payload: datatypes.JSON(`{}`)}},
		{"source with two owners", "23514", "chk_model_sources_owner",
			&model.ModelSource{Provenance: up, ModelSlug: "llama", Quantization: "bf16", DraftSlug: "eagle", Type: "huggingface", Repo: "r"}},
		{"source without owner", "23514", "chk_model_sources_owner",
			&model.ModelSource{Provenance: up, Type: "huggingface", Repo: "r"}},
		{"unknown billing", "23514", "chk_node_type_prices_billing",
			&model.NodeTypePrice{Provenance: up, NodeTypeID: "azure/nd96", Region: "eastus", Billing: "hourly"}},
		{"file of a missing source", "23503", "fk_model_files_model_source",
			&model.ModelFile{Provenance: up, ModelSourceID: uuid.New(), Path: "config.json"}},
		{"fit key twice in a run", "23505", "idx_fits_key", fit()},
		{"fit of a missing run", "23503", "fk_fits_generator_run",
			&model.Fit{Derivation: model.Derivation{GeneratorRunID: uuid.New()}, FitKey: "k"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := gormDB.Create(c.row).Error
			if pe := pgError(err); pe == nil || pe.Code != c.code || pe.ConstraintName != c.constraint {
				t.Fatalf("got %v, want %s violation of %s", err, c.code, c.constraint)
			}
		})
	}
}
