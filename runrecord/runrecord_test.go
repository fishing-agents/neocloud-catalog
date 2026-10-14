// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

package runrecord

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func valid() Record {
	tps := 812.5
	return Record{
		Schema: SchemaID, RecordID: "0f8fad5b-d9cb-469f-a165-70867728950e", InstallID: "7c9e6679-7425-40de-944b-e07fc1f90ae7",
		Platform: "gpustack", PlatformVersion: "v2.0.0",
		ObservedFrom: time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC), ObservedTo: time.Date(2026, 10, 7, 10, 5, 0, 0, time.UTC),
		Model: Model{Catalog: &CatalogRef{Slug: "llama-3.1-8b", Preset: "sglang-bf16", Version: "2026-10-05T14:22:45Z"},
			Source: "huggingface", Repo: "meta-llama/Llama-3.1-8B-Instruct", Quantization: "bf16", ParamsB: 8, WeightsGB: 16.1},
		Config: Config{Backend: "sglang", BackendVersion: "v0.5.21", Args: []string{"--context-length=8192", "--enforce-eager"},
			TensorParallel: 1, MaxContext: 8192, GPUModel: "H100", GPUCount: 1, GPUMemoryGB: 80, Nodes: 1},
		Outcome:  Outcome{Status: StatusRan, OutputTokensPerSec: &tps},
		Workload: Workload{Source: SourceBenchmarkStandard, InputTokens: 512, OutputTokens: 128, Concurrency: 4},
	}
}

func TestValidRecordRoundTrips(t *testing.T) {
	b, err := json.Marshal(valid())
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(strings.NewReader(string(b)))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reflect.DeepEqual(got, valid()) {
		t.Fatalf("round trip changed the record:\n got %+v\nwant %+v", got, valid())
	}
}

// Fields outside the contract (host or tenant data) are refused at any depth.
func TestDecodeRefusesForbiddenFields(t *testing.T) {
	b, _ := json.Marshal(valid())
	for _, c := range []struct{ name, path, field string }{
		{"hostname", "", "hostname"},
		{"namespace", "config", "namespace"},
		{"prompt", "workload", "prompt"},
		{"tenant", "model", "tenant_id"},
	} {
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		target := m
		if c.path != "" {
			target = m[c.path].(map[string]any)
		}
		target[c.field] = "x"
		raw, _ := json.Marshal(m)
		if _, err := Decode(strings.NewReader(string(raw))); err == nil || !strings.Contains(err.Error(), "unknown field") {
			t.Errorf("%s: decode err = %v, want an unknown-field refusal", c.name, err)
		}
	}
}

func TestValidateRefuses(t *testing.T) {
	for name, mutate := range map[string]func(*Record){
		"unlisted flag":         func(r *Record) { r.Config.Args = []string{"--api-key=secret"} },
		"path value":            func(r *Record) { r.Config.Args = []string{"--max-model-len=/etc/passwd"} },
		"switch with value":     func(r *Record) { r.Config.Args = []string{"--enforce-eager=1"} },
		"value flag bare":       func(r *Record) { r.Config.Args = []string{"--max-model-len"} },
		"failed without class":  func(r *Record) { r.Outcome = Outcome{Status: StatusFailed} },
		"ran with failure":      func(r *Record) { r.Outcome.Failure = "oom" },
		"private repo path":     func(r *Record) { r.Model.Repo = "/models/internal" },
		"unknown backend":       func(r *Record) { r.Config.Backend = "tgi" },
		"reversed window":       func(r *Record) { r.ObservedTo = r.ObservedFrom.Add(-time.Second) },
		"benchmark sans levels": func(r *Record) { r.Workload.Concurrency = 0 },
		"long reason": func(r *Record) {
			r.Outcome = Outcome{Status: StatusFailed, Failure: "crash", Reason: strings.Repeat("x", maxReason+1)}
		},
	} {
		r := valid()
		mutate(&r)
		if err := r.Validate(); err == nil {
			t.Errorf("%s: Validate accepted it", name)
		}
	}
	live := valid()
	live.Workload = Workload{Source: SourceLive, InputTokens: 900, OutputTokens: 200, Requests: 1200}
	if err := live.Validate(); err != nil {
		t.Errorf("live window without levels: %v", err)
	}
}

func TestFilterArgs(t *testing.T) {
	in := []string{
		"--model", "/models/llama", "--served-model-name", "llama", "--tensor-parallel-size", "2",
		"--max-model-len=8192", "--enforce-eager", "positional", "--kv-cache-dtype", "fp8",
		"--speculative-algorithm", `{"model":"/x"}`, "--api-key", "sk-1", "--host", "0.0.0.0",
	}
	want := []string{"--tensor-parallel-size=2", "--max-model-len=8192", "--enforce-eager", "--kv-cache-dtype=fp8"}
	if got := FilterArgs(in); !slices.Equal(got, want) {
		t.Fatalf("FilterArgs = %v, want %v", got, want)
	}
	r := valid()
	r.Config.Args = FilterArgs(in)
	if err := r.Validate(); err != nil {
		t.Fatalf("filtered args do not validate: %v", err)
	}
}

// Every schema object's properties are the struct's json names and its required
// list is exactly the fields without omitempty.
func TestSchemaMatchesTypes(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal(JSONSchema, &schema); err != nil {
		t.Fatal(err)
	}
	compare(t, "record", schema, reflect.TypeFor[Record]())
}

func compare(t *testing.T, path string, schema map[string]any, typ reflect.Type) {
	t.Helper()
	props, _ := schema["properties"].(map[string]any)
	if schema["additionalProperties"] != false {
		t.Errorf("%s: schema allows additional properties", path)
	}
	var names, required []string
	for f := range typ.Fields() {
		name, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
		names = append(names, name)
		if opts != "omitempty" {
			required = append(required, name)
		}
		ft := f.Type
		if ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct && ft != reflect.TypeFor[time.Time]() {
			sub, _ := props[name].(map[string]any)
			compare(t, path+"."+name, sub, ft)
		}
	}
	var schemaNames, schemaRequired []string
	for k := range props {
		schemaNames = append(schemaNames, k)
	}
	for _, r := range schema["required"].([]any) {
		schemaRequired = append(schemaRequired, r.(string))
	}
	slices.Sort(names)
	slices.Sort(schemaNames)
	slices.Sort(required)
	slices.Sort(schemaRequired)
	if !slices.Equal(names, schemaNames) {
		t.Errorf("%s: Go fields %v, schema properties %v", path, names, schemaNames)
	}
	if !slices.Equal(required, schemaRequired) {
		t.Errorf("%s: Go required %v, schema required %v", path, required, schemaRequired)
	}
}
