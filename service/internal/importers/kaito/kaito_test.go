// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

package kaito

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
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

const (
	dataDir       = "../../../../data"
	fixture       = "../../../testdata/kaito/model_catalog.yaml"
	fixtureSHA256 = "f726754e71c93d1e2c1a2c652c9101b5fb952d71080e3f4056baf1e09d6931f2"
	llama         = "meta-llama/Llama-3.1-8B-Instruct"
	// recordedVersion changes only with the compared facts.
	recordedVersion = "sha256:b30a1bc2c5f5b230ef56d748ff74e5c6f7a623213c1481baa1bb05d9dcee99d7"
)

var ctx = context.Background()

func recorded(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if h := sha256.Sum256(b); hex.EncodeToString(h[:]) != fixtureSHA256 {
		t.Fatalf("%s is not the recorded file: sha256 %x", fixture, h)
	}
	return string(b)
}

func edit(t *testing.T, yaml, repo, old, new string) string {
	t.Helper()
	start := strings.Index(yaml, "- name: "+repo+"\n")
	if start < 0 {
		t.Fatalf("no entry %s", repo)
	}
	end := len(yaml)
	if n := strings.Index(yaml[start+1:], "\n- name: "); n >= 0 {
		end = start + 1 + n
	}
	e := yaml[start:end]
	if !strings.Contains(e, old) {
		t.Fatalf("entry %s lacks %q", repo, old)
	}
	return yaml[:start] + strings.Replace(e, old, new, 1) + yaml[end:]
}

func testImporter(t *testing.T, catalog string) *Importer {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/model_catalog.yaml" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(catalog))
	}))
	t.Cleanup(srv.Close)
	i := New(srv.URL + "/model_catalog.yaml")
	i.getter.Backoff = time.Millisecond
	return i
}

func bootstrapped(t *testing.T, suffix string) (*store.Store, *gorm.DB) {
	t.Helper()
	sqlDB, db := dbtest.Fresh(t, suffix)
	dbtest.Migrate(t, sqlDB)
	s := store.New(db)
	boot, err := bootstrap.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := importers.Execute(ctx, s, boot, uuid.New()); err != nil {
		t.Fatal(err)
	}
	return s, db
}

func rows(t *testing.T, db *gorm.DB) map[string]string {
	t.Helper()
	var tables []string
	if err := db.Raw(`SELECT table_name FROM information_schema.tables
		WHERE table_schema = 'public' AND table_type = 'BASE TABLE' AND table_name <> 'import_runs'`).Scan(&tables).Error; err != nil {
		t.Fatal(err)
	}
	all := map[string]string{}
	for _, table := range tables {
		var rs string
		if err := db.Raw(`SELECT coalesce(jsonb_agg(to_jsonb(t) ORDER BY to_jsonb(t)::text), '[]')::text FROM "` + table + `" t`).Scan(&rs).Error; err != nil {
			t.Fatal(err)
		}
		all[table] = rs
	}
	return all
}

func summaryOf(t *testing.T, run model.ImportRun) importers.Summary {
	t.Helper()
	var s importers.Summary
	if err := json.Unmarshal(run.Summary, &s); err != nil {
		t.Fatalf("summary %s: %v", run.Summary, err)
	}
	return s
}

func conflictsOf(t *testing.T, s importers.Summary) string {
	t.Helper()
	b, err := json.Marshal(s.Conflicts)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The pinned KAITO catalog agrees with the committed one; a rerun reads the
// same input.
func TestRecordedCatalogAgrees(t *testing.T) {
	s, db := bootstrapped(t, "kaito_recorded")
	before := rows(t, db)
	imp := testImporter(t, recorded(t))
	first, err := importers.Execute(ctx, s, imp, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if first.Importer != "kaito" || first.Layer != model.LayerUpstream || first.Source != "kaito" || first.SourceURL != imp.url ||
		first.Status != model.RunSucceeded || first.InputVersion != recordedVersion {
		t.Fatalf("run = %+v, want a succeeded kaito run", first)
	}
	sum := summaryOf(t, first)
	if len(sum.Changes) != 0 || len(sum.Conflicts) != 0 || len(sum.Errors) != 0 || sum.Sources == nil || *sum.Sources != (importers.SourceCounts{Read: 1}) {
		t.Errorf("summary %s, want llama-3.1-8b compared and nothing else", first.Summary)
	}
	var raw struct{ Changes json.RawMessage }
	if err := json.Unmarshal(first.Summary, &raw); err != nil || string(raw.Changes) != "{}" {
		t.Errorf("summary %s, want empty changes", first.Summary)
	}
	second, err := importers.Execute(ctx, s, imp, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if second.InputVersion != first.InputVersion || !reflect.DeepEqual(summaryOf(t, second), sum) {
		t.Errorf("second run %s at %s, want %s at %s", second.Summary, second.InputVersion, first.Summary, first.InputVersion)
	}
	if !reflect.DeepEqual(rows(t, db), before) {
		t.Error("the runs changed rows")
	}
}

// Disagreements are conflicts, never written; an entry lacking a value is an
// error.
func TestDisagreementsAreReportedNotWritten(t *testing.T) {
	const gemma = `- name: google/gemma-2-9b-it
  modelTokenLimit: 8192
  hiddenSize: 3584
  numHiddenLayers: 42
  numAttentionHeads: 16
  numKeyValueHeads: 8
`
	llamaArch := `{"attention_heads":32,"head_dim":128,"kv_heads":8,"layers":32}`
	for name, tc := range map[string]struct {
		catalog   func(string) string
		conflicts string
		errors    []string
		sources   importers.SourceCounts
	}{
		"kv heads": {
			func(y string) string { return edit(t, y, llama, "numKeyValueHeads: 8", "numKeyValueHeads: 4") },
			`[{"kind":"model","key":"llama-3.1-8b","field":"architecture","curated":` + llamaArch +
				`,"imported":{"attention_heads":32,"head_dim":128,"kv_heads":4,"layers":32}}]`,
			nil, importers.SourceCounts{Read: 1},
		},
		"token limit": {
			func(y string) string { return edit(t, y, llama, "modelTokenLimit: 131072", "modelTokenLimit: 65536") },
			`[{"kind":"model","key":"llama-3.1-8b","field":"context_max","curated":131072,"imported":65536}]`,
			nil, importers.SourceCounts{Read: 1},
		},
		"explicit head size": {
			func(y string) string { return y + gemma + "  headDim: 256\n" },
			`null`, nil, importers.SourceCounts{Read: 2},
		},
		"derived head size, catalog order, case-insensitive repo": {
			func(y string) string {
				return y + gemma + `- name: mistralai/mixtral-8x7b-instruct-v0.1
  modelTokenLimit: 32768
  hiddenSize: 4096
  numHiddenLayers: 32
  numAttentionHeads: 32
  numKeyValueHeads: 0
`
			},
			`[{"kind":"model","key":"mixtral-8x7b","field":"architecture","curated":` + llamaArch +
				`,"imported":{"attention_heads":32,"head_dim":128,"kv_heads":32,"layers":32}},` +
				`{"kind":"model","key":"gemma-2-9b","field":"architecture","curated":{"attention_heads":16,"head_dim":256,"kv_heads":8,"layers":42}` +
				`,"imported":{"attention_heads":16,"head_dim":224,"kv_heads":8,"layers":42}}]`,
			nil, importers.SourceCounts{Read: 3},
		},
		"incomplete entry": {
			func(y string) string {
				return y + "- name: Qwen/Qwen2.5-14B-Instruct\n  modelTokenLimit: 131072\n  hiddenSize: 5120\n  numAttentionHeads: 40\n"
			},
			`[{"kind":"model","key":"qwen-2.5-14b","field":"context_max","curated":32768,"imported":131072}]`,
			[]string{"qwen-2.5-14b: KAITO's Qwen/Qwen2.5-14B-Instruct lacks the layers, attention heads or hidden size"},
			importers.SourceCounts{Read: 1, Failed: 1},
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, db := bootstrapped(t, "kaito_disagree")
			before := rows(t, db)
			run, err := importers.Execute(ctx, s, testImporter(t, tc.catalog(recorded(t))), uuid.New())
			if err != nil {
				t.Fatal(err)
			}
			sum := summaryOf(t, run)
			if got := conflictsOf(t, sum); got != tc.conflicts {
				t.Errorf("conflicts\n%s\nwant\n%s", got, tc.conflicts)
			}
			if !reflect.DeepEqual(sum.Errors, tc.errors) || sum.Sources == nil || *sum.Sources != tc.sources || len(sum.Changes) != 0 {
				t.Errorf("summary %s, want errors %q, sources %+v, no changes", run.Summary, tc.errors, tc.sources)
			}
			if run.Status != model.RunSucceeded {
				t.Errorf("run %s: %s", run.Status, run.Error)
			}
			if !reflect.DeepEqual(rows(t, db), before) {
				t.Error("the run changed rows")
			}
		})
	}
}

// The input version changes only with compared entries.
func TestInputVersionFollowsTheData(t *testing.T) {
	s, _ := bootstrapped(t, "kaito_version")
	version := func(catalog string) string {
		t.Helper()
		run, err := importers.Execute(ctx, s, testImporter(t, catalog), uuid.New())
		if err != nil {
			t.Fatal(err)
		}
		return run.InputVersion
	}
	y := recorded(t)
	at := version(y)
	if v := version(edit(t, y, "microsoft/phi-4", "modelTokenLimit: 16384", "modelTokenLimit: 32768")); v != at {
		t.Errorf("an entry of another model changed the input version: %s, recorded %s", v, at)
	}
	if v := version(edit(t, y, llama, "numHiddenLayers: 32", "numHiddenLayers: 33")); v == at {
		t.Error("a changed llama layer count kept the input version")
	}
}

func TestUnreadableCatalogFailsTheRun(t *testing.T) {
	s, db := bootstrapped(t, "kaito_fail")
	before := rows(t, db)
	missing := testImporter(t, "")
	missing.url += ".missing"
	for name, tc := range map[string]struct {
		imp  *Importer
		want string
	}{
		"not found":    {missing, "404"},
		"not yaml":     {testImporter(t, "models: [\n"), "yaml"},
		"no model":     {testImporter(t, "# empty\nmodels: []\n"), "lists no model"},
		"other format": {testImporter(t, "- name: x\n"), "yaml"},
	} {
		run, err := importers.Execute(ctx, s, tc.imp, uuid.New())
		if err == nil || run.Status != model.RunFailed || !strings.Contains(run.Error, tc.want) {
			t.Errorf("%s: run %s %q (%v), want failed with %q", name, run.Status, run.Error, err, tc.want)
		}
	}
	if !reflect.DeepEqual(rows(t, db), before) {
		t.Error("failed runs changed rows")
	}
}
