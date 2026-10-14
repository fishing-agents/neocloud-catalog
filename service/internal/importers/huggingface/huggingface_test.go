// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

package huggingface

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	catalog "github.com/fishman/neocloud-catalog"
	"github.com/fishman/neocloud-catalog/service/internal/dbtest"
	"github.com/fishman/neocloud-catalog/service/internal/importers"
	"github.com/fishman/neocloud-catalog/service/internal/importers/bootstrap"
	"github.com/fishman/neocloud-catalog/service/internal/model"
	"github.com/fishman/neocloud-catalog/service/internal/store"
)

const (
	dataDir    = "../../../../data"
	fixtureDir = "../../../testdata/hf"
)

var ctx = context.Background()

// hub serves testdata/hf/<repo>/<revision>/<call>.<status>.<ext>; an unrecorded
// call fails the test.
func hub(t *testing.T) *httptest.Server { return rewritingHub(t, nil) }

func rewritingHub(t *testing.T, rewrite func(path string, body []byte) []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		repo, rev, call, ok := route(r.URL.Path)
		var matches []string
		if ok {
			matches, _ = filepath.Glob(filepath.Join(fixtureDir, repo, rev, call+".*"))
		}
		if len(matches) != 1 || call == "tree" && r.URL.Query().Get("recursive") != "1" {
			t.Errorf("unrecorded request %s", r.URL)
			http.NotFound(w, r)
			return
		}
		name := strings.Split(filepath.Base(matches[0]), ".")
		status, err := strconv.Atoi(name[1])
		body, rerr := os.ReadFile(matches[0])
		if err != nil || rerr != nil {
			t.Errorf("fixture %s: %v %v", matches[0], err, rerr)
		}
		if rewrite != nil {
			body = rewrite(r.URL.Path, body)
		}
		w.Header().Set("Content-Type", map[string]string{"json": "application/json; charset=utf-8", "txt": "text/plain; charset=utf-8"}[name[2]])
		w.WriteHeader(status)
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func route(p string) (repo, rev, call string, ok bool) {
	s := strings.Split(strings.TrimPrefix(p, "/"), "/")
	switch {
	case len(s) == 6 && s[0] == "api" && s[1] == "models" && s[4] == "revision":
		return s[2] + "/" + s[3], s[5], "info", true
	case len(s) == 6 && s[0] == "api" && s[1] == "models" && s[4] == "tree":
		return s[2] + "/" + s[3], s[5], "tree", true
	case len(s) == 5 && s[2] == "resolve" && s[4] == "config.json":
		return s[0] + "/" + s[1], s[3], "config", true
	}
	return "", "", "", false
}

func testImporter(endpoint, token string) *Importer {
	i := New(endpoint, token)
	i.interval, i.getter.Backoff = 0, time.Millisecond
	return i
}

func newStore(t *testing.T, suffix string) (*store.Store, *gorm.DB) {
	t.Helper()
	sqlDB, gormDB := dbtest.Fresh(t, suffix)
	dbtest.Migrate(t, sqlDB)
	return store.New(gormDB), gormDB
}

func committed(t *testing.T) store.Inputs {
	t.Helper()
	in, _, err := bootstrap.Load(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	return in
}

func seed(t *testing.T, s *store.Store, source string, in store.Inputs) {
	t.Helper()
	run, err := s.StartImport(ctx, uuid.New(), "seed", model.LayerUpstream, source, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ReplaceLayer(ctx, model.LayerUpstream, run.ID, in); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishImport(ctx, run.ID, "v1", nil, nil); err != nil {
		t.Fatal(err)
	}
}

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

func summaryOf(t *testing.T, run model.ImportRun) (importers.Summary, []string) {
	t.Helper()
	var s importers.Summary
	if err := json.Unmarshal(run.Summary, &s); err != nil {
		t.Fatalf("summary %s: %v", run.Summary, err)
	}
	conflicts := []string{}
	for _, c := range s.Conflicts {
		b, _ := json.Marshal(c)
		conflicts = append(conflicts, string(b))
	}
	return s, conflicts
}

func diff(t *testing.T, what string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s =\n%s\nwant\n%s", what, strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func filesBySource(in store.Inputs) map[string][]catalog.ModelFile {
	files := map[string][]catalog.ModelFile{}
	for _, m := range in.Models {
		for _, q := range m.Quantizations {
			for _, src := range q.Sources {
				files[src.Type+" "+src.Repo] = src.Files
			}
		}
	}
	for _, d := range in.Drafts {
		files[d.Source.Type+" "+d.Source.Repo] = d.Source.Files
	}
	return files
}

// gatedErrors are a tokenless run's errors for a gated reference source.
func gatedErrors(owner, repo, rev, firstLFS string) []string {
	src := owner + " " + repo + "@" + rev + ": "
	return []string{
		src + "gated: token missing (huggingface.token_file): file " + firstLFS + ": LFS digest hidden",
		src + "GET /" + repo + "/resolve/" + rev + "/config.json: gated: token missing (huggingface.token_file): HTTP 401: Access to model " +
			repo + " is restricted. You must have access to it and be authenticated to access it. Please log in.",
	}
}

var curatedRunErrors = slices.Concat(
	gatedErrors("llama-3.1-8b/bf16", "meta-llama/Llama-3.1-8B-Instruct", "0e9e39f249a16976918f6564b8830bc894c89659", "model-00001-of-00004.safetensors"),
	gatedErrors("llama-3.1-70b/bf16", "meta-llama/Llama-3.1-70B-Instruct", "1605565b47bb9346c5515c34102e054115b4f98b", "model-00001-of-00030.safetensors"),
	gatedErrors("gemma-2-9b/bf16", "google/gemma-2-9b-it", "11c9b309abf73637e4b6f9a3fa1e92e615547819", "model-00001-of-00004.safetensors"),
)

// manifestSizes are file counts per source; none for gated sources read without
// a token.
var manifestSizes = map[string]int{
	"meta-llama/Llama-3.1-8B-Instruct": 0, "RedHatAI/Meta-Llama-3.1-8B-Instruct-FP8": 11, "bartowski/Meta-Llama-3.1-8B-Instruct-GGUF": 1,
	"meta-llama/Llama-3.1-70B-Instruct": 0, "RedHatAI/Meta-Llama-3.1-70B-Instruct-FP8": 24,
	"mistralai/Mixtral-8x7B-Instruct-v0.1": 28, "RedHatAI/Mixtral-8x7B-Instruct-v0.1-FP8": 20,
	"google/gemma-2-9b-it": 0, "RedHatAI/gemma-2-9b-it-FP8": 12,
	"Qwen/Qwen2.5-14B-Instruct": 18, "RedHatAI/Qwen2.5-14B-Instruct-FP8-dynamic": 15, "bartowski/Qwen2.5-14B-Instruct-GGUF": 1,
	"RedHatAI/Llama-3.1-8B-Instruct-speculator.eagle3": 9, "lmsys/sglang-EAGLE3-LLaMA3.1-Instruct-8B": 4,
}

// The committed catalog imports without conflicts; tokenless gated sources
// fail; a rerun changes nothing.
func TestImportCuratedCatalog(t *testing.T) {
	s, db := newStore(t, "hf_curated")
	in := committed(t)
	seed(t, s, model.SourceCuration, in)
	before := rowTimes(t, db)
	imp := testImporter(hub(t).URL, "")

	first, err := importers.Execute(ctx, s, imp, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if first.Importer != "huggingface" || first.Layer != model.LayerUpstream || first.Source != "huggingface" || first.Status != model.RunSucceeded ||
		!strings.HasPrefix(first.InputVersion, "sha256:") {
		t.Fatalf("run = %+v, want a succeeded huggingface run of the upstream layer", first)
	}
	sum, conflicts := summaryOf(t, first)
	diff(t, "conflicts", conflicts, []string{})
	diff(t, "errors", sum.Errors, curatedRunErrors)
	if sum.Sources == nil || *sum.Sources != (importers.SourceCounts{Read: 11, Failed: 3}) {
		t.Errorf("sources = %+v, want 11 read and the 3 gated failed", sum.Sources)
	}
	inserted := 0
	for _, n := range manifestSizes {
		inserted += n
	}
	if want := (store.Changes{"model_files": {Inserted: inserted - 2}}); !reflect.DeepEqual(sum.Changes, want) {
		t.Errorf("changes = %+v, want %+v (the gguf manifests are curated)", sum.Changes, want)
	}

	eff, err := s.Effective(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := catalog.Generate(eff.Families, eff.NodeTypes, eff.Models, eff.Drafts, eff.Backends); err != nil {
		t.Errorf("generate over the imported manifests: %v", err)
	}
	files := filesBySource(eff)
	for repo, n := range manifestSizes {
		got := files[catalog.SourceHuggingFace+" "+repo]
		if len(got) != n {
			t.Errorf("%s: %d files, want %d", repo, len(got), n)
		}
		for _, f := range got {
			if strings.HasSuffix(f.Path, ".pt") || strings.HasSuffix(f.Path, ".pth") {
				t.Errorf("%s: manifest holds %s, a checkpoint no server loads", repo, f.Path)
			}
		}
	}
	mixtral := files["huggingface mistralai/Mixtral-8x7B-Instruct-v0.1"]
	if len(mixtral) != 28 || mixtral[2] != (catalog.ModelFile{Path: "config.json", Size: 720, Digest: "git-oid:de132a80b25c9d12c674dbd84cedc1d6f35fb756"}) ||
		mixtral[4] != (catalog.ModelFile{Path: "model-00001-of-00019.safetensors", Size: 4892809584,
			Digest: "sha256:54669c5aec29fe5e4edd8098f7b564a137ba36be22ad25a194cd93f2bb54c940"}) {
		t.Errorf("mixtral manifest %+v, want its tree but the consolidated .pt checkpoints", mixtral)
	}
	if f := files["huggingface lmsys/sglang-EAGLE3-LLaMA3.1-Instruct-8B"]; !slices.ContainsFunc(f, func(f catalog.ModelFile) bool { return f.Path == "pytorch_model.bin" }) {
		t.Errorf("lmsys draft manifest %+v lacks its only weights, pytorch_model.bin", f)
	}
	for _, m := range eff.Models {
		for _, q := range m.Quantizations {
			for k := range q.Sources {
				if q.Sources[k].Type == catalog.SourceHuggingFace && q.Kind != "gguf" {
					q.Sources[k].Files = nil
				}
			}
		}
	}
	for i := range eff.Drafts {
		eff.Drafts[i].Source.Files = nil
	}
	if !reflect.DeepEqual(eff.Models, in.Models) || !reflect.DeepEqual(eff.Drafts, in.Drafts) {
		t.Error("the import changed curated values")
	}
	for _, slug := range []string{"qwen-2.5-14b", "llama-3.1-8b"} {
		p := dbtest.Provenance(t, s, store.KindModel, slug)
		for _, f := range []string{"context_max", "params_b", "architecture"} {
			if src := p[0].Fields[f].Source; src != model.SourceCuration {
				t.Errorf("%s %s provenance %s, want curation kept", slug, f, src)
			}
		}
	}
	var curatedGGUF int64
	if err := db.Model(&model.ModelFile{}).Where("source = ? AND path LIKE ?", model.SourceCuration, "%.gguf").Count(&curatedGGUF).Error; err != nil {
		t.Fatal(err)
	}
	if curatedGGUF != 2 {
		t.Errorf("%d curated gguf files, want both kept", curatedGGUF)
	}
	atFirst := rowTimes(t, db)
	for id, at := range before {
		if !atFirst[id].Equal(at) {
			t.Errorf("row %s outside the imported manifests changed", id)
		}
	}
	second, err := importers.Execute(ctx, s, imp, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	sum2, conflicts2 := summaryOf(t, second)
	if len(sum2.Changes) != 0 || second.InputVersion != first.InputVersion {
		t.Errorf("second run changed %+v at input %s, want nothing at %s", sum2.Changes, second.InputVersion, first.InputVersion)
	}
	diff(t, "second run conflicts", conflicts2, conflicts)
	diff(t, "second run errors", sum2.Errors, sum.Errors)
	if after := rowTimes(t, db); !reflect.DeepEqual(after, atFirst) {
		t.Error("second run changed rows")
	}
}

// nominal sets older curated values the Hub contradicts: rounded parameters and
// Qwen's YaRN context.
func nominal(in store.Inputs) store.Inputs {
	in.Models[2].ParamsB = 47
	qwen := &in.Models[4]
	qwen.ParamsB, qwen.ParamsActiveB, qwen.ContextMax = 14, 14, 131072
	return in
}

// Contradicted curated values are kept and listed as conflicts.
func TestImportKeepsCuratedValues(t *testing.T) {
	s, _ := newStore(t, "hf_keep")
	in := nominal(committed(t))
	seed(t, s, model.SourceCuration, in)
	run, err := importers.Execute(ctx, s, testImporter(hub(t).URL, ""), uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	sum, conflicts := summaryOf(t, run)
	diff(t, "conflicts", conflicts, []string{
		`{"kind":"model","key":"mixtral-8x7b","field":"params_b","curated":47,"imported":46.7}`,
		`{"kind":"model","key":"qwen-2.5-14b","field":"context_max","curated":131072,"imported":32768}`,
		`{"kind":"model","key":"qwen-2.5-14b","field":"params_active_b","curated":14,"imported":14.8}`,
		`{"kind":"model","key":"qwen-2.5-14b","field":"params_b","curated":14,"imported":14.8}`,
	})
	if _, ok := sum.Changes["models"]; ok {
		t.Errorf("changes %+v, want no model updated", sum.Changes)
	}
	eff, err := s.Effective(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if q := eff.Models[4]; q.ContextMax != 131072 || q.ParamsB != 14 || eff.Models[2].ParamsB != 47 {
		t.Errorf("qwen context %d params %v, mixtral params %v; want the curated values", q.ContextMax, q.ParamsB, eff.Models[2].ParamsB)
	}
	p := dbtest.Provenance(t, s, store.KindModel, "qwen-2.5-14b")
	for _, f := range []string{"context_max", "params_b", "params_active_b"} {
		if src := p[0].Fields[f].Source; src != model.SourceCuration {
			t.Errorf("qwen %s provenance %s, want curation kept", f, src)
		}
	}
}

// Over bootstrapped values the Hub's values replace them; agreeing values keep
// their provenance.
func TestImportFillsUncuratedValues(t *testing.T) {
	s, _ := newStore(t, "hf_fill")
	seed(t, s, "bootstrap-from-json", nominal(committed(t)))
	run, err := importers.Execute(ctx, s, testImporter(hub(t).URL, ""), uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	sum, conflicts := summaryOf(t, run)
	if len(conflicts) != 0 || sum.Changes["models"] != (store.Change{Updated: 2}) {
		t.Errorf("conflicts %v, changes %+v; want none and mixtral and qwen updated", conflicts, sum.Changes)
	}
	eff, err := s.Effective(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]catalog.ModelTemplate{}
	for _, m := range eff.Models {
		got[m.Slug] = m
	}
	qwen := got["qwen-2.5-14b"]
	if qwen.ContextMax != 32768 || qwen.ParamsB != 14.8 || qwen.ParamsActiveB != 14.8 ||
		qwen.Architecture != (catalog.Architecture{Layers: 48, AttentionHeads: 40, KVHeads: 8, HeadDim: 128}) {
		t.Errorf("qwen = context %d params %v/%v arch %+v, want the Hub's", qwen.ContextMax, qwen.ParamsB, qwen.ParamsActiveB, qwen.Architecture)
	}
	if m := got["mixtral-8x7b"]; m.ParamsB != 46.7 || m.ParamsActiveB != 12.9 {
		t.Errorf("mixtral params %v active %v, want 46.7 and 12.9", m.ParamsB, m.ParamsActiveB)
	}
	p := dbtest.Provenance(t, s, store.KindModel, "qwen-2.5-14b")
	if f := p[0].Fields["context_max"]; f.Source != "huggingface" || f.ImportRunID == nil || *f.ImportRunID != run.ID {
		t.Errorf("qwen context_max provenance %+v, want run %s", f, run.ID)
	}
	if f := p[0].Fields["license"]; f.Source != "bootstrap-from-json" {
		t.Errorf("qwen license provenance %+v, want the unchanged value's", f)
	}
	if f := dbtest.Provenance(t, s, store.KindModel, "llama-3.1-8b")[0].Fields["architecture"]; f.Source != "bootstrap-from-json" {
		t.Errorf("llama-3.1-8b architecture provenance %+v, want untouched: its config is gated", f)
	}
}

func TestMissingRevisionFailsTheSource(t *testing.T) {
	s, _ := newStore(t, "hf_missing")
	in := committed(t)
	qwen := &in.Models[4].Quantizations[0].Sources[0]
	qwen.Revision = strings.Repeat("f", 40)
	seed(t, s, model.SourceCuration, in)
	run, err := importers.Execute(ctx, s, testImporter(hub(t).URL, ""), uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	sum, conflicts := summaryOf(t, run)
	if sum.Sources == nil || *sum.Sources != (importers.SourceCounts{Read: 10, Failed: 4}) {
		t.Errorf("sources = %+v, want 10 read, 4 failed", sum.Sources)
	}
	missing := "qwen-2.5-14b/bf16 Qwen/Qwen2.5-14B-Instruct@" + qwen.Revision + ": GET /api/models/Qwen/Qwen2.5-14B-Instruct/revision/" +
		qwen.Revision + ": HTTP 404: Invalid rev id: " + qwen.Revision
	diff(t, "errors", sum.Errors, slices.Insert(slices.Clone(curatedRunErrors), 6, missing))
	if slices.ContainsFunc(conflicts, func(c string) bool { return strings.Contains(c, "qwen") }) {
		t.Errorf("conflicts %v: the model's facts come from the missing revision", conflicts)
	}
	eff, err := s.Effective(ctx)
	if err != nil {
		t.Fatal(err)
	}
	files := filesBySource(eff)
	if len(files["huggingface Qwen/Qwen2.5-14B-Instruct"]) != 0 || len(files["huggingface RedHatAI/Qwen2.5-14B-Instruct-FP8-dynamic"]) != 15 {
		t.Errorf("qwen manifests bf16 %d fp8 %d, want 0 and 15", len(files["huggingface Qwen/Qwen2.5-14B-Instruct"]),
			len(files["huggingface RedHatAI/Qwen2.5-14B-Instruct-FP8-dynamic"]))
	}
}

func TestUnreachableHubFailsTheRun(t *testing.T) {
	s, db := newStore(t, "hf_down")
	seed(t, s, model.SourceCuration, committed(t))
	before := rowTimes(t, db)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	imp := testImporter(srv.URL, "")
	imp.getter.Retries = 1
	run, err := importers.Execute(ctx, s, imp, uuid.New())
	if err == nil || run.Status != model.RunFailed || !strings.Contains(run.Error, "no source could be read") || !strings.Contains(run.Error, "HTTP 503") {
		t.Errorf("run %+v, err %v; want failed: no source could be read", run, err)
	}
	if n := calls.Load(); n != 2*14 {
		t.Errorf("%d requests, want 2 per source (one retry) for 14 sources", n)
	}
	if sum, _ := summaryOf(t, run); sum.Sources == nil || *sum.Sources != (importers.SourceCounts{Failed: 14}) {
		t.Errorf("sources = %+v, want all 14 failed", sum.Sources)
	}
	if after := rowTimes(t, db); !reflect.DeepEqual(after, before) {
		t.Error("failed run changed rows")
	}
}

func TestCancelledRunIsFailed(t *testing.T) {
	s, db := newStore(t, "hf_cancel")
	seed(t, s, model.SourceCuration, committed(t))
	before := rowTimes(t, db)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cancel()
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	run, err := importers.Execute(runCtx, s, testImporter(srv.URL, ""), uuid.New())
	if !errors.Is(err, context.Canceled) || run.Status != model.RunFailed || run.FinishedAt == nil {
		t.Errorf("run %+v, err %v; want failed by cancellation", run, err)
	}
	if after := rowTimes(t, db); !reflect.DeepEqual(after, before) {
		t.Error("cancelled run changed rows")
	}
}

// Mixtral's active parameters are 12.9 B of 46.7 B.
func TestMixtralActiveParameters(t *testing.T) {
	c := &client{Importer: testImporter(hub(t).URL, "")}
	r := c.read(ctx, job{src: catalog.ModelSource{Type: catalog.SourceHuggingFace, Repo: "mistralai/Mixtral-8x7B-Instruct-v0.1",
		Revision: "eba92302a2861cdc0098cc54bc9f17cb2c47eb61"}, ref: true})
	if len(r.errs) > 0 || r.config == nil || r.info == nil || r.info.Safetensors == nil {
		t.Fatalf("read: %v", r.errs)
	}
	active := float64(r.active) / 1e9
	if math.Abs(active-12.9) > 0.1 {
		t.Errorf("active parameters %.3f B, want within 0.1 of 12.9", active)
	}
	if v := modelValues(r)["params_active_b"]; v != 12.9 {
		t.Errorf("params_active_b %v, want 12.9", v)
	}
}

// Requests are spaced; 429 and 5xx are retried after Retry-After, other errors
// are not.
func TestRequestsAreSpacedAndRetried(t *testing.T) {
	var calls atomic.Int32
	statuses := []int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(calls.Add(1)) - 1
		code := http.StatusOK
		if n < len(statuses) {
			code = statuses[n]
		}
		if code == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "1")
		}
		w.WriteHeader(code)
		w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	get := func(i *Importer) (time.Duration, error) {
		calls.Store(0)
		start := time.Now()
		_, _, err := (&client{Importer: i}).get(ctx, srv.URL+"/x")
		return time.Since(start), err
	}

	statuses = []int{http.StatusTooManyRequests, http.StatusBadGateway}
	took, err := get(testImporter(srv.URL, ""))
	if err != nil || calls.Load() != 3 || took < time.Second {
		t.Errorf("429 then 502: %d requests in %s, err %v; want 3, the first retry after Retry-After 1s", calls.Load(), took, err)
	}
	statuses = []int{503, 503, 503, 503, 503}
	if _, err := get(testImporter(srv.URL, "")); err == nil || !strings.Contains(err.Error(), "HTTP 503") || calls.Load() != 4 {
		t.Errorf("persistent 503: %d requests, err %v; want 4 and the status", calls.Load(), err)
	}
	statuses = []int{http.StatusNotFound}
	if _, err := get(testImporter(srv.URL, "")); err == nil || calls.Load() != 1 {
		t.Errorf("404: %d requests, err %v; want 1 and an error", calls.Load(), err)
	}

	statuses = nil
	spaced := testImporter(srv.URL, "")
	spaced.interval = 100 * time.Millisecond
	c := &client{Importer: spaced}
	start := time.Now()
	for range 3 {
		if _, _, err := c.get(ctx, srv.URL+"/x"); err != nil {
			t.Fatal(err)
		}
	}
	if took := time.Since(start); took < 200*time.Millisecond {
		t.Errorf("3 requests in %s, want spaced 100ms apart", took)
	}
}

// A gated refusal says whether the token is missing or rejected, and never
// leaks it.
func TestGatedRefusals(t *testing.T) {
	var auth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth.Store(r.Header.Get("Authorization"))
		if r.Header.Get("Authorization") == "" {
			http.Error(w, "Access to model o/m is restricted.", http.StatusUnauthorized)
			return
		}
		http.Error(w, "Access to model o/m is restricted and you are not in the authorized list.", http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	_, _, err := (&client{Importer: testImporter(srv.URL, "")}).get(ctx, srv.URL+"/o/m/resolve/r/config.json")
	if err == nil || !strings.Contains(err.Error(), "gated: token missing (huggingface.token_file): HTTP 401") || auth.Load() != "" {
		t.Errorf("without a token: err %v, authorization %q", err, auth.Load())
	}
	_, _, err = (&client{Importer: testImporter(srv.URL, "hf_s3cret")}).get(ctx, srv.URL+"/o/m/resolve/r/config.json")
	if err == nil || !strings.Contains(err.Error(), "gated: token rejected or licence not accepted: HTTP 403") ||
		strings.Contains(err.Error(), "s3cret") || auth.Load() != "Bearer hf_s3cret" {
		t.Errorf("with a token: err %v, authorization %q", err, auth.Load())
	}
}

func TestTreeFollowsNextPage(t *testing.T) {
	var next string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cursor") == "" {
			w.Header().Set("Link", "<"+next+">; rel=\"next\"")
			w.Write([]byte(`[{"type":"file","path":"a.json","size":1,"oid":"` + strings.Repeat("a", 40) + `"}]`))
			return
		}
		w.Write([]byte(`[{"type":"file","path":"b.json","size":2,"oid":"` + strings.Repeat("b", 40) + `"}]`))
	}))
	t.Cleanup(srv.Close)
	c := &client{Importer: testImporter(srv.URL, "")}
	next = srv.URL + "/api/models/o/m/tree/r?recursive=1&cursor=x"
	tree, err := c.tree(ctx, srv.URL+"/api/models/o/m/tree/r?recursive=1")
	if err != nil || len(tree) != 2 || tree[1].Path != "b.json" {
		t.Errorf("tree %+v, err %v; want both pages", tree, err)
	}
	var elsewhere atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { elsewhere.Add(1) }))
	t.Cleanup(other.Close)
	next = other.URL + "/api/models/o/m/tree/r?recursive=1&cursor=x"
	if _, err := c.tree(ctx, srv.URL+"/api/models/o/m/tree/r?recursive=1"); err == nil || elsewhere.Load() != 0 {
		t.Errorf("next page on another host: err %v, %d requests there; want refused", err, elsewhere.Load())
	}
}

// The input version ignores download counts and follows the licence.
func TestInputVersionFollowsTheData(t *testing.T) {
	s, _ := newStore(t, "hf_version")
	seed(t, s, model.SourceCuration, committed(t))
	const qwenInfo = "/api/models/Qwen/Qwen2.5-14B-Instruct/revision/cf98f3b3bbb457ad9e2bb7baf9a0125b6b88caa8"
	version := func(rewrite func(string, []byte) []byte) string {
		t.Helper()
		run, err := importers.Execute(ctx, s, testImporter(rewritingHub(t, rewrite).URL, ""), uuid.New())
		if err != nil {
			t.Fatal(err)
		}
		return run.InputVersion
	}
	replace := func(old, new string) func(string, []byte) []byte {
		return func(path string, body []byte) []byte {
			if path == qwenInfo {
				if !strings.Contains(string(body), old) {
					t.Fatalf("qwen info lacks %s", old)
				}
				return []byte(strings.Replace(string(body), old, new, 1))
			}
			return body
		}
	}
	recorded := version(nil)
	if v := version(replace(`"downloads":`, `"downloads":1`)); v != recorded {
		t.Errorf("download count changed the input version: %s, recorded %s", v, recorded)
	}
	if v := version(replace(`"license":"apache-2.0"`, `"license":"mit"`)); v == recorded {
		t.Error("a licence change kept the input version")
	}
}

func TestRetryAfterIsCapped(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)
	start := time.Now()
	_, _, err := (&client{Importer: testImporter(srv.URL, "")}).get(ctx, srv.URL+"/x")
	if err == nil || !strings.Contains(err.Error(), "Retry-After 1h0m0s exceeds 5m0s") || calls.Load() != 1 || time.Since(start) > time.Second {
		t.Errorf("err %v after %d requests in %s; want an immediate error naming the Retry-After", err, calls.Load(), time.Since(start))
	}
}

func TestManifestSkipsEmptyFiles(t *testing.T) {
	oid := strings.Repeat("a", 40)
	files, weights, err := manifest([]entry{
		{Type: "file", Path: ".gitkeep", OID: oid},
		{Type: "file", Path: "config.json", Size: 2, OID: oid},
		{Type: "file", Path: "model.safetensors", Size: 8, OID: oid, LFS: &struct {
			OID string `json:"oid"`
		}{strings.Repeat("b", 64)}},
	}, false, nil)
	if err != nil || weights != 8 || len(files) != 2 || files[0].Path != "config.json" {
		t.Errorf("manifest %+v weights %d, err %v; want config.json and the weights", files, weights, err)
	}
}

// Active parameters import for dense and Mixtral-style models only; others are
// listed.
func TestActiveParametersOfKnownLayoutsOnly(t *testing.T) {
	cases := map[string]struct {
		config string
		active int64
		err    string
	}{
		"dense": {`{"architectures":["Qwen2ForCausalLM"]}`, 1000, ""},
		"mixtral": {`{"architectures":["MixtralForCausalLM"],"num_local_experts":8,"num_experts_per_tok":2,"output_router_logits":false}`,
			1000 - 6*2*3*4*5, ""},
		"qwen moe": {`{"architectures":["Qwen2MoeForCausalLM"],"num_experts":60,"num_experts_per_tok":4,"moe_intermediate_size":1408}`, 0,
			"mixture of experts [Qwen2MoeForCausalLM moe_intermediate_size num_experts num_experts_per_tok] not recognised"},
		"shared experts": {`{"architectures":["DeepseekV3ForCausalLM"],"n_routed_experts":256,"n_shared_experts":1,"num_experts_per_tok":8}`, 0,
			"mixture of experts [n_routed_experts n_shared_experts num_experts_per_tok] not recognised"},
	}
	for name, c := range cases {
		body := strings.Replace(c.config, "{", `{"num_hidden_layers":2,"num_attention_heads":2,"hidden_size":4,"intermediate_size":5,"max_position_embeddings":8,`, 1)
		cfg, err := parseConfig([]byte(body))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		active, err := cfg.active(1000)
		if active != c.active || c.err == "" && err != nil || c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)) {
			t.Errorf("%s: active %d, err %v; want %d, %q", name, active, err, c.active, c.err)
		}
	}

	srv := rewritingHub(t, func(path string, body []byte) []byte {
		if strings.HasSuffix(path, "/config.json") {
			return []byte(strings.Replace(string(body), "{", `{"n_shared_experts":1,`, 1))
		}
		return body
	})
	r := (&client{Importer: testImporter(srv.URL, "")}).read(ctx, job{src: catalog.ModelSource{Type: catalog.SourceHuggingFace,
		Repo: "mistralai/Mixtral-8x7B-Instruct-v0.1", Revision: "eba92302a2861cdc0098cc54bc9f17cb2c47eb61"}, ref: true})
	cols := modelValues(r)
	if _, ok := cols["params_active_b"]; ok || cols["params_b"] != 46.7 || len(r.errs) != 1 || !strings.Contains(r.errs[0].Error(), "not recognised") {
		t.Errorf("columns %v, errors %v; want params_b without params_active_b and the layout listed", cols, r.errs)
	}
}
