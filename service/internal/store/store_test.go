package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"

	catalog "github.com/fishman/neocloud-catalog"
	"github.com/fishman/neocloud-catalog/service/internal/dbtest"
	"github.com/fishman/neocloud-catalog/service/internal/model"
	"github.com/fishman/neocloud-catalog/service/internal/store"
)

var ctx = context.Background()

func newStore(t *testing.T, suffix string) (*store.Store, *gorm.DB) {
	t.Helper()
	sqlDB, gormDB := dbtest.Fresh(t, suffix)
	dbtest.Migrate(t, sqlDB)
	return store.New(gormDB), gormDB
}

// startRun records a running import run writing layer with source.
func startRun(t *testing.T, db *gorm.DB, layer, source string) uuid.UUID {
	t.Helper()
	run := model.ImportRun{Importer: "test", InputVersion: "v1", Source: source, SourceURL: "https://example.com/" + source,
		Layer: layer, Status: model.RunRunning, StartedAt: time.Now()}
	if err := db.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	return run.ID
}

// committed reads the committed data/*.json, the bootstrap input.
func committed(t *testing.T) store.Inputs {
	t.Helper()
	var in store.Inputs
	for file, v := range map[string]any{
		"families.json":     &in.Families,
		"node_types.json":   &in.NodeTypes,
		"models.json":       &in.Models,
		"draft_models.json": &in.Drafts,
		"backends.json":     &in.Backends,
	} {
		b, err := os.ReadFile(filepath.Join("../../../data", file))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, v); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
	}
	return in
}

func clone[T any](t *testing.T, v T) T {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var c T
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func effective(t *testing.T, s *store.Store) store.Inputs {
	t.Helper()
	in, err := s.Effective(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return in
}

// sameCore compares the core objects of two inputs, field by field.
func sameCore(t *testing.T, got, want store.Inputs) {
	t.Helper()
	for _, c := range []struct {
		name      string
		got, want any
	}{
		{"families", got.Families, want.Families},
		{"node types", got.NodeTypes, want.NodeTypes},
		{"models", got.Models, want.Models},
		{"drafts", got.Drafts, want.Drafts},
		{"backends", got.Backends, want.Backends},
	} {
		if !reflect.DeepEqual(c.got, c.want) {
			g, _ := json.Marshal(c.got)
			w, _ := json.Marshal(c.want)
			t.Errorf("%s differ:\n got %s\nwant %s", c.name, g, w)
		}
	}
}

func provenance(t *testing.T, s *store.Store, kind, key string) []store.ProvenanceEntry {
	t.Helper()
	p, err := s.Provenance(ctx, kind, key)
	if err != nil {
		t.Fatalf("provenance %s %s: %v", kind, key, err)
	}
	return p
}

// rowsOf counts the curated rows a run wrote.
func rowsOf(t *testing.T, db *gorm.DB, run uuid.UUID) int64 {
	t.Helper()
	var n int64
	for _, m := range []any{&model.Family{}, &model.NodeType{}, &model.Model{}, &model.ModelQuantization{}, &model.ModelSource{},
		&model.ModelFile{}, &model.Preset{}, &model.DraftModel{}, &model.Backend{}, &model.BackendVersion{}, &model.FlagTable{},
		&model.ExtensionPayload{}} {
		var c int64
		if err := db.Model(m).Where("import_run_id = ?", run).Count(&c).Error; err != nil {
			t.Fatal(err)
		}
		n += c
	}
	return n
}

// Every core field of the committed data survives ReplaceLayer and Effective,
// in curated order; replacing the layer with the same input changes no row.
func TestReplaceLayerRoundTripsCommittedData(t *testing.T) {
	s, db := newStore(t, "roundtrip")
	in := committed(t)
	first := startRun(t, db, model.LayerUpstream, model.SourceCuration)
	if err := s.ReplaceLayer(ctx, model.LayerUpstream, first, in); err != nil {
		t.Fatal(err)
	}
	sameCore(t, effective(t, s), in)

	written := rowsOf(t, db, first)
	second := startRun(t, db, model.LayerUpstream, model.SourceCuration)
	if err := s.ReplaceLayer(ctx, model.LayerUpstream, second, committed(t)); err != nil {
		t.Fatal(err)
	}
	if n := rowsOf(t, db, second); n != 0 {
		t.Errorf("re-import of the same input rewrote %d rows", n)
	}
	if n := rowsOf(t, db, first); n != written {
		t.Errorf("first run holds %d rows after the re-import, want %d", n, written)
	}
	sameCore(t, effective(t, s), in)
}

func TestEffectiveCatalogLayering(t *testing.T) {
	s, db := newStore(t, "layering")
	in := committed(t)
	run := startRun(t, db, model.LayerUpstream, model.SourceCuration)
	if err := s.ReplaceLayer(ctx, model.LayerUpstream, run, in); err != nil {
		t.Fatal(err)
	}
	upstream := effective(t, s)
	slug := upstream.Models[1].Slug

	// A local override replaces the whole model, children included, in place.
	override := clone(t, upstream.Models[1])
	override.Description = "operator build"
	override.Presets = override.Presets[:1]
	override.Quantizations[0].Sources = nil
	added := clone(t, upstream.Models[0])
	added.Slug = "operator-model"
	for _, m := range []catalog.ModelTemplate{override, added} {
		if err := s.Upsert(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	want := clone(t, upstream)
	want.Models[1] = override
	want.Models = append(want.Models, added)
	sameCore(t, effective(t, s), want)

	p := provenance(t, s, store.KindModel, slug)
	if len(p) != 2 {
		t.Fatalf("provenance of %s = %+v, want the local and the upstream row", slug, p)
	}
	if l := p[0]; l.Layer != model.LayerLocal || !l.Effective || l.Source != model.SourceCuration || l.ImportRunID != nil ||
		l.Fields["description"].Source != model.SourceCuration {
		t.Errorf("local entry = %+v, want the effective curated override", l)
	}
	if u := p[1]; u.Layer != model.LayerUpstream || u.Effective || u.ImportRunID == nil || *u.ImportRunID != run {
		t.Errorf("upstream entry = %+v, want the overridden row of run %s", u, run)
	}

	// Deleting the override restores the upstream model.
	for _, key := range []string{slug, added.Slug} {
		if err := s.Delete(ctx, store.KindModel, key); err != nil {
			t.Fatal(err)
		}
	}
	sameCore(t, effective(t, s), upstream)
	if p := provenance(t, s, store.KindModel, slug); len(p) != 1 || p[0].Layer != model.LayerUpstream || !p[0].Effective {
		t.Errorf("provenance after delete = %+v, want the effective upstream row", p)
	}
	if err := s.Delete(ctx, store.KindModel, slug); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("deleting a missing override: %v, want ErrNotFound", err)
	}
}

func TestReplaceLayerIsAtomic(t *testing.T) {
	s, db := newStore(t, "atomic")
	if err := s.ReplaceLayer(ctx, model.LayerUpstream, startRun(t, db, model.LayerUpstream, model.SourceCuration), committed(t)); err != nil {
		t.Fatal(err)
	}
	before := effective(t, s)

	// Rows of other tables change first; the backend's NUL byte then fails
	// in Postgres.
	bad := committed(t)
	bad.Models = bad.Models[1:]
	bad.Families[0].MemoryGB++
	bad.NodeTypes = append(bad.NodeTypes, catalog.NodeType{ID: "aws/new", Family: "H100", GPUs: 8, Source: "https://example.com"})
	last := &bad.Backends[len(bad.Backends)-1]
	last.Versions[0].HealthPath = "/health\x00"
	run := startRun(t, db, model.LayerUpstream, model.SourceCuration)
	err := s.ReplaceLayer(ctx, model.LayerUpstream, run, bad)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("replace with a failing row: %v, want the database's error", err)
	}
	sameCore(t, effective(t, s), before)
	if n := rowsOf(t, db, run); n != 0 {
		t.Errorf("failed run left %d rows", n)
	}
}

func TestOverlayOnlyAddsPayloads(t *testing.T) {
	s, db := newStore(t, "overlay")
	upRun := startRun(t, db, model.LayerUpstream, model.SourceCuration)
	if err := s.ReplaceLayer(ctx, model.LayerUpstream, upRun, committed(t)); err != nil {
		t.Fatal(err)
	}
	before := effective(t, s)
	snap, _, err := catalog.Generate(before.Families, before.NodeTypes, before.Models, before.Drafts, before.Backends)
	if err != nil {
		t.Fatal(err)
	}
	m, b := before.Models[0], before.Backends[0]
	payload := catalog.Extensions{"example.com/tier": json.RawMessage(`{"tier": "gold", "rank": 1}`)}
	ov := catalog.Overlay{
		Families: map[string]catalog.Extensions{before.Families[0].Name: payload},
		Models:   map[string]catalog.Extensions{m.Slug: payload},
		Presets:  map[string]catalog.Extensions{m.Slug + "/" + m.Presets[0].Name: payload},
		Fits:     map[string]catalog.Extensions{catalog.FitKey(snap.Fits[0]): payload},
		Backends: map[string]catalog.Extensions{b.Name + "/" + b.Versions[0].Version: payload},
	}
	ovRun := startRun(t, db, model.LayerOverlay, "overlay-import")
	if err := s.ReplaceLayer(ctx, model.LayerOverlay, ovRun, store.Inputs{Extensions: ov}); err != nil {
		t.Fatal(err)
	}

	after := effective(t, s)
	sameCore(t, after, before)
	got := after.Extensions
	if got.SchemaVersion != catalog.SchemaVersion {
		t.Errorf("overlay schema = %q, want %q", got.SchemaVersion, catalog.SchemaVersion)
	}
	got.SnapshotDigest = snap.Digest()
	merged, err := catalog.Merge(snap, got)
	if err != nil {
		t.Fatalf("merge effective overlay: %v", err)
	}
	var tier struct {
		Tier string `json:"tier"`
	}
	if err := json.Unmarshal(merged.Fits[0].Extensions["example.com/tier"], &tier); err != nil || tier.Tier != "gold" {
		t.Errorf("merged fit payload = %s (%v), want tier gold", merged.Fits[0].Extensions["example.com/tier"], err)
	}
	if n := len(merged.Models[0].Presets[0].Extensions) + len(merged.Backends[0].Versions[0].Extensions) +
		len(merged.Families[0].Extensions) + len(merged.Models[0].Extensions); n != 4 {
		t.Errorf("merged %d family, model, preset and backend payloads, want 4", n)
	}

	refused := map[string]func() error{
		"payload of an unknown model": func() error {
			return s.ReplaceLayer(ctx, model.LayerOverlay, ovRun, store.Inputs{Extensions: catalog.Overlay{
				Models: map[string]catalog.Extensions{"no-such-model": payload}}})
		},
		"payload of a fit of an unknown family": func() error {
			return s.ReplaceLayer(ctx, model.LayerOverlay, ovRun, store.Inputs{Extensions: catalog.Overlay{
				Fits: map[string]catalog.Extensions{m.Slug + "/B200/-/bf16/auto": payload}}})
		},
		"core object in the overlay": func() error {
			return s.ReplaceLayer(ctx, model.LayerOverlay, ovRun, store.Inputs{Extensions: ov, Families: before.Families[:1]})
		},
		"payloads in upstream": func() error {
			in := committed(t)
			in.Extensions = ov
			return s.ReplaceLayer(ctx, model.LayerUpstream, upRun, in)
		},
		"payload inside a local object": func() error {
			f := before.Families[0]
			f.Extensions = payload
			return s.Upsert(ctx, f)
		},
	}
	for name, write := range refused {
		if err := write(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if final := effective(t, s); !reflect.DeepEqual(final.Extensions, after.Extensions) {
		t.Errorf("refused writes changed the overlay: %+v", final.Extensions)
	}
}

// An importer run never overwrites a curated value (R12); values it imported
// itself it updates, and every unchanged value keeps its provenance.
func TestImporterKeepsCuratedFields(t *testing.T) {
	s, db := newStore(t, "curated")
	h100 := committed(t).Families[2]
	if err := s.ReplaceLayer(ctx, model.LayerUpstream, startRun(t, db, model.LayerUpstream, model.SourceCuration),
		store.Inputs{Families: []catalog.Family{h100}}); err != nil {
		t.Fatal(err)
	}

	imported := clone(t, h100)
	imported.MemoryGB = 1
	x := clone(t, h100)
	x.Name, x.MemoryGB = "X100", 100
	epoch := startRun(t, db, model.LayerUpstream, "epoch")
	if err := s.ReplaceLayer(ctx, model.LayerUpstream, epoch, store.Inputs{Families: []catalog.Family{imported, x}}); err != nil {
		t.Fatal(err)
	}
	x.MemoryGB = 120
	again := startRun(t, db, model.LayerUpstream, "epoch")
	if err := s.ReplaceLayer(ctx, model.LayerUpstream, again, store.Inputs{Families: []catalog.Family{imported, x}}); err != nil {
		t.Fatal(err)
	}

	sameCore(t, effective(t, s), store.Inputs{Families: []catalog.Family{h100, x}})
	if f := provenance(t, s, store.KindFamily, h100.Name)[0].Fields["memory_gb"]; f.Source != model.SourceCuration {
		t.Errorf("H100 memory_gb provenance = %+v, want curation", f)
	}
	fields := provenance(t, s, store.KindFamily, x.Name)[0].Fields
	if f := fields["memory_gb"]; f.Source != "epoch" || f.ImportRunID == nil || *f.ImportRunID != again {
		t.Errorf("X100 memory_gb provenance = %+v, want run %s", f, again)
	}
	if f := fields["vendor"]; f.ImportRunID == nil || *f.ImportRunID != epoch {
		t.Errorf("X100 vendor provenance = %+v, want the unchanged value's run %s", f, epoch)
	}

	curated := clone(t, h100)
	curated.MemoryGB = 81
	if err := s.ReplaceLayer(ctx, model.LayerUpstream, startRun(t, db, model.LayerUpstream, model.SourceCuration),
		store.Inputs{Families: []catalog.Family{curated}}); err != nil {
		t.Fatal(err)
	}
	sameCore(t, effective(t, s), store.Inputs{Families: []catalog.Family{curated}})
}

func TestReplaceLayerRefusesBadRunsAndKeys(t *testing.T) {
	s, db := newStore(t, "refuse")
	in := committed(t)
	finished := model.ImportRun{Importer: "test", Source: model.SourceCuration, Layer: model.LayerUpstream,
		Status: model.RunSucceeded, StartedAt: time.Now()}
	if err := db.Create(&finished).Error; err != nil {
		t.Fatal(err)
	}
	dupModel := committed(t)
	dupModel.Models = append(dupModel.Models, dupModel.Models[0])
	dupFile := committed(t)
	src := &dupFile.Models[0].Quantizations[0].Sources[0]
	src.Files = []catalog.ModelFile{{Path: "a.gguf", Size: 1, Digest: "sha256:00"}, {Path: "a.gguf", Size: 2, Digest: "sha256:01"}}
	up := startRun(t, db, model.LayerUpstream, model.SourceCuration)
	for name, c := range map[string]struct {
		layer string
		run   uuid.UUID
		in    store.Inputs
	}{
		"unknown run":          {model.LayerUpstream, uuid.New(), in},
		"run of another layer": {model.LayerLocal, up, in},
		"finished run":         {model.LayerUpstream, finished.ID, in},
		"duplicate model":      {model.LayerUpstream, up, dupModel},
		"duplicate file path":  {model.LayerUpstream, up, dupFile},
	} {
		if err := s.ReplaceLayer(ctx, c.layer, c.run, c.in); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if got := effective(t, s); len(got.Models)+len(got.Families) != 0 {
		t.Errorf("refused writes left %d models and %d families", len(got.Models), len(got.Families))
	}
}
