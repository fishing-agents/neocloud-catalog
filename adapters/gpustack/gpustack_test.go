package gpustack

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	catalog "github.com/fishman/neocloud-catalog"
	"github.com/fishman/neocloud-catalog/adapters"
)

var update = flag.Bool("update", false, "rewrite testdata from the committed snapshot")

// export runs the registered adapter and checks coverage.
func export(t *testing.T, snap catalog.CatalogSnapshot) (adapters.Export, document) {
	t.Helper()
	a, ok := adapters.Get("gpustack")
	if !ok {
		t.Fatal("gpustack adapter not registered")
	}
	e, err := a.Export(snap, Format, adapters.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := adapters.Coverage(a, snap, Format, e); err != nil {
		t.Fatal(err)
	}
	var doc document
	if err := json.Unmarshal(e.Artifacts[0].Body, &doc); err != nil {
		t.Fatal(err)
	}
	return e, doc
}

func committed(t *testing.T) catalog.CatalogSnapshot {
	t.Helper()
	snap, err := catalog.SnapshotV3()
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func TestGolden(t *testing.T) {
	e, _ := export(t, committed(t))
	for _, art := range e.Artifacts {
		path := filepath.Join("testdata", Format, art.Path)
		if *update {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, art.Body, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(art.Body, want) {
			t.Errorf("%s differs from the export; rerun with -update and review the diff", path)
		}
	}
}

func TestCommittedPresets(t *testing.T) {
	snap := committed(t)
	e, doc := export(t, snap)
	if len(e.Skipped) > 0 {
		t.Errorf("committed snapshot skips %v", e.Skipped)
	}
	wrongFlag := map[string]string{"vllm": "--context-length", "sglang": "--max-model-len"}
	for _, r := range adapters.Presets(snap) {
		set := doc.ModelSets[slices.IndexFunc(doc.ModelSets, func(s modelSet) bool { return s.Name == r.Model.Name })]
		i := slices.IndexFunc(set.Specs, func(s spec) bool { return s.Mode == r.Preset.Name })
		if i < 0 {
			t.Errorf("%v: no spec with mode %q", r.Key(), r.Preset.Name)
			continue
		}
		params := set.Specs[i].BackendParameters
		want, _ := adapters.ContextArg(r.Preset.Backend, r.Model.DefaultContext)
		if params[len(params)-1] != want {
			t.Errorf("%v: context = %q, want %q", r.Key(), params[len(params)-1], want)
		}
		if slices.ContainsFunc(params, func(p string) bool { return strings.HasPrefix(p, wrongFlag[r.Preset.Backend]) }) {
			t.Errorf("%v: %s carries the other backend's context flag: %v", r.Key(), r.Preset.Backend, params)
		}
	}
	// GPUStack takes a repository's defaults from its last spec; that must be
	// the vLLM one, its default backend.
	for _, set := range doc.ModelSets {
		last := map[string]string{}
		for _, s := range set.Specs {
			last[s.HuggingFaceRepoID+s.ModelScopeModelID] = s.Backend
		}
		for repo, backend := range last {
			if backend != "vLLM" {
				t.Errorf("%s: last spec of %s is %s, want vLLM", set.Name, repo, backend)
			}
		}
	}
}

func TestSkips(t *testing.T) {
	snap := catalog.CatalogSnapshot{
		Backends: []catalog.Backend{{Name: "vllm", Versions: []catalog.BackendVersion{{Version: "v1", Image: "vllm/vllm-openai:v1", Vendors: []string{"nvidia"}}}}},
		Models: []catalog.ModelTemplate{
			{
				Slug: "m", Name: "M", DefaultContext: 4096, ContextMax: 8192,
				Quantizations: []catalog.Quantization{
					{Name: "bf16", Sources: []catalog.ModelSource{{Type: catalog.SourceOCI, Repo: "reg/m"}, {Type: catalog.SourceModelScope, Repo: "ms/m"}}},
					{Name: "int4", Sources: []catalog.ModelSource{{Type: catalog.SourceOCI, Repo: "reg/m-int4"}}},
				},
				Presets: []catalog.Preset{
					{Name: "vllm-bf16", Quantization: "bf16", Backend: "vllm", BackendVersion: "v1", Vendors: []string{"amd"}},
					{Name: "vllm-int4", Quantization: "int4", Backend: "vllm", BackendVersion: "v1"},
					{Name: "tgi-bf16", Quantization: "bf16", Backend: "tgi", BackendVersion: "v1"},
				},
			},
			{
				Slug: "n", Name: "N", DefaultContext: 4096, ContextMax: 4096,
				Quantizations: []catalog.Quantization{{Name: "bf16", Sources: []catalog.ModelSource{{Type: catalog.SourceHuggingFace, Repo: "o/n"}}}},
				Presets:       []catalog.Preset{{Name: "tgi-bf16", Quantization: "bf16", Backend: "tgi", BackendVersion: "v1"}},
			},
		},
	}
	e, doc := export(t, snap)
	wantSkips := []adapters.Skip{
		{PresetKey: adapters.PresetKey{Model: "m", Preset: "vllm-int4"}, Reason: SkipNoSource},
		{PresetKey: adapters.PresetKey{Model: "m", Preset: "tgi-bf16"}, Reason: SkipBackend},
		{PresetKey: adapters.PresetKey{Model: "n", Preset: "tgi-bf16"}, Reason: SkipBackend},
	}
	if !slices.Equal(e.Skipped, wantSkips) {
		t.Errorf("skipped = %v, want %v", e.Skipped, wantSkips)
	}
	if len(doc.ModelSets) != 1 || doc.ModelSets[0].Name != "M" || len(doc.ModelSets[0].Specs) != 1 {
		t.Fatalf("model sets = %+v, want M with one spec (N has nothing to deploy)", doc.ModelSets)
	}
	set := doc.ModelSets[0]
	s := set.Specs[0]
	if s.Source != "model_scope" || s.ModelScopeModelID != "ms/m" || set.Home != "https://modelscope.cn/models/ms/m" {
		t.Errorf("source = %s %q home %q, want the ModelScope fallback", s.Source, s.ModelScopeModelID, set.Home)
	}
	if s.GPUFilters == nil || !slices.Equal(s.GPUFilters.Vendor, []string{"amd"}) {
		t.Errorf("gpu_filters = %+v, want the preset's vendors over the backend's", s.GPUFilters)
	}
}

func TestParameters(t *testing.T) {
	for _, c := range []struct{ args, want []string }{
		{[]string{"--enable-auto-tool-choice", "--tool-call-parser", "llama3_json"}, []string{"--enable-auto-tool-choice", "--tool-call-parser=llama3_json"}},
		{[]string{"--rope-theta", "-1.5", "--x=1"}, []string{"--rope-theta=-1.5", "--x=1"}},
		// GPUStack would shlex-split "--chat-template=a b.jinja" into two tokens.
		{[]string{"--chat-template", "a b.jinja"}, []string{"--chat-template", "a b.jinja"}},
		{[]string{"--hf-overrides", `{"a":1}`}, []string{"--hf-overrides", `{"a":1}`}},
		// A flag with several values keeps them as separate items.
		{[]string{"--lora-modules", "a", "b", "--trust-remote-code"}, []string{"--lora-modules", "a", "b", "--trust-remote-code"}},
	} {
		if got := parameters(c.args); !slices.Equal(got, c.want) {
			t.Errorf("parameters(%q) = %q, want %q", c.args, got, c.want)
		}
	}
}
