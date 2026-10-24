// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
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

// committedWithEmptyLists adds a model and a backend with explicitly empty lists: the snapshot
// marshals them without omitempty, so nil would change the digest.
func committedWithEmptyLists(t *testing.T) store.Inputs {
	in := committed(t)
	in.Models = append(in.Models, catalog.ModelTemplate{Slug: "no-quantizations", Name: "None", Family: "none", Task: "chat",
		Quantizations: []catalog.Quantization{}})
	in.Backends = append(in.Backends, catalog.Backend{Name: "no-versions", Versions: []catalog.BackendVersion{}})
	return in
}

// everyField sets every stored core field the committed data leaves zero (prefix caching unset, off and on).
func everyField() store.Inputs {
	on, off := true, false
	preset := func(name string, prefix *bool) catalog.Preset {
		return catalog.Preset{Name: name, Quantization: "fp8", Backend: "vllm", BackendVersion: "v2", MinBackendVersion: "v1",
			Purpose: "latency", Context: 8192, TensorParallel: 2, KVCacheDType: "fp8", PrefixCaching: prefix, ToolParser: "hermes",
			ReasoningParser: "deepseek_r1", ChatTemplate: "tools", Speculative: &catalog.Speculative{Method: "eagle3", DraftModel: "d", Tokens: 3},
			Topology: catalog.Topology{Kind: "disaggregated", Nodes: 2, Pipeline: 2, Prefill: 4, Decode: 4, Transfer: "nixl"},
			Env:      map[string]string{"VLLM_USE_V1": "1"}, ExtraArgs: []string{"--enforce-eager"}, Vendors: []string{"nvidia"}}
	}
	return store.Inputs{
		Families: []catalog.Family{{Name: "F", Vendor: "nvidia", Architecture: "hopper", ComputeCapability: "9.0", DTypes: []string{"bf16"},
			MemoryGB: 80, MemoryBandwidthGBs: 3350.5, TFLOPS: map[string]float64{"bf16": 989.4}, Interconnect: "nvlink",
			InterconnectGBs: 900, Sources: []string{"https://example.com/f"}}},
		NodeTypes: []catalog.NodeType{{ID: "aws/f.48xlarge", Family: "F", GPUs: 8, Source: "https://example.com/n"}},
		Models: []catalog.ModelTemplate{{Slug: "m", Name: "M", Family: "llama", Description: "d", Task: "chat",
			Capabilities: []string{"tools"}, ParamsB: 8.03, ParamsActiveB: 8.03, ReleaseDate: "2026-01-01", Home: "https://example.com/m",
			Icon: "https://example.com/m.png", Aliases: []string{"mm"}, DefaultContext: 8192, ContextMax: 131072,
			Quantizations: []catalog.Quantization{{Name: "fp8", Kind: "fp8", RequiresDType: "fp8", WeightsGB: 8.5,
				Sources: []catalog.ModelSource{{Type: "huggingface", Repo: "org/m", Revision: "abc",
					Files: []catalog.ModelFile{{Path: "model.safetensors", Size: 8_500_000_000, Digest: "sha256:aa"}}}}}},
			Architecture: catalog.Architecture{Layers: 32, AttentionHeads: 32, KVHeads: 8, HeadDim: 128}, License: "apache-2.0", Gated: true,
			Presets: []catalog.Preset{preset("unset", nil), preset("off", &off), preset("on", &on)}}},
		Drafts: []catalog.DraftModel{{Slug: "d", Method: "eagle3", Targets: []string{"m"}, WeightsGB: 1.5, Backends: []string{"vllm"},
			Source: catalog.ModelSource{Type: "huggingface", Repo: "org/d", Revision: "def",
				Files: []catalog.ModelFile{{Path: "config.json", Size: 512, Digest: "git-oid:bb"}}}}},
		Backends: []catalog.Backend{{Name: "vllm", Versions: []catalog.BackendVersion{{Version: "v2",
			Images: map[string]string{"nvidia": "vllm:v2"}, Command: []string{"vllm", "serve", "{{model_path}}"}, HealthPath: "/health",
			MinComputeCapability: "8.0", Quantizations: map[string]catalog.QuantSupport{"fp8": {NativeDType: "fp8", EmulatedFrom: "8.0"}},
			Defaults: map[string]string{"gpu_memory_utilization": "0.9"},
			Flags: catalog.FlagTable{Context: "--max-model-len", TensorParallel: "--tensor-parallel-size",
				PipelineParallel: "--pipeline-parallel-size", KVCacheDType: "--kv-cache-dtype", KVCacheDTypes: map[string]string{"fp8": "fp8_e4m3"},
				PrefixCachingOn: "--enable-prefix-caching", PrefixCachingOff: "--no-enable-prefix-caching", ToolParser: "--tool-call-parser",
				ToolParserNeeds: []string{"--enable-auto-tool-choice"}, ToolParsers: map[string]string{"hermes": "hermes"},
				ReasoningParser: "--reasoning-parser", ReasoningParsers: map[string]string{"deepseek_r1": "deepseek_r1"},
				ChatTemplate: "--chat-template", ChatTemplates: map[string]map[string]string{"tools": {"nvidia": "/t.jinja"}},
				Speculative: "json:--speculative-config", SpeculativeMethods: map[string]string{"eagle3": "eagle3"},
				Disaggregation: "vllm", KVTransfers: map[string]string{"nixl": "NixlConnector"}, MultiNode: "vllm", SeparateValues: true}}}}},
		// Keys out of order: the body must keep its key order (json, not jsonb).
		Templates: []catalog.Template{{Kind: "app", Slug: "t", Version: "1.0.0", Status: "tested", Name: "T", Description: "d",
			Category: "c", Icon: "https://example.com/t.png", UpstreamChartRef: &chartRef, ChartDigest: &chartDigest, PendingPublish: true,
			Service: "t", Port: 80, ValuesSchema: json.RawMessage(`[{"key":"z","type":"string"}]`), UpstreamsPath: "u", AppKind: "chart",
			Requests: json.RawMessage(`{"memory":"1Gi","cpu":"1"}`), Requires: []string{"p"}, Expose: json.RawMessage(`{"service":"t","port":80}`),
			PinnedValues: json.RawMessage(`{"z":1,"a":2}`), FormSchema: json.RawMessage(`{"properties":{"zz":{},"a":{}}}`), Namespace: "n",
			AppAPIGroups: []string{"t.example.com"}}},
	}
}

var (
	chartRef    = "oci://ghcr.io/acme/t:1.0.0"
	chartDigest = "sha256:" + strings.Repeat("ab", 32)
)

// requireEveryField fails on a zero field in v outside the derived, overlay-only and unset paths,
// so a new core field must join the fixture.
func requireEveryField(t *testing.T, path string, v reflect.Value, unset map[string]bool) {
	t.Helper()
	switch {
	case unset[path]:
		return
	case v.IsZero():
		t.Errorf("fixture leaves %s zero", path)
		return
	}
	switch v.Kind() {
	case reflect.Pointer:
		if v.Elem().Kind() == reflect.Struct {
			requireEveryField(t, path, v.Elem(), unset)
		}
	case reflect.Struct:
		for i := range v.NumField() {
			if name := v.Type().Field(i).Name; name != "KVBytesPerToken" && name != "Deployment" && name != "Extensions" {
				requireEveryField(t, path+"."+name, v.Field(i), unset)
			}
		}
	case reflect.Slice:
		for i := range v.Len() {
			requireEveryField(t, fmt.Sprintf("%s[%d]", path, i), v.Index(i), unset)
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			requireEveryField(t, fmt.Sprintf("%s[%v]", path, k), v.MapIndex(k), unset)
		}
	}
}

// Every core field survives ReplaceLayer and Effective in order, Generate reproduces the file digest,
// and replacing with the same input changes no row.
func TestReplaceLayerRoundTripsCommittedData(t *testing.T) {
	s, db := newStore(t, "roundtrip")
	in := committedWithEmptyLists(t)
	first := startRun(t, db, model.LayerUpstream, model.SourceCuration)
	if _, _, err := s.ReplaceLayer(ctx, model.LayerUpstream, first, in); err != nil {
		t.Fatal(err)
	}
	got := effective(t, s)
	sameCore(t, got, in)
	fromFiles, _, err := catalog.Generate(in.Families, in.NodeTypes, in.Models, in.Drafts, in.Backends)
	if err != nil {
		t.Fatal(err)
	}
	fromStore, _, err := catalog.Generate(got.Families, got.NodeTypes, got.Models, got.Drafts, got.Backends)
	if err != nil {
		t.Fatal(err)
	}
	if fromStore.Digest() != fromFiles.Digest() {
		t.Errorf("snapshot digest from the store %s, from the files %s", fromStore.Digest(), fromFiles.Digest())
	}

	written := rowsOf(t, db, first)
	second := startRun(t, db, model.LayerUpstream, model.SourceCuration)
	if _, _, err := s.ReplaceLayer(ctx, model.LayerUpstream, second, committedWithEmptyLists(t)); err != nil {
		t.Fatal(err)
	}
	if n := rowsOf(t, db, second); n != 0 {
		t.Errorf("re-import of the same input rewrote %d rows", n)
	}
	if n := rowsOf(t, db, first); n != written {
		t.Errorf("first run holds %d rows after the re-import, want %d", n, written)
	}
	sameCore(t, effective(t, s), in)

	every := everyField()
	requireEveryField(t, "fixture", reflect.ValueOf(every), map[string]bool{"fixture.Models[0].Presets[0].PrefixCaching": true})
	if _, _, err := s.ReplaceLayer(ctx, model.LayerUpstream, startRun(t, db, model.LayerUpstream, model.SourceCuration), every); err != nil {
		t.Fatal(err)
	}
	got = effective(t, s)
	sameCore(t, got, every)
	if !reflect.DeepEqual(got.Templates, every.Templates) {
		t.Errorf("templates = %+v, want %+v", got.Templates, every.Templates)
	}
}

func TestEffectiveCatalogLayering(t *testing.T) {
	s, db := newStore(t, "layering")
	in := committed(t)
	run := startRun(t, db, model.LayerUpstream, model.SourceCuration)
	if _, _, err := s.ReplaceLayer(ctx, model.LayerUpstream, run, in); err != nil {
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

	p := dbtest.Provenance(t, s, store.KindModel, slug)
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
	if p := dbtest.Provenance(t, s, store.KindModel, slug); len(p) != 1 || p[0].Layer != model.LayerUpstream || !p[0].Effective {
		t.Errorf("provenance after delete = %+v, want the effective upstream row", p)
	}
	if err := s.Delete(ctx, store.KindModel, slug); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("deleting a missing override: %v, want ErrNotFound", err)
	}
}

// A local override takes its upstream row's place and local-only keys follow by (position, key), even
// after upstream reorders and grows; node types stay ordered by id.
func TestEffectiveCrossLayerOrder(t *testing.T) {
	s, db := newStore(t, "cross_layer_order")
	in := committed(t)
	if _, _, err := s.ReplaceLayer(ctx, model.LayerUpstream, startRun(t, db, model.LayerUpstream, "test"), in); err != nil {
		t.Fatal(err)
	}
	up := effective(t, s)
	override := clone(t, up.Models[1])
	override.Description = "operator build"
	b, a := clone(t, up.Models[0]), clone(t, up.Models[0])
	b.Slug, a.Slug = "operator-b", "operator-a"
	nt := catalog.NodeType{ID: "aaa/first", Family: up.NodeTypes[0].Family, GPUs: 8, Source: "https://example.com/nt"}
	for _, o := range []any{override, b, a, nt} {
		if err := s.Upsert(ctx, o); err != nil {
			t.Fatal(err)
		}
	}

	slices.Reverse(in.Models)
	for i := range 3 {
		extra := clone(t, in.Models[0])
		extra.Slug = fmt.Sprintf("upstream-new-%d", i)
		in.Models = append(in.Models, extra)
	}
	if _, _, err := s.ReplaceLayer(ctx, model.LayerUpstream, startRun(t, db, model.LayerUpstream, "test"), in); err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, m := range in.Models {
		want = append(want, m.Slug)
	}
	want = append(want, b.Slug, a.Slug)
	got := effective(t, s)
	var slugs []string
	for _, m := range got.Models {
		slugs = append(slugs, m.Slug)
		if m.Slug == override.Slug && m.Description != override.Description {
			t.Errorf("model %s is not the local override", m.Slug)
		}
	}
	if !slices.Equal(slugs, want) {
		t.Errorf("effective model order\n got %v\nwant %v", slugs, want)
	}
	if !slices.IsSortedFunc(got.NodeTypes, func(x, y catalog.NodeType) int { return strings.Compare(x.ID, y.ID) }) || got.NodeTypes[0].ID != nt.ID {
		t.Errorf("node types are not ordered by id with the local %s first", nt.ID)
	}
}

// An overlay that does not apply leaves both layers unchanged; a release without one empties the overlay layer.
func TestReplaceUpstreamIsAtomic(t *testing.T) {
	s, db := newStore(t, "replace_upstream")
	in := committed(t)
	m := in.Models[0]
	payload := catalog.Extensions{"example.com/tier": json.RawMessage(`{"tier":"gold"}`)}
	in.Extensions = catalog.Overlay{Models: map[string]catalog.Extensions{m.Slug: payload},
		Presets: map[string]catalog.Extensions{m.Slug + "/" + m.Presets[1].Name: payload}}
	first := startRun(t, db, model.LayerUpstream, "upstream")
	v := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, _, err := s.ReplaceUpstream(ctx, first, v, in, "https://example.com/overlay"); err != nil {
		t.Fatal(err)
	}
	before := effective(t, s)
	if len(before.Extensions.Models) != 1 || len(before.Extensions.Presets) != 1 {
		t.Fatalf("overlay layer = %+v, want the imported payloads", before.Extensions)
	}
	var p model.ExtensionPayload
	if err := db.Take(&p, "object_kind = 'models'").Error; err != nil {
		t.Fatal(err)
	}
	if p.Layer != model.LayerOverlay || p.ImportRunID == nil || *p.ImportRunID != first || p.SourceURL != "https://example.com/overlay" {
		t.Errorf("overlay payload provenance = %+v, want layer overlay, run %s, the overlay's URL", p.Provenance, first)
	}

	// The override drops a preset the next overlay targets; the upstream layer would change too.
	override := clone(t, m)
	override.Presets = override.Presets[:1]
	if err := s.Upsert(ctx, override); err != nil {
		t.Fatal(err)
	}
	before = effective(t, s)
	next := committed(t)
	next.Models = next.Models[1:]
	next.Families[0].MemoryGB++
	next.Extensions = in.Extensions
	run := startRun(t, db, model.LayerUpstream, "upstream")
	_, _, err := s.ReplaceUpstream(ctx, run, v, next, "https://example.com/overlay")
	if err == nil {
		t.Fatal("an overlay with a payload of a preset the effective catalog lacks was applied")
	}
	if want := "the local model " + m.Slug + " overrides the upstream one without it"; !strings.Contains(err.Error(), want) {
		t.Errorf("refusal %q does not say %q", err, want)
	}
	after := effective(t, s)
	sameCore(t, after, before)
	if !reflect.DeepEqual(after.Extensions, before.Extensions) {
		t.Errorf("a refused import changed the overlay layer: %+v", after.Extensions)
	}
	if n := rowsOf(t, db, run); n != 0 {
		t.Errorf("refused import left %d rows", n)
	}

	if _, _, err := s.ReplaceUpstream(ctx, startRun(t, db, model.LayerOverlay, "upstream"), v, committed(t), ""); err == nil {
		t.Error("an overlay-layer run replaced the upstream layer")
	}
	changes, _, err := s.ReplaceUpstream(ctx, startRun(t, db, model.LayerUpstream, "upstream"), v, committed(t), "")
	if err != nil {
		t.Fatal(err)
	}
	if got := effective(t, s).Extensions; len(got.Models)+len(got.Presets) != 0 || changes["extension_payloads"].Deleted != 2 {
		t.Errorf("a release without an overlay left %+v (changes %+v), want an empty overlay layer", got, changes)
	}
}

// Under the write lock a release older than one another run wrote (finished or not) is refused, so
// imports that both passed the early check cannot roll back; the version is recorded only with the write.
func TestReplaceUpstreamRefusesOlderVersion(t *testing.T) {
	s, db := newStore(t, "upstream_versions")
	start := func() uuid.UUID {
		t.Helper()
		run, err := s.StartImport(ctx, uuid.New(), "upstream", model.LayerUpstream, "upstream", "")
		if err != nil {
			t.Fatal(err)
		}
		return run.ID
	}
	version := func(run uuid.UUID) *time.Time {
		t.Helper()
		var r model.ImportRun
		if err := db.Take(&r, "id = ?", run).Error; err != nil {
			t.Fatal(err)
		}
		return r.ReleaseVersion
	}
	jan, feb := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	older, newer := start(), start()
	if _, _, err := s.ReplaceUpstream(ctx, newer, feb, committed(t), ""); err != nil {
		t.Fatal(err)
	}
	in := committed(t)
	in.Models = in.Models[1:]
	_, _, err := s.ReplaceUpstream(ctx, older, jan, in, "")
	if !errors.Is(err, store.ErrConflict) || !strings.Contains(err.Error(), newer.String()) {
		t.Errorf("an older release after a newer one: %v, want a conflict naming run %s", err, newer)
	}
	if got := effective(t, s); len(got.Models) != len(committed(t).Models) {
		t.Errorf("the older release replaced the newer one (%d models)", len(got.Models))
	}
	if v := version(newer); v == nil || !v.Equal(feb) || version(older) != nil {
		t.Errorf("recorded versions %v and %v, want %s on the run that wrote only", v, version(older), feb)
	}

	if _, err := s.FinishImport(ctx, newer, "", nil, errors.New("worker failed after the write")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ReplaceUpstream(ctx, start(), jan, in, ""); !errors.Is(err, store.ErrConflict) {
		t.Errorf("an older release after a failed run that wrote a newer one: %v, want a conflict", err)
	}
	if _, _, err := s.ReplaceUpstream(ctx, start(), feb, committed(t), ""); err != nil {
		t.Errorf("the release imported last again: %v", err)
	}
}

func TestReplaceLayerIsAtomic(t *testing.T) {
	s, db := newStore(t, "atomic")
	if _, _, err := s.ReplaceLayer(ctx, model.LayerUpstream, startRun(t, db, model.LayerUpstream, model.SourceCuration), committed(t)); err != nil {
		t.Fatal(err)
	}
	before := effective(t, s)

	// Other tables change first; the backend's NUL byte then fails in Postgres.
	bad := committed(t)
	bad.Models = bad.Models[1:]
	bad.Families[0].MemoryGB++
	bad.NodeTypes = append(bad.NodeTypes, catalog.NodeType{ID: "aws/new", Family: "H100", GPUs: 8, Source: "https://example.com"})
	last := &bad.Backends[len(bad.Backends)-1]
	last.Versions[0].HealthPath = "/health\x00"
	run := startRun(t, db, model.LayerUpstream, model.SourceCuration)
	_, _, err := s.ReplaceLayer(ctx, model.LayerUpstream, run, bad)
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
	if _, _, err := s.ReplaceLayer(ctx, model.LayerUpstream, upRun, committed(t)); err != nil {
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
	if _, _, err := s.ReplaceLayer(ctx, model.LayerOverlay, ovRun, store.Inputs{Extensions: ov}); err != nil {
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

	replace := func(layer string, run uuid.UUID, in store.Inputs) error {
		_, _, err := s.ReplaceLayer(ctx, layer, run, in)
		return err
	}
	refused := map[string]func() error{
		"payload of an unknown model": func() error {
			return replace(model.LayerOverlay, ovRun, store.Inputs{Extensions: catalog.Overlay{
				Models: map[string]catalog.Extensions{"no-such-model": payload}}})
		},
		"payload of a fit of an unknown family": func() error {
			return replace(model.LayerOverlay, ovRun, store.Inputs{Extensions: catalog.Overlay{
				Fits: map[string]catalog.Extensions{m.Slug + "/B200/-/bf16/auto": payload}}})
		},
		"core object in the overlay": func() error {
			return replace(model.LayerOverlay, ovRun, store.Inputs{Extensions: ov, Families: before.Families[:1]})
		},
		"payloads in upstream": func() error {
			in := committed(t)
			in.Extensions = ov
			return replace(model.LayerUpstream, upRun, in)
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

// An importer run never overwrites a curated value and reports the disagreement; its own values it
// updates, and unchanged values keep their provenance.
func TestImporterKeepsCuratedFields(t *testing.T) {
	s, db := newStore(t, "curated")
	h100 := committed(t).Families[2]
	if _, _, err := s.ReplaceLayer(ctx, model.LayerUpstream, startRun(t, db, model.LayerUpstream, model.SourceCuration),
		store.Inputs{Families: []catalog.Family{h100}}); err != nil {
		t.Fatal(err)
	}

	imported := clone(t, h100)
	imported.MemoryGB = 1
	x := clone(t, h100)
	x.Name, x.MemoryGB = "X100", 100
	wantConflicts := []string{`{"kind":"family","key":"H100","field":"memory_gb","curated":80,"imported":1}`}
	epoch := startRun(t, db, model.LayerUpstream, "epoch")
	_, conflicts, err := s.ReplaceLayer(ctx, model.LayerUpstream, epoch, store.Inputs{Families: []catalog.Family{imported, x}})
	if err != nil {
		t.Fatal(err)
	}
	if got := conflictsJSON(t, conflicts); !reflect.DeepEqual(got, wantConflicts) {
		t.Errorf("conflicts = %s, want %s", got, wantConflicts)
	}
	x.MemoryGB = 120
	again := startRun(t, db, model.LayerUpstream, "epoch")
	_, conflicts, err = s.ReplaceLayer(ctx, model.LayerUpstream, again, store.Inputs{Families: []catalog.Family{imported, x}})
	if err != nil {
		t.Fatal(err)
	}
	if got := conflictsJSON(t, conflicts); !reflect.DeepEqual(got, wantConflicts) {
		t.Errorf("conflicts of the run again = %s, want %s", got, wantConflicts)
	}

	sameCore(t, effective(t, s), store.Inputs{Families: []catalog.Family{h100, x}})
	if f := dbtest.Provenance(t, s, store.KindFamily, h100.Name)[0].Fields["memory_gb"]; f.Source != model.SourceCuration {
		t.Errorf("H100 memory_gb provenance = %+v, want curation", f)
	}
	fields := dbtest.Provenance(t, s, store.KindFamily, x.Name)[0].Fields
	if f := fields["memory_gb"]; f.Source != "epoch" || f.ImportRunID == nil || *f.ImportRunID != again {
		t.Errorf("X100 memory_gb provenance = %+v, want run %s", f, again)
	}
	if f := fields["vendor"]; f.ImportRunID == nil || *f.ImportRunID != epoch {
		t.Errorf("X100 vendor provenance = %+v, want the unchanged value's run %s", f, epoch)
	}

	curated := clone(t, h100)
	curated.MemoryGB = 81
	if _, _, err := s.ReplaceLayer(ctx, model.LayerUpstream, startRun(t, db, model.LayerUpstream, model.SourceCuration),
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
		if _, _, err := s.ReplaceLayer(ctx, c.layer, c.run, c.in); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if got := effective(t, s); len(got.Models)+len(got.Families) != 0 {
		t.Errorf("refused writes left %d models and %d families", len(got.Models), len(got.Families))
	}
}

// Lists the runs of effective core rows and values: not only the latest, not overridden rows, not the overlay's.
func TestEffectiveWithRunsListsRunsOfEffectiveValues(t *testing.T) {
	s, db := newStore(t, "runs")
	replace := func(layer string, run uuid.UUID, in store.Inputs) {
		t.Helper()
		if _, _, err := s.ReplaceLayer(ctx, layer, run, in); err != nil {
			t.Fatal(err)
		}
	}
	runsOf := func(want ...uuid.UUID) {
		t.Helper()
		in, got, err := s.EffectiveWithRuns(ctx)
		if err != nil {
			t.Fatal(err)
		}
		sameCore(t, in, effective(t, s))
		slices.SortFunc(want, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
		if !reflect.DeepEqual(got, want) {
			t.Errorf("effective import runs = %v, want %v", got, want)
		}
	}
	in := committed(t)
	all := startRun(t, db, model.LayerUpstream, model.SourceCuration)
	replace(model.LayerUpstream, all, in)
	h100 := &in.Families[2]
	h100.MemoryGB++
	memory := startRun(t, db, model.LayerUpstream, model.SourceCuration)
	replace(model.LayerUpstream, memory, in)
	runsOf(all, memory)

	// added is left only in the field provenance of X100's unchanged values.
	x := clone(t, *h100)
	x.Name = "X100"
	in.Families = append(in.Families, x)
	added := startRun(t, db, model.LayerUpstream, "epoch")
	replace(model.LayerUpstream, added, in)
	in.Families[len(in.Families)-1].MemoryGB++
	updated := startRun(t, db, model.LayerUpstream, "epoch")
	replace(model.LayerUpstream, updated, in)
	overlay := startRun(t, db, model.LayerOverlay, "overlay-import")
	replace(model.LayerOverlay, overlay, store.Inputs{Extensions: catalog.Overlay{
		Families: map[string]catalog.Extensions{"X100": {"example.com/tier": json.RawMessage(`{}`)}}}})
	runsOf(all, memory, added, updated)

	if err := s.Upsert(ctx, *h100); err != nil {
		t.Fatal(err)
	}
	runsOf(all, added, updated)
}

// Fresh and finished runs are left alone.
func TestFailAbandonedRuns(t *testing.T) {
	s, db := newStore(t, "store_abandoned")
	oldImport, err := s.StartImport(ctx, uuid.New(), "test", model.LayerUpstream, "test", "")
	if err != nil {
		t.Fatal(err)
	}
	oldGen, err := s.StartGeneratorRun(ctx, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	finished, err := s.StartImport(ctx, uuid.New(), "test", model.LayerUpstream, "test", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishImport(ctx, finished.ID, "v1", nil, nil); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"import_runs", "generator_runs"} {
		if err := db.Exec(`UPDATE ` + table + ` SET started_at = now() - interval '2 hours'`).Error; err != nil {
			t.Fatal(err)
		}
	}
	fresh, err := s.StartImport(ctx, uuid.New(), "test", model.LayerUpstream, "test", "")
	if err != nil {
		t.Fatal(err)
	}

	n, err := s.FailAbandonedRuns(ctx, time.Hour)
	if err != nil || n != 2 {
		t.Fatalf("FailAbandonedRuns = %d, %v; want 2", n, err)
	}
	for id, want := range map[uuid.UUID]string{oldImport.ID: model.RunFailed, finished.ID: model.RunSucceeded, fresh.ID: model.RunRunning} {
		var r model.ImportRun
		if err := db.Take(&r, "id = ?", id).Error; err != nil {
			t.Fatal(err)
		}
		if r.Status != want || (want == model.RunFailed) != (r.Error == store.AbandonedError && r.FinishedAt != nil) {
			t.Errorf("import run %s: %s %q, want %s", id, r.Status, r.Error, want)
		}
	}
	var g model.GeneratorRun
	if err := db.Take(&g, "id = ?", oldGen.ID).Error; err != nil {
		t.Fatal(err)
	}
	if g.Status != model.RunFailed || g.Error != store.AbandonedError || g.FinishedAt == nil {
		t.Errorf("generator run: %s %q", g.Status, g.Error)
	}
	if _, err := s.FinishImport(ctx, oldImport.ID, "v1", nil, nil); err == nil {
		t.Error("the worker finished a run already failed as abandoned")
	}
	if n, err := s.FailAbandonedRuns(ctx, time.Hour); err != nil || n != 0 {
		t.Errorf("second FailAbandonedRuns = %d, %v; want 0", n, err)
	}
}
