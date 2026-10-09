package store_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	catalog "github.com/fishman/neocloud-catalog"
	"github.com/fishman/neocloud-catalog/service/internal/dbtest"
	"github.com/fishman/neocloud-catalog/service/internal/model"
	"github.com/fishman/neocloud-catalog/service/internal/store"
)

// rowTimes maps every curated row's id to its update time.
func rowTimes(t *testing.T, db *gorm.DB) map[uuid.UUID]time.Time {
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

func conflictsJSON(t *testing.T, cs []store.Conflict) []string {
	t.Helper()
	out := []string{}
	for _, c := range cs {
		b, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, string(b))
	}
	return out
}

func manifestOf(m catalog.ModelTemplate, quant int, files []catalog.ModelFile) store.Manifest {
	src := m.Quantizations[quant].Sources[0]
	return store.Manifest{ModelSlug: m.Slug, Quantization: m.Quantizations[quant].Name, Type: src.Type, Repo: src.Repo,
		Revision: src.Revision, Files: files}
}

var twoFiles = []catalog.ModelFile{
	{Path: "config.json", Size: 663, Digest: "git-oid:172d20a3d7fc27df9ed89628cafdbaf1a773ddc1"},
	{Path: "model.safetensors", Size: 3885154816, Digest: "sha256:b477be7572f0ab3ae3cbba38d508cc33e70600b2045669c4ad848051c3432094"},
}

// An importer's patch keeps curated values and a curated manifest (R12) and
// lists where its values differ; it fills a source without files and leaves
// every other row as it was.
func TestPatchKeepsCuratedValues(t *testing.T) {
	s, db := newStore(t, "patch_curated")
	in := committed(t)
	if _, _, err := s.ReplaceLayer(ctx, model.LayerUpstream, startRun(t, db, model.LayerUpstream, model.SourceCuration), in); err != nil {
		t.Fatal(err)
	}
	before := rowTimes(t, db)
	qwen := in.Models[4]
	gguf := qwen.Quantizations[2].Sources[0].Files[0]
	bigger := gguf
	bigger.Size++
	run := startRun(t, db, model.LayerUpstream, "huggingface")
	changes, conflicts, err := s.Patch(ctx, run, store.Patch{
		Values: []store.Values{
			{Kind: store.KindModel, Key: qwen.Slug, Columns: map[string]any{"context_max": 131072, "params_b": 14.8}},
			{Kind: store.KindQuantization, Key: qwen.Slug + "/bf16", Columns: map[string]any{"weights_gb": 29.5}},
		},
		Manifests: []store.Manifest{manifestOf(qwen, 0, twoFiles), manifestOf(qwen, 2, []catalog.ModelFile{bigger})},
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := (store.Changes{"model_files": {Inserted: 2}}); !reflect.DeepEqual(changes, want) {
		t.Errorf("changes = %+v, want %+v", changes, want)
	}
	wantConflicts := []string{
		`{"kind":"model","key":"qwen-2.5-14b","field":"context_max","curated":32768,"imported":131072}`,
		`{"kind":"source","key":"qwen-2.5-14b/Q4_K_M huggingface bartowski/Qwen2.5-14B-Instruct-GGUF","field":"files/Qwen2.5-14B-Instruct-Q4_K_M.gguf",` +
			`"curated":{"path":"Qwen2.5-14B-Instruct-Q4_K_M.gguf","size":8988110976,"digest":"sha256:e47ad95dad6ff848b431053b375adb5d39321290ea2c638682577dafca87c008"},` +
			`"imported":{"path":"Qwen2.5-14B-Instruct-Q4_K_M.gguf","size":8988110977,"digest":"sha256:e47ad95dad6ff848b431053b375adb5d39321290ea2c638682577dafca87c008"}}`,
	}
	if got := conflictsJSON(t, conflicts); !reflect.DeepEqual(got, wantConflicts) {
		t.Errorf("conflicts =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(wantConflicts, "\n"))
	}

	want := clone(t, in)
	want.Models[4].Quantizations[0].Sources[0].Files = twoFiles
	sameCore(t, effective(t, s), want)
	if f := dbtest.Provenance(t, s, store.KindModel, qwen.Slug)[0].Fields["context_max"]; f.Source != model.SourceCuration {
		t.Errorf("context_max provenance = %+v, want curation", f)
	}
	after := rowTimes(t, db)
	for id, at := range before {
		if !after[id].Equal(at) {
			t.Errorf("row %s changed outside the patch", id)
		}
	}
	if n := rowsOf(t, db, run); n != 2 || len(after) != len(before)+2 {
		t.Errorf("run wrote %d rows, %d rows added; want the 2 files", n, len(after)-len(before))
	}
}

// Values an importer run imported, or another importer's, it sets: the row
// keeps its origin, the column records the run. Patching again changes
// nothing; a new manifest replaces its source's files and no other source's.
func TestPatchFillsImportedValues(t *testing.T) {
	s, db := newStore(t, "patch_fill")
	in := committed(t)
	other := startRun(t, db, model.LayerUpstream, "other")
	if _, _, err := s.ReplaceLayer(ctx, model.LayerUpstream, other, in); err != nil {
		t.Fatal(err)
	}
	llama := in.Models[0]
	arch := catalog.Architecture{Layers: 1, AttentionHeads: 2, KVHeads: 1, HeadDim: 64}
	patch := store.Patch{
		Values:    []store.Values{{Kind: store.KindModel, Key: llama.Slug, Columns: map[string]any{"architecture": arch, "context_max": llama.ContextMax}}},
		Manifests: []store.Manifest{manifestOf(llama, 0, twoFiles)},
	}
	run := startRun(t, db, model.LayerUpstream, "huggingface")
	changes, conflicts, err := s.Patch(ctx, run, patch)
	if err != nil {
		t.Fatal(err)
	}
	if want := (store.Changes{"models": {Updated: 1}, "model_files": {Inserted: 2}}); !reflect.DeepEqual(changes, want) || len(conflicts) != 0 {
		t.Errorf("changes = %+v conflicts %+v, want %+v and none", changes, conflicts, want)
	}
	want := clone(t, in)
	want.Models[0].Architecture = arch
	want.Models[0].Quantizations[0].Sources[0].Files = twoFiles
	sameCore(t, effective(t, s), want)
	p := dbtest.Provenance(t, s, store.KindModel, llama.Slug)[0]
	if p.Source != "other" || *p.ImportRunID != other {
		t.Errorf("row provenance = %+v, want its origin, run %s", p, other)
	}
	if f := p.Fields["architecture"]; f.Source != "huggingface" || *f.ImportRunID != run {
		t.Errorf("architecture provenance = %+v, want run %s", f, run)
	}
	if f := p.Fields["context_max"]; *f.ImportRunID != other {
		t.Errorf("context_max provenance = %+v, want the unchanged value's run %s", f, other)
	}

	before := rowTimes(t, db)
	if changes, _, err := s.Patch(ctx, startRun(t, db, model.LayerUpstream, "huggingface"), patch); err != nil || len(changes) != 0 {
		t.Errorf("same patch again: changes %+v, err %v; want none", changes, err)
	}
	if after := rowTimes(t, db); !reflect.DeepEqual(after, before) {
		t.Error("same patch again changed rows")
	}

	resized := []catalog.ModelFile{twoFiles[0]}
	resized[0].Size++
	changes, _, err = s.Patch(ctx, startRun(t, db, model.LayerUpstream, "huggingface"),
		store.Patch{Manifests: []store.Manifest{manifestOf(llama, 0, resized)}})
	if want := (store.Changes{"model_files": {Updated: 1, Deleted: 1}}); err != nil || !reflect.DeepEqual(changes, want) {
		t.Errorf("new manifest: changes %+v, err %v; want %+v", changes, err, want)
	}
	want.Models[0].Quantizations[0].Sources[0].Files = resized
	sameCore(t, effective(t, s), want)
}

// A patch is one transaction: any refused part writes nothing.
func TestPatchRefuses(t *testing.T) {
	s, db := newStore(t, "patch_refuse")
	in := committed(t)
	if _, _, err := s.ReplaceLayer(ctx, model.LayerUpstream, startRun(t, db, model.LayerUpstream, "other"), in); err != nil {
		t.Fatal(err)
	}
	finished := model.ImportRun{Importer: "test", Source: "huggingface", Layer: model.LayerUpstream, Status: model.RunSucceeded, StartedAt: time.Now()}
	if err := db.Create(&finished).Error; err != nil {
		t.Fatal(err)
	}
	llama := in.Models[0]
	valid := store.Values{Kind: store.KindModel, Key: llama.Slug, Columns: map[string]any{"context_max": 1}}
	moved := manifestOf(llama, 0, twoFiles)
	moved.Revision = strings.Repeat("0", 40)
	dup := manifestOf(llama, 0, []catalog.ModelFile{twoFiles[0], twoFiles[0]})
	run := startRun(t, db, model.LayerUpstream, "huggingface")
	for name, c := range map[string]struct {
		run   uuid.UUID
		patch store.Patch
		want  error
	}{
		"unknown run":             {uuid.New(), store.Patch{Values: []store.Values{valid}}, nil},
		"finished run":            {finished.ID, store.Patch{Values: []store.Values{valid}}, nil},
		"unknown model":           {run, store.Patch{Values: []store.Values{valid, {Kind: store.KindModel, Key: "nope", Columns: map[string]any{"context_max": 1}}}}, store.ErrNotFound},
		"unknown column":          {run, store.Patch{Values: []store.Values{valid, {Kind: store.KindModel, Key: llama.Slug, Columns: map[string]any{"slug": "x"}}}}, store.ErrInvalid},
		"wrong value type":        {run, store.Patch{Values: []store.Values{valid, {Kind: store.KindModel, Key: llama.Slug, Columns: map[string]any{"context_max": "long"}}}}, store.ErrInvalid},
		"unpatchable kind":        {run, store.Patch{Values: []store.Values{valid, {Kind: store.KindBackend, Key: "vllm", Columns: map[string]any{"position": 1}}}}, store.ErrInvalid},
		"new family partly given": {run, store.Patch{Values: []store.Values{valid, {Kind: store.KindFamily, Key: "X100", Columns: map[string]any{"memory_gb": 80.0}}}}, store.ErrInvalid},
		"source pinned anew":      {run, store.Patch{Values: []store.Values{valid}, Manifests: []store.Manifest{moved}}, store.ErrConflict},
		"duplicate file path":     {run, store.Patch{Values: []store.Values{valid}, Manifests: []store.Manifest{dup}}, store.ErrInvalid},
		"unknown source owner":    {run, store.Patch{Values: []store.Values{valid}, Manifests: []store.Manifest{{ModelSlug: llama.Slug, Quantization: "int4", Type: "huggingface", Repo: "x/y"}}}, store.ErrNotFound},
	} {
		_, _, err := s.Patch(ctx, c.run, c.patch)
		if err == nil || c.want != nil && !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", name, err, c.want)
		}
	}
	sameCore(t, effective(t, s), in)
}

// A patch stamps only the columns it sets: a column the row has no field
// provenance for (one a later migration added) stays without one.
func TestPatchStampsOnlyItsColumns(t *testing.T) {
	s, db := newStore(t, "patch_untouched")
	in := committed(t)
	if _, _, err := s.ReplaceLayer(ctx, model.LayerUpstream, startRun(t, db, model.LayerUpstream, "other"), in); err != nil {
		t.Fatal(err)
	}
	llama := in.Models[0]
	if err := db.Exec(`UPDATE models SET field_provenance = field_provenance - 'license' WHERE slug = ?`, llama.Slug).Error; err != nil {
		t.Fatal(err)
	}
	patch := func(contextMax int) store.Changes {
		t.Helper()
		changes, _, err := s.Patch(ctx, startRun(t, db, model.LayerUpstream, "huggingface"),
			store.Patch{Values: []store.Values{{Kind: store.KindModel, Key: llama.Slug, Columns: map[string]any{"context_max": contextMax}}}})
		if err != nil {
			t.Fatal(err)
		}
		return changes
	}
	if changes := patch(llama.ContextMax); len(changes) != 0 {
		t.Errorf("same value: changes %+v, want none", changes)
	}
	if changes := patch(1); !reflect.DeepEqual(changes, store.Changes{"models": {Updated: 1}}) {
		t.Errorf("new value: changes %+v, want the model updated", changes)
	}
	fields := dbtest.Provenance(t, s, store.KindModel, llama.Slug)[0].Fields
	if f, ok := fields["license"]; ok {
		t.Errorf("license provenance %+v, want none: the patch did not set it", f)
	}
	if fields["context_max"].Source != "huggingface" {
		t.Errorf("context_max provenance %+v, want the patch's", fields["context_max"])
	}
}

// A price patch writes only its provider's prices: rows outside its prefix or
// of a node type it keeps are refused, and a refused patch writes nothing.
func TestPatchPricesStaysInScope(t *testing.T) {
	s, db := newStore(t, "patch_prices")
	run := startRun(t, db, model.LayerUpstream, "azure-prices")
	ok := model.NodeTypePrice{NodeTypeID: "azure/a", Region: "eastus", Billing: model.BillingSpot, USDPerHour: 1}
	for name, p := range map[string]store.Prices{
		"no prefix":      {Rows: []model.NodeTypePrice{ok}},
		"other provider": {Prefix: "azure/", Rows: []model.NodeTypePrice{ok, {NodeTypeID: "aws/a", Region: "us-east-1", Billing: model.BillingSpot}}},
		"kept":           {Prefix: "azure/", Kept: []string{"azure/a"}, Rows: []model.NodeTypePrice{ok}},
		"billing":        {Prefix: "azure/", Rows: []model.NodeTypePrice{{NodeTypeID: "azure/b", Region: "eastus", Billing: "monthly"}}},
	} {
		if _, _, err := s.Patch(ctx, run, store.Patch{Prices: &p}); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("%s: err %v, want ErrInvalid", name, err)
		}
	}
	var n int64
	if err := db.Model(&model.NodeTypePrice{}).Count(&n).Error; err != nil || n != 0 {
		t.Errorf("%d prices written by refused patches (%v)", n, err)
	}
}
