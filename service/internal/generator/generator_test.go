// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

package generator_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"gorm.io/gorm"

	catalog "github.com/fishman/neocloud-catalog"
	"github.com/fishman/neocloud-catalog/service/internal/dbtest"
	"github.com/fishman/neocloud-catalog/service/internal/generator"
	"github.com/fishman/neocloud-catalog/service/internal/importers"
	"github.com/fishman/neocloud-catalog/service/internal/importers/bootstrap"
	"github.com/fishman/neocloud-catalog/service/internal/model"
	"github.com/fishman/neocloud-catalog/service/internal/store"
)

const dataDir = "../../../data"

var ctx = context.Background()

// bootstrapped is a store holding the committed data/*.json in upstream.
func bootstrapped(t *testing.T, suffix string) (*store.Store, *gorm.DB, model.ImportRun) {
	t.Helper()
	sqlDB, db := dbtest.Fresh(t, suffix)
	dbtest.Migrate(t, sqlDB)
	s := store.New(db)
	imp, err := bootstrap.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	run, err := importers.Execute(ctx, s, imp, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	return s, db, run
}

// fromFiles generates the snapshot of the committed files, read directly.
func fromFiles(t *testing.T) (catalog.CatalogSnapshot, []string) {
	t.Helper()
	var families []catalog.Family
	var nodeTypes []catalog.NodeType
	var models []catalog.ModelTemplate
	var drafts []catalog.DraftModel
	var backends []catalog.Backend
	for file, v := range map[string]any{"families.json": &families, "node_types.json": &nodeTypes, "models.json": &models,
		"draft_models.json": &drafts, "backends.json": &backends} {
		b, err := os.ReadFile(filepath.Join(dataDir, file))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, v); err != nil {
			t.Fatal(err)
		}
	}
	snap, report, err := catalog.Generate(families, nodeTypes, models, drafts, backends)
	if err != nil {
		t.Fatal(err)
	}
	return snap, report
}

func stored(t *testing.T, db *gorm.DB, id uuid.UUID) model.GeneratorRun {
	t.Helper()
	var run model.GeneratorRun
	if err := db.Take(&run, "id = ?", id).Error; err != nil {
		t.Fatal(err)
	}
	return run
}

func count(t *testing.T, db *gorm.DB, row any, run uuid.UUID) int {
	t.Helper()
	var n int64
	if err := db.Model(row).Where("generator_run_id = ?", run).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	return int(n)
}

// Generating from imported data/*.json gives the committed snapshot's content.
func TestBootstrapRoundTripDigest(t *testing.T) {
	s, db, imp := bootstrapped(t, "gen_digest")
	run, err := generator.Run(ctx, s, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	files, _ := fromFiles(t)
	if run.Snapshot == nil {
		t.Fatal("generator run stored no snapshot")
	}
	if got, want := run.Snapshot.Digest(), files.Digest(); got != want {
		t.Errorf("snapshot digest from the database %s, from the files %s", got, want)
	}
	committed, err := catalog.SnapshotV4()
	if err != nil {
		t.Fatal(err)
	}
	export := *stored(t, db, run.ID).Snapshot
	if export.Version != "" {
		t.Errorf("stored snapshot version = %q, want it left to the release", export.Version)
	}
	export.Version = committed.Version
	if got, want := export.Digest(), committed.Digest(); got != want {
		t.Errorf("stored snapshot at the committed version has digest %s, committed snapshot %s", got, want)
	}
	if !reflect.DeepEqual([]uuid.UUID(run.InputImportRuns), []uuid.UUID{imp.ID}) {
		t.Errorf("input import runs = %v, want the bootstrap run %s", run.InputImportRuns, imp.ID)
	}
}

// A failed generation is stored as a failed run without derived rows.
func TestGeneratorRunStoresReport(t *testing.T) {
	s, db, _ := bootstrapped(t, "gen_report")
	eff, err := s.Effective(ctx)
	if err != nil {
		t.Fatal(err)
	}
	huge := eff.Models[0]
	huge.Slug = "huge-model"
	huge.Quantizations = []catalog.Quantization{huge.Quantizations[0]}
	huge.Quantizations[0].WeightsGB = 1e6
	huge.Presets = nil
	for _, p := range eff.Models[0].Presets {
		if p.Quantization == huge.Quantizations[0].Name && p.Speculative == nil {
			huge.Presets = append(huge.Presets, p)
		}
	}
	if len(huge.Presets) == 0 {
		t.Fatal("no preset to copy")
	}
	if err := s.Upsert(ctx, huge); err != nil {
		t.Fatal(err)
	}
	in, runs, err := s.EffectiveWithRuns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	snap, report, err := catalog.Generate(in.Families, in.NodeTypes, in.Models, in.Drafts, in.Backends)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(report, "\n"), "huge-model: no deployment minimum") {
		t.Fatalf("report %q lacks the huge model", report)
	}

	run, err := generator.Run(ctx, s, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	got := stored(t, db, run.ID)
	if got.Status != model.RunSucceeded || got.FinishedAt == nil || got.Error != "" || got.SchemaVersion != catalog.SchemaVersion {
		t.Errorf("stored run = %+v, want a succeeded run of schema %s", got, catalog.SchemaVersion)
	}
	if !reflect.DeepEqual([]string(got.Report), report) || !reflect.DeepEqual([]string(run.Report), report) {
		t.Errorf("stored report %q, returned %q, want %q", got.Report, run.Report, report)
	}
	if got.InputDigest != generator.InputDigest(in) || got.InputDigest == "" {
		t.Errorf("input digest = %q, want %q", got.InputDigest, generator.InputDigest(in))
	}
	if !reflect.DeepEqual([]uuid.UUID(got.InputImportRuns), runs) {
		t.Errorf("input import runs = %v, want %v", got.InputImportRuns, runs)
	}
	if got.Snapshot == nil || got.Snapshot.Digest() != snap.Digest() || !got.Snapshot.GeneratedAt.Equal(got.StartedAt) {
		t.Errorf("stored snapshot does not match the generated one at the run's start")
	}

	estimates, guidance := 0, 0
	for _, f := range snap.Fits {
		if f.Performance != nil {
			estimates++
		}
	}
	for _, m := range snap.Models {
		if m.Deployment.Minimum != nil {
			guidance++
		}
		if m.Deployment.Optimal != nil {
			guidance++
		}
	}
	if n := count(t, db, &model.Fit{}, run.ID); n != len(snap.Fits) {
		t.Errorf("%d fits stored, want %d", n, len(snap.Fits))
	}
	if n := count(t, db, &model.FitEstimate{}, run.ID); n != estimates {
		t.Errorf("%d fit estimates stored, want %d", n, estimates)
	}
	if n := count(t, db, &model.Guidance{}, run.ID); n != guidance || guidance == 0 {
		t.Errorf("%d guidance rows stored, want %d", n, guidance)
	}
	m := snap.Models[0]
	var min model.Guidance
	if err := db.Take(&min, "generator_run_id = ? AND model_slug = ? AND kind = ?", run.ID, m.Slug, "minimum").Error; err != nil {
		t.Fatal(err)
	}
	r := m.Deployment.Minimum
	if min.Family != r.Family || min.GPUs != r.GPUs || min.Preset != r.Preset || !reflect.DeepEqual([]string(min.FitKeys), r.FitKeys) {
		t.Errorf("stored minimum of %s = %+v, want %+v", m.Slug, min, *r)
	}

	bad := huge
	bad.Slug, bad.Task = "bad-model", "dance"
	if err := s.Upsert(ctx, bad); err != nil {
		t.Fatal(err)
	}
	failed, err := generator.Run(ctx, s, uuid.New())
	if err == nil || !strings.Contains(err.Error(), "dance") {
		t.Fatalf("generation with an unknown task: %v, want its error", err)
	}
	got = stored(t, db, failed.ID)
	if got.Status != model.RunFailed || got.FinishedAt == nil || !strings.Contains(got.Error, "dance") || got.Snapshot != nil {
		t.Errorf("stored failed run = %+v, want failed with the error and no snapshot", got)
	}
	if n := count(t, db, &model.Fit{}, failed.ID); n != 0 {
		t.Errorf("failed run stored %d fits", n)
	}
}

// Same inputs give the same digests, each run with derived rows of its own.
func TestGeneratorRunsAreReproducible(t *testing.T) {
	s, db, _ := bootstrapped(t, "gen_repro")
	first, err := generator.Run(ctx, s, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	second, err := generator.Run(ctx, s, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if first.InputDigest != second.InputDigest || first.Snapshot.Digest() != second.Snapshot.Digest() {
		t.Errorf("same inputs gave input digests %s, %s and snapshot digests %s, %s",
			first.InputDigest, second.InputDigest, first.Snapshot.Digest(), second.Snapshot.Digest())
	}
	if a, b := count(t, db, &model.Fit{}, first.ID), count(t, db, &model.Fit{}, second.ID); a != b || a != len(first.Snapshot.Fits) {
		t.Errorf("runs stored %d and %d fits, want %d each", a, b, len(first.Snapshot.Fits))
	}

	eff, err := s.Effective(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f := eff.Families[0]
	f.MemoryGB++
	if err := s.Upsert(ctx, f); err != nil {
		t.Fatal(err)
	}
	third, err := generator.Run(ctx, s, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if third.InputDigest == first.InputDigest {
		t.Error("changed inputs kept the input digest")
	}
}

// costLines are the newest succeeded run's estimates for llama-3.1-70b on nodeType in region.
func costLines(t *testing.T, s *store.Store, nodeType, region string) []string {
	t.Helper()
	rows, err := s.CostEstimates(ctx, store.CostFilter{Model: "llama-3.1-70b", Region: region})
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, c := range rows {
		if c.NodeTypeID == nodeType {
			lines = append(lines, fmt.Sprintf("%s %s %s %d x %v/h %v/month %s cheapest=%t", c.Setup, c.FitKey, c.Billing, c.Instances,
				c.USDPerHour, c.USDPerMonth, c.PriceSource, c.Cheapest))
		}
	}
	return lines
}

// A local price overrides the upstream one and removing it restores it; prices never touch the input digest.
func TestCostEstimatesUseEffectivePrices(t *testing.T) {
	s, db, _ := bootstrapped(t, "gen_costs")
	const mi300x, h100 = "azure/Standard_ND96isr_MI300X_v5", "azure/Standard_ND96isr_H100_v5"
	imp, err := s.StartImport(ctx, uuid.New(), "azure-prices", model.LayerUpstream, "azure-prices", "https://prices.azure.com/api/retail/prices")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Patch(ctx, imp.ID, store.Patch{Prices: &store.Prices{Prefix: "azure/", Rows: []model.NodeTypePrice{
		{NodeTypeID: mi300x, Region: "eastus2", Billing: model.BillingOnDemand, USDPerHour: 48},
		{NodeTypeID: h100, Region: "eastus", Billing: model.BillingOnDemand, USDPerHour: 98.32},
	}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishImport(ctx, imp.ID, "v1", nil, nil); err != nil {
		t.Fatal(err)
	}
	upstream := []string{
		"fit llama-3.1-70b/MI300X/-/bf16/auto on_demand 1 x 48/h 35040/month azure-prices cheapest=false",
		"fit llama-3.1-70b/MI300X/-/bf16/auto/2-node on_demand 2 x 96/h 70080/month azure-prices cheapest=false",
		"fit llama-3.1-70b/MI300X/-/fp8/auto on_demand 1 x 48/h 35040/month azure-prices cheapest=false",
		"optimal  on_demand 1 x 48/h 35040/month azure-prices cheapest=true",
	}
	first, err := generator.Run(ctx, s, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if got := costLines(t, s, mi300x, "eastus2"); !slices.Equal(got, upstream) {
		t.Errorf("upstream estimates =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(upstream, "\n"))
	}
	var c model.CostEstimate
	if err := db.Take(&c, "generator_run_id = ? AND node_type_id = ? AND setup = 'fit'", first.ID, h100).Error; err != nil {
		t.Fatal(err)
	}
	if c.PriceSource != "azure-prices" || c.PriceSourceURL != imp.SourceURL || !c.PriceAsOf.Equal(imp.StartedAt) {
		t.Errorf("H100 estimate price provenance %s %s %s, want the import run's", c.PriceSource, c.PriceSourceURL, c.PriceAsOf)
	}

	if err := s.ReplaceLocalPrices(ctx, []model.NodeTypePrice{{NodeTypeID: mi300x, Region: "eastus2", Billing: model.BillingOnDemand, USDPerHour: 40}}); err != nil {
		t.Fatal(err)
	}
	local, err := generator.Run(ctx, s, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"fit llama-3.1-70b/MI300X/-/bf16/auto on_demand 1 x 40/h 29200/month curation cheapest=false",
		"fit llama-3.1-70b/MI300X/-/bf16/auto/2-node on_demand 2 x 80/h 58400/month curation cheapest=false",
		"fit llama-3.1-70b/MI300X/-/fp8/auto on_demand 1 x 40/h 29200/month curation cheapest=false",
		"optimal  on_demand 1 x 40/h 29200/month curation cheapest=true",
	}
	if got := costLines(t, s, mi300x, "eastus2"); !slices.Equal(got, want) {
		t.Errorf("estimates with the local price =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if local.InputDigest != first.InputDigest {
		t.Error("a price changed the input digest")
	}

	if err := s.ReplaceLocalPrices(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := generator.Run(ctx, s, uuid.New()); err != nil {
		t.Fatal(err)
	}
	if got := costLines(t, s, mi300x, "eastus2"); !slices.Equal(got, upstream) {
		t.Errorf("estimates after removing the local price =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(upstream, "\n"))
	}
	if a, b := count(t, db, &model.CostEstimate{}, first.ID), count(t, db, &model.CostEstimate{}, local.ID); a != b || a == 0 {
		t.Errorf("runs stored %d and %d cost estimates, want the same nonzero count", a, b)
	}
}
