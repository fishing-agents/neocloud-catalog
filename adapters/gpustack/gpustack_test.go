// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

package gpustack

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
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
	snap, err := catalog.SnapshotV4()
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

func key(model, preset string) adapters.PresetKey {
	return adapters.PresetKey{Model: model, Preset: preset}
}

func specs(doc document, r adapters.Ref) []spec {
	var out []spec
	for _, set := range doc.ModelSets {
		if set.Name == r.Model.Name {
			for _, s := range set.Specs {
				if s.Mode == r.Preset.Name {
					out = append(out, s)
				}
			}
		}
	}
	return out
}

// One spec per GPU vendor, with Render's args minus the GPU count and the needed compute capability.
func TestCommittedPresets(t *testing.T) {
	snap := committed(t)
	e, doc := export(t, snap)
	wantSkips := []adapters.Skip{
		{PresetKey: key("llama-3.1-70b", "sglang-bf16-pd"), Reason: SkipTopology},
		{PresetKey: key("llama-3.1-70b", "vllm-bf16-2node"), Reason: SkipTopology},
	}
	if !slices.Equal(e.Skipped, wantSkips) {
		t.Errorf("skipped = %v, want %v", e.Skipped, wantSkips)
	}
	wantCC := map[string]string{"vllm/fp8": ">=7.5", "sglang/fp8": ">=8.0", "vllm/float": ">=8.0", "sglang/float": ">=8.0", "llamacpp/gguf": ""}
	for _, r := range adapters.Presets(snap) {
		if !slices.Contains(e.Exported, r.Key()) {
			continue
		}
		bv, _ := adapters.BackendVersion(snap, r.Preset.Backend, r.Preset.BackendVersion)
		q, _ := r.Quantization()
		vendors := slices.DeleteFunc(adapters.Vendors(r.Preset, bv), func(v string) bool { return v == "cpu" })
		got := specs(doc, r)
		if len(got) != len(vendors) {
			t.Errorf("%v: %d specs, want one per vendor of %v", r.Key(), len(got), vendors)
			continue
		}
		for i, s := range got {
			v := vendors[i]
			args, err := catalog.Render(r.Model, r.Preset, bv, catalog.ModelFit{}, adapters.Draft(snap, r.Preset), v)
			if err != nil {
				t.Fatal(err)
			}
			cc := ""
			if v == "nvidia" {
				cc = wantCC[r.Preset.Backend+"/"+q.Kind]
			}
			if s.GPUFilters == nil || !slices.Equal(s.GPUFilters.Vendor, []string{v}) || s.GPUFilters.ComputeCapability != cc || s.ImageName != bv.Images[v] {
				t.Errorf("%v: spec %d filters %+v on %s, want vendor %s, compute capability %q, image %s", r.Key(), i, s.GPUFilters, s.ImageName, v, cc, bv.Images[v])
			}
			if r.Preset.Backend == "llamacpp" {
				if s.Backend != "Custom" || s.BackendParameters != nil || !strings.HasSuffix(s.RunCommand, " "+strings.Join(args, " ")) {
					t.Errorf("%v: %s run_command %q parameters %q, want Custom ending in %q", r.Key(), s.Backend, s.RunCommand, s.BackendParameters, args)
				}
				continue
			}
			if want := parameters(args); !slices.Equal(s.BackendParameters, want) || s.RunCommand != "" {
				t.Errorf("%v: backend_parameters = %q, want %q", r.Key(), s.BackendParameters, want)
			}
		}
	}
	// GPUStack defaults a repo from its last spec: it must be plain vLLM.
	speculative := map[string]bool{}
	for _, r := range adapters.Presets(snap) {
		speculative[r.Preset.Name] = r.Preset.Speculative != nil
	}
	for _, set := range doc.ModelSets {
		last := map[string]spec{}
		vllm := map[string]bool{}
		for _, s := range set.Specs {
			repo := s.HuggingFaceRepoID + s.ModelScopeModelID
			last[repo] = s
			vllm[repo] = vllm[repo] || s.Backend == "vLLM"
		}
		for repo, s := range last {
			if vllm[repo] && (s.Backend != "vLLM" || speculative[s.Mode]) {
				t.Errorf("%s: last spec of %s is %s %s, want a plain vLLM spec", set.Name, repo, s.Backend, s.Mode)
			}
		}
	}
}

// llama.cpp runs as Custom: run_command holds llama-server's args, the GGUF file is the filename.
func TestLlamaCpp(t *testing.T) {
	snap := committed(t)
	_, doc := export(t, snap)
	r := adapters.Presets(snap)[slices.IndexFunc(adapters.Presets(snap), func(r adapters.Ref) bool {
		return r.Key() == key("llama-3.1-8b", "llamacpp-q4km")
	})]
	got := specs(doc, r)
	want := spec{
		Mode:                "llamacpp-q4km",
		Quantization:        "Q4_K_M",
		GPUFilters:          &gpuFilters{Vendor: []string{"nvidia"}},
		Source:              "huggingface",
		HuggingFaceRepoID:   "bartowski/Meta-Llama-3.1-8B-Instruct-GGUF",
		HuggingFaceFilename: "Meta-Llama-3.1-8B-Instruct-Q4_K_M.gguf",
		Backend:             "Custom",
		ImageName:           "ghcr.io/ggml-org/llama.cpp:server-cuda-b11429",
		RunCommand:          "--model {{model_path}} --alias {{model_name}} --host 0.0.0.0 --port {{port}} -c 8192 --jinja",
	}
	if len(got) != 2 || !reflect.DeepEqual(got[1], want) || got[0].ImageName != "ghcr.io/ggml-org/llama.cpp:server-rocm-b11429" {
		t.Errorf("specs = %+v, want an amd spec then %+v", got, want)
	}
}

// Only a single-file gguf names its file; a multi-file gguf source is skipped.
func TestOnlyGGUFNamesItsFile(t *testing.T) {
	snap := committed(t)
	m := &snap.Models[slices.IndexFunc(snap.Models, func(m catalog.ModelTemplate) bool { return m.Slug == "llama-3.1-8b" })]
	file := func(path string) catalog.ModelFile {
		return catalog.ModelFile{Path: path, Size: 1, Digest: "sha256:" + strings.Repeat("ab", 32)}
	}
	m.Quantizations[0].Sources[0].Files = []catalog.ModelFile{file("config.json"), file("model.safetensors")}
	gguf := &m.Quantizations[2].Sources[0]
	gguf.Files = append(gguf.Files, file("other.gguf"))
	e, doc := export(t, snap)
	refs := adapters.Presets(snap)
	bf16 := specs(doc, refs[slices.IndexFunc(refs, func(r adapters.Ref) bool { return r.Key() == key("llama-3.1-8b", "vllm-bf16") })])
	if len(bf16) == 0 {
		t.Fatal("vllm-bf16 has no spec")
	}
	for _, s := range bf16 {
		if s.HuggingFaceRepoID != "meta-llama/Llama-3.1-8B-Instruct" || s.HuggingFaceFilename != "" {
			t.Errorf("vllm-bf16 source %q file %q, want the repository without a file", s.HuggingFaceRepoID, s.HuggingFaceFilename)
		}
	}
	if want := (adapters.Skip{PresetKey: key("llama-3.1-8b", "llamacpp-q4km"), Reason: SkipNoSource}); !slices.Contains(e.Skipped, want) {
		t.Errorf("skipped = %v, want %v", e.Skipped, want)
	}
}

// Model metadata maps to GPUStack's vocabulary (long-context is context/<N>K).
func TestModelSets(t *testing.T) {
	snap := committed(t)
	_, doc := export(t, snap)
	want := map[string]modelSet{
		"Llama 3.1 8B": {Description: "Llama 3.1 instruction-tuned 8B text model, optimized for multilingual dialogue use cases.",
			Home: "https://huggingface.co/meta-llama/Llama-3.1-8B-Instruct", Size: 8, Categories: []string{"llm"},
			Capabilities: []string{"context/128K", "tools"}, Licenses: []string{"llama3.1"}, ReleaseDate: "2024-07-23"},
		"Mixtral 8x7B": {Description: "Instruction-tuned Mixtral-8x7B, a pretrained generative sparse mixture of experts.",
			Home: "https://huggingface.co/mistralai/Mixtral-8x7B-Instruct-v0.1", Size: 46.7, ActivatedSize: 12.9, Categories: []string{"llm"},
			Capabilities: []string{"context/32K"}, Licenses: []string{"apache-2.0"}, ReleaseDate: "2023-12-11"},
	}
	for _, set := range doc.ModelSets {
		w, ok := want[set.Name]
		if !ok {
			continue
		}
		w.Name, w.Specs = set.Name, set.Specs
		if !reflect.DeepEqual(set, w) {
			t.Errorf("model set = %+v, want %+v", set, w)
		}
		delete(want, set.Name)
	}
	if len(want) > 0 {
		t.Errorf("model sets missing: %v", want)
	}

	m := snap.Models[0]
	m.Home, m.Task, m.Capabilities, m.ParamsActiveB = "", "rerank", []string{"reasoning", "vision", "json-schema", "long-context"}, m.ParamsB
	_, doc = export(t, catalog.CatalogSnapshot{Models: []catalog.ModelTemplate{m}, Backends: snap.Backends, DraftModels: snap.DraftModels})
	set := doc.ModelSets[0]
	if !slices.Equal(set.Categories, []string{"reranker"}) || !slices.Equal(set.Capabilities, []string{"context/128K", "reasoning", "vision"}) ||
		set.ActivatedSize != 0 || set.Home != "https://huggingface.co/meta-llama/Llama-3.1-8B-Instruct" {
		t.Errorf("rerank model set = %+v, want category reranker, GPUStack's capabilities only, no active size, home from the first spec", set)
	}
	m.Home = "https://example.com/card"
	if _, doc = export(t, catalog.CatalogSnapshot{Models: []catalog.ModelTemplate{m}, Backends: snap.Backends, DraftModels: snap.DraftModels}); doc.ModelSets[0].Home != m.Home {
		t.Errorf("home = %s, want the model's %s", doc.ModelSets[0].Home, m.Home)
	}
}

func TestComputeCapability(t *testing.T) {
	images := map[string]string{"nvidia": "n", "amd": "a"}
	snap := catalog.CatalogSnapshot{
		Families: []catalog.Family{
			{Name: "A100", Vendor: "nvidia", ComputeCapability: "8.0", DTypes: []string{"bf16"}},
			{Name: "H100", Vendor: "nvidia", ComputeCapability: "9.0", DTypes: []string{"bf16", "fp8"}},
			{Name: "L4", Vendor: "nvidia", ComputeCapability: "8.9", DTypes: []string{"bf16", "fp8"}},
			{Name: "MI300X", Vendor: "amd", ComputeCapability: "gfx942", DTypes: []string{"bf16", "fp8"}},
		},
		Backends: []catalog.Backend{
			{Name: "vllm", Versions: []catalog.BackendVersion{{Version: "v1", Images: images, MinComputeCapability: "7.5", Quantizations: map[string]catalog.QuantSupport{
				"float": {NativeDType: "bf16"}, "fp8": {NativeDType: "fp8", EmulatedFrom: "7.5"},
			}}}},
			{Name: "sglang", Versions: []catalog.BackendVersion{{Version: "v1", Images: images, Quantizations: map[string]catalog.QuantSupport{
				"fp8": {NativeDType: "fp8"},
			}}}},
		},
		Models: []catalog.ModelTemplate{{
			Slug: "m", Name: "M",
			Quantizations: []catalog.Quantization{
				{Name: "bf16", Kind: "float", Sources: []catalog.ModelSource{{Type: catalog.SourceHuggingFace, Repo: "o/m"}}},
				{Name: "fp8", Kind: "fp8", Sources: []catalog.ModelSource{{Type: catalog.SourceHuggingFace, Repo: "o/m-fp8"}}},
			},
			Presets: []catalog.Preset{
				{Name: "vllm-fp8", Quantization: "fp8", Backend: "vllm", BackendVersion: "v1"},
				{Name: "sglang-fp8", Quantization: "fp8", Backend: "sglang", BackendVersion: "v1"},
				{Name: "vllm-bf16", Quantization: "bf16", Backend: "vllm", BackendVersion: "v1"},
			},
		}},
	}
	_, doc := export(t, snap)
	// AMD gfx targets are not compute capabilities, so only NVIDIA specs get a floor.
	want := map[string]string{"vllm-fp8": ">=7.5", "sglang-fp8": ">=8.9", "vllm-bf16": ">=8.0"}
	for _, s := range doc.ModelSets[0].Specs {
		cc := want[s.Mode]
		if s.GPUFilters.Vendor[0] == "amd" {
			cc = ""
		}
		if s.GPUFilters.ComputeCapability != cc {
			t.Errorf("%s on %v: compute_capability %q, want %q", s.Mode, s.GPUFilters.Vendor, s.GPUFilters.ComputeCapability, cc)
		}
	}
}

func TestSkips(t *testing.T) {
	snap := catalog.CatalogSnapshot{
		Backends: []catalog.Backend{{Name: "vllm", Versions: []catalog.BackendVersion{{
			Version: "v1", Images: map[string]string{"nvidia": "vllm/vllm-openai:v1", "amd": "vllm/vllm-openai-rocm:v1"},
			Quantizations: map[string]catalog.QuantSupport{"float": {}},
			Flags:         catalog.FlagTable{Context: "--max-model-len", MultiNode: "vllm"},
		}}}},
		Models: []catalog.ModelTemplate{
			{
				Slug: "m", Name: "M", DefaultContext: 4096, ContextMax: 8192,
				Quantizations: []catalog.Quantization{
					{Name: "bf16", Kind: "float", Sources: []catalog.ModelSource{{Type: catalog.SourceOCI, Repo: "reg/m"}, {Type: catalog.SourceModelScope, Repo: "ms/m"}}},
					{Name: "int4", Kind: "float", Sources: []catalog.ModelSource{{Type: catalog.SourceOCI, Repo: "reg/m-int4"}}},
				},
				Presets: []catalog.Preset{
					{Name: "vllm-bf16", Quantization: "bf16", Backend: "vllm", BackendVersion: "v1", Context: 4096, Vendors: []string{"amd"}},
					{Name: "vllm-int4", Quantization: "int4", Backend: "vllm", BackendVersion: "v1"},
					{Name: "tgi-bf16", Quantization: "bf16", Backend: "tgi", BackendVersion: "v1"},
					{Name: "vllm-bf16-2node", Quantization: "bf16", Backend: "vllm", BackendVersion: "v1", Topology: catalog.Topology{Kind: "multi-node", Nodes: 2}},
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
		{PresetKey: key("m", "vllm-int4"), Reason: SkipNoSource},
		{PresetKey: key("m", "tgi-bf16"), Reason: SkipBackend},
		{PresetKey: key("m", "vllm-bf16-2node"), Reason: SkipTopology},
		{PresetKey: key("n", "tgi-bf16"), Reason: SkipBackend},
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
	if s.GPUFilters == nil || !slices.Equal(s.GPUFilters.Vendor, []string{"amd"}) || s.ImageName != "vllm/vllm-openai-rocm:v1" {
		t.Errorf("gpu_filters = %+v image %s, want only the preset's vendor with its image", s.GPUFilters, s.ImageName)
	}
	if !slices.Equal(s.BackendParameters, []string{"--max-model-len=4096"}) {
		t.Errorf("backend_parameters = %q, want the preset's context", s.BackendParameters)
	}
}

func TestParameters(t *testing.T) {
	for _, c := range []struct{ args, want []string }{
		{[]string{"--enable-auto-tool-choice", "--tool-call-parser", "llama3_json"}, []string{"--enable-auto-tool-choice", "--tool-call-parser=llama3_json"}},
		{[]string{"--rope-theta", "-1.5", "--x=1"}, []string{"--rope-theta=-1.5", "--x=1"}},
		// GPUStack would shlex-split "--chat-template=a b.jinja" into two tokens.
		{[]string{"--chat-template", "a b.jinja"}, []string{"--chat-template", "a b.jinja"}},
		{[]string{"--hf-overrides", `{"a":1}`}, []string{"--hf-overrides", `{"a":1}`}},
		// shlex would also drop the quotes of a joined JSON value.
		{[]string{`--speculative-config={"method":"eagle3"}`}, []string{"--speculative-config", `{"method":"eagle3"}`}},
		// A flag with several values keeps them as separate items.
		{[]string{"--lora-modules", "a", "b", "--trust-remote-code"}, []string{"--lora-modules", "a", "b", "--trust-remote-code"}},
	} {
		if got := parameters(c.args); !slices.Equal(got, c.want) {
			t.Errorf("parameters(%q) = %q, want %q", c.args, got, c.want)
		}
	}
}

// run_command is shlex-split by GPUStack after it fills the placeholders.
func TestShellJoin(t *testing.T) {
	got := shellJoin([]string{"-m", "{{model_path}}", "a b", `it's`, ""})
	if want := `-m {{model_path}} 'a b' 'it'"'"'s' ''`; got != want {
		t.Errorf("shellJoin = %s, want %s", got, want)
	}
}
