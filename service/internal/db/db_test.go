package db_test

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/fishman/neocloud-catalog/service/internal/dbtest"
	"github.com/fishman/neocloud-catalog/service/internal/model"
)

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
	sqlDB, gormDB := dbtest.Fresh(t, "scratch")
	dbtest.Migrate(t, sqlDB)

	// AutoMigrate in a rolled-back transaction: any DDL it wants to issue is a
	// drift between the models and the migrations.
	rec := &ddlRecorder{}
	tx := gormDB.Session(&gorm.Session{Logger: rec}).Begin()
	err := tx.AutoMigrate(model.All()...)
	tx.Rollback()
	if err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	var drift []string
	for _, stmt := range rec.stmts {
		if !resetsCurrentDefault(t, sqlDB, stmt) {
			drift = append(drift, stmt)
		}
	}
	if len(drift) > 0 {
		t.Errorf("models drift from migrations; AutoMigrate wants:\n%s", strings.Join(drift, "\n"))
	}
}

var setDefault = regexp.MustCompile(`^ALTER TABLE "(\w+)" ALTER COLUMN "(\w+)" SET DEFAULT (.+)$`)

// resetsCurrentDefault reports whether stmt re-sets a column's existing default: GORM re-sets
// defaults it cannot parse (jsonb '{}') every run, which is no drift.
func resetsCurrentDefault(t *testing.T, sqlDB *sql.DB, stmt string) bool {
	t.Helper()
	m := setDefault.FindStringSubmatch(stmt)
	if m == nil {
		return false
	}
	var current sql.NullString
	err := sqlDB.QueryRow(`SELECT column_default FROM information_schema.columns WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2`, m[1], m[2]).Scan(&current)
	if err != nil {
		t.Fatalf("default of %s.%s: %v", m[1], m[2], err)
	}
	value, _, _ := strings.Cut(current.String, "::")
	return current.Valid && value == m[3]
}

func pgError(err error) *pgconn.PgError {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr
	}
	return nil
}

func TestNaturalKeyUniquePerLayer(t *testing.T) {
	sqlDB, gormDB := dbtest.Fresh(t, "natural")
	dbtest.Migrate(t, sqlDB)

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
	sqlDB, gormDB := dbtest.Fresh(t, "constraints")
	dbtest.Migrate(t, sqlDB)

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

func TestFieldProvenanceRoundTrip(t *testing.T) {
	sqlDB, gormDB := dbtest.Fresh(t, "fieldprov")
	dbtest.Migrate(t, sqlDB)

	run := uuid.New()
	asOf := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	want := model.FieldProvenance{
		"params_b":    {Source: "huggingface", SourceURL: "https://huggingface.co/meta/llama", AsOf: asOf, ImportRunID: &run},
		"description": {Source: model.SourceCuration, AsOf: asOf},
	}
	curated := model.Model{Provenance: model.Provenance{Layer: model.LayerLocal}, Slug: "llama",
		FieldProvenance: datatypes.NewJSONType(want)}
	bare := model.Model{Provenance: model.Provenance{Layer: model.LayerUpstream}, Slug: "llama"}
	for _, m := range []*model.Model{&curated, &bare} {
		if err := gormDB.Create(m).Error; err != nil {
			t.Fatal(err)
		}
	}

	var got model.Model
	if err := gormDB.First(&got, "id = ?", curated.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.FieldProvenance.Data(), want) {
		t.Errorf("field provenance = %+v, want %+v", got.FieldProvenance.Data(), want)
	}
	var raw string
	if err := sqlDB.QueryRow(`SELECT field_provenance::text FROM models WHERE id = $1`, bare.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw != "{}" {
		t.Errorf("unset field provenance stored as %s, want {}", raw)
	}
}
