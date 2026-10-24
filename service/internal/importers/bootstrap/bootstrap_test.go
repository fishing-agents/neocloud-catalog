// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

package bootstrap_test

import (
	"context"
	"encoding/json"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/fishman/neocloud-catalog/service/internal/dbtest"
	"github.com/fishman/neocloud-catalog/service/internal/importers"
	"github.com/fishman/neocloud-catalog/service/internal/importers/bootstrap"
	"github.com/fishman/neocloud-catalog/service/internal/model"
	"github.com/fishman/neocloud-catalog/service/internal/store"
)

const dataDir = "../../../../data"

var ctx = context.Background()

func newStore(t *testing.T, suffix string) (*store.Store, *gorm.DB) {
	t.Helper()
	sqlDB, gormDB := dbtest.Fresh(t, suffix)
	dbtest.Migrate(t, sqlDB)
	return store.New(gormDB), gormDB
}

func curatedRows(t *testing.T, db *gorm.DB) map[uuid.UUID]time.Time {
	t.Helper()
	rows := map[uuid.UUID]time.Time{}
	for _, table := range []string{"families", "node_types", "models", "model_quantizations", "model_sources", "model_files",
		"presets", "draft_models", "backends", "backend_versions", "flag_tables"} {
		var rs []struct {
			ID        uuid.UUID
			UpdatedAt time.Time
		}
		if err := db.Table(table).Select("id, updated_at").Find(&rs).Error; err != nil {
			t.Fatal(err)
		}
		for _, r := range rs {
			rows[r.ID] = r.UpdatedAt
		}
	}
	return rows
}

func summaryOf(t *testing.T, run model.ImportRun) importers.Summary {
	t.Helper()
	var s importers.Summary
	if err := json.Unmarshal(run.Summary, &s); err != nil {
		t.Fatalf("summary %s: %v", run.Summary, err)
	}
	return s
}

// A second run changes no row and says so in its summary.
func TestImporterIdempotent(t *testing.T) {
	s, db := newStore(t, "bootstrap_idem")
	imp, err := bootstrap.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	in, version, err := bootstrap.Load(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		t.Fatal(err)
	}

	first, err := importers.Execute(ctx, s, imp, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	wantURL := (&url.URL{Scheme: "file", Path: abs}).String()
	if first.Importer != "bootstrap-from-json" || first.Layer != model.LayerUpstream || first.Source != "bootstrap-from-json" ||
		first.SourceURL != wantURL || first.Status != model.RunSucceeded || first.FinishedAt == nil || first.Error != "" {
		t.Errorf("first run = %+v, want a succeeded bootstrap-from-json run of the upstream layer from %s", first, wantURL)
	}
	if !strings.HasPrefix(version, "sha256:") || first.InputVersion != version {
		t.Errorf("input version = %q, want the files' %q", first.InputVersion, version)
	}
	changes := summaryOf(t, first).Changes
	if c := changes["families"]; c != (store.Change{Inserted: len(in.Families)}) {
		t.Errorf("first run families change = %+v, want %d inserted", c, len(in.Families))
	}
	if c := changes["models"]; c != (store.Change{Inserted: len(in.Models)}) {
		t.Errorf("first run models change = %+v, want %d inserted", c, len(in.Models))
	}
	got, err := s.Effective(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Models, in.Models) || !reflect.DeepEqual(got.Families, in.Families) {
		t.Error("effective catalog differs from the files")
	}

	before := curatedRows(t, db)
	second, err := importers.Execute(ctx, s, imp, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if second.ID == first.ID || second.Status != model.RunSucceeded || second.InputVersion != version {
		t.Errorf("second run = %+v, want another succeeded run of input %s", second, version)
	}
	if c := summaryOf(t, second).Changes; len(c) != 0 {
		t.Errorf("second run changed %+v, want nothing", c)
	}
	if after := curatedRows(t, db); !reflect.DeepEqual(after, before) {
		t.Errorf("second run changed rows: %d rows before, %d after", len(before), len(after))
	}
	var runs int64
	if err := db.Model(&model.ImportRun{}).Where("importer = ?", "bootstrap-from-json").Count(&runs).Error; err != nil {
		t.Fatal(err)
	}
	if runs != 2 {
		t.Errorf("%d bootstrap runs recorded, want 2", runs)
	}
}

// A curated family value survives a differing bootstrap and is listed as a
// conflict.
func TestKeptCurationIsListed(t *testing.T) {
	s, _ := newStore(t, "bootstrap_curated")
	imp, err := bootstrap.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := importers.Execute(ctx, s, imp, uuid.New()); err != nil {
		t.Fatal(err)
	}
	epoch, err := s.StartImport(ctx, uuid.New(), "epoch", model.LayerUpstream, "epoch", "https://epoch.ai/data")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Patch(ctx, epoch.ID, store.Patch{CurationURL: "file:///data/families.overlay.json", Values: []store.Values{
		{Kind: store.KindFamily, Key: "H100", Columns: map[string]any{"memory_gb": 81.0}, Curated: []string{"memory_gb"}}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishImport(ctx, epoch.ID, "v1", nil, nil); err != nil {
		t.Fatal(err)
	}
	run, err := importers.Execute(ctx, s, imp, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	want := []store.Conflict{{Kind: store.KindFamily, Key: "H100", Field: "memory_gb", Curated: 81.0, Imported: 80.0}}
	if sum := summaryOf(t, run); !reflect.DeepEqual(sum.Conflicts, want) || len(sum.Changes) != 0 {
		t.Errorf("summary %+v, want no changes and conflicts %+v", sum, want)
	}
	in, err := s.Effective(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range in.Families {
		if f.Name == "H100" && f.MemoryGB != 81 {
			t.Errorf("H100 memory_gb %v after the bootstrap, want the curated 81", f.MemoryGB)
		}
	}
}

func TestFailedImportIsRecorded(t *testing.T) {
	s, db := newStore(t, "bootstrap_fail")
	imp, err := bootstrap.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	run, err := importers.Execute(ctx, s, imp, uuid.New())
	if err == nil {
		t.Fatal("import of an empty directory succeeded")
	}
	var stored model.ImportRun
	if err := db.Take(&stored, "id = ?", run.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Status != model.RunFailed || stored.FinishedAt == nil || !strings.Contains(stored.Error, "families.json") {
		t.Errorf("stored run = %+v, want failed naming the missing file", stored)
	}
}
