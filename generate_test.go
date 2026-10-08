package catalog

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
)

func llama70b() ModelTemplate {
	return ModelTemplate{
		Slug: "llama-3.1-70b", DefaultContext: 8192, ContextMax: 131072,
		Quantizations: []Quantization{{Name: "bf16", WeightsGB: 141.1}},
		Architecture:  Architecture{Layers: 80, AttentionHeads: 64, KVHeads: 8, HeadDim: 128},
	}
}

func TestGenerateFits(t *testing.T) {
	snap, _, err := Generate(
		[]Family{{Name: "L4", MemoryGB: 24}, {Name: "A100", MemoryGB: 80}, {Name: "MI300X", MemoryGB: 192}},
		nil,
		[]ModelTemplate{llama8b(), llama70b()},
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]ModelFit{}
	for _, f := range snap.Fits {
		got[f.ModelSlug+"/"+f.DeviceFamily+"/"+f.Quantization] = f
	}
	cases := []struct {
		key       string
		gpus, ctx int
		vram      float64
	}{
		// (24 x 0.9 - 16.1)GB / 131072B per token; vram is weights + KV at max_context.
		{"llama-3.1-8b/L4/bf16", 1, 41961, 21.6},
		{"llama-3.1-8b/A100/bf16", 1, 131072, 33.28},
		// 141.1GB does not fit one A100 (72GB budget): two, with (144 - 141.1)GB of KV.
		{"llama-3.1-70b/A100/bf16", 2, 8850, 144},
		{"llama-3.1-70b/MI300X/bf16", 1, 96740, 172.8},
		{"llama-3.1-70b/L4/bf16", 8, 96740, 172.8},
	}
	for _, c := range cases {
		f, ok := got[c.key]
		if !ok {
			t.Errorf("%s: missing fit", c.key)
			continue
		}
		if f.GPUCount != c.gpus || f.MaxContext != c.ctx || f.VRAMRequiredGB != c.vram {
			t.Errorf("%s = %d GPUs, ctx %d, vram %v; want %d, %d, %v", c.key, f.GPUCount, f.MaxContext, f.VRAMRequiredGB, c.gpus, c.ctx, c.vram)
		}
	}
	if kv := snap.Models[0].KVBytesPerToken; kv != 131072 {
		t.Errorf("snapshot kv_bytes_per_token = %v, want derived 131072", kv)
	}
}

func TestGenerateRespectsTensorParallelHeads(t *testing.T) {
	// 6 attention heads: TP 4 and 8 are illegal, so the 2-GPU option is the last
	// one tried before giving up.
	m := ModelTemplate{Slug: "odd", DefaultContext: 1024, ContextMax: 1024,
		Quantizations: []Quantization{{Name: "bf16", WeightsGB: 30}},
		Architecture:  Architecture{Layers: 1, AttentionHeads: 6, KVHeads: 6, HeadDim: 64}}
	snap, _, err := Generate([]Family{{Name: "small", MemoryGB: 10}}, nil, []ModelTemplate{m}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Fits) != 0 {
		t.Fatalf("fits = %+v, want none: 30GB needs 4 GPUs of 9GB but 6 heads forbid TP 4", snap.Fits)
	}
	if !slices.Equal(tpDegrees(6), []int{1, 2}) {
		t.Fatalf("tp degrees for 6 heads = %v, want [1 2]", tpDegrees(6))
	}
}

func TestGenerateRejectsUnresolvedPresetsAndUnpinnedSources(t *testing.T) {
	backends := []Backend{{Name: "vllm", Versions: []BackendVersion{{Version: "v1"}}}}
	cases := []struct {
		name string
		edit func(*ModelTemplate)
		want string
	}{
		{"valid", func(*ModelTemplate) {}, ""},
		{"local source needs no revision", func(m *ModelTemplate) {
			m.Quantizations[0].Sources[0] = ModelSource{Type: SourceLocal, Repo: "/models/m"}
		}, ""},
		{"unknown quantization", func(m *ModelTemplate) { m.Presets[0].Quantization = "int4" }, `unknown quantization "int4"`},
		{"unknown backend", func(m *ModelTemplate) { m.Presets[0].Backend = "tgi" }, "unknown backend version tgi v1"},
		{"unknown backend version", func(m *ModelTemplate) { m.Presets[0].BackendVersion = "v2" }, "unknown backend version vllm v2"},
		{"duplicate preset", func(m *ModelTemplate) { m.Presets = append(m.Presets, m.Presets[0]) }, `duplicate preset "vllm-bf16"`},
		{"huggingface source without revision", func(m *ModelTemplate) { m.Quantizations[0].Sources[0].Revision = "" }, "revision required"},
		{"unknown source type", func(m *ModelTemplate) { m.Quantizations[0].Sources[0].Type = "s3" }, `unknown type "s3"`},
	}
	for _, c := range cases {
		m := llama8b()
		m.Quantizations[0].Sources = []ModelSource{{Type: SourceHuggingFace, Repo: "org/model", Revision: "abc123"}}
		m.Presets = []Preset{{Name: "vllm-bf16", Quantization: "bf16", Backend: "vllm", BackendVersion: "v1"}}
		c.edit(&m)
		_, _, err := Generate(nil, nil, []ModelTemplate{m}, nil, backends)
		if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
}

// curatedSnap puts one model and its drafts next to the curated backends, so
// presets are validated against the real flag tables.
func curatedSnap(t *testing.T, m ModelTemplate, drafts ...DraftModel) CatalogSnapshot {
	t.Helper()
	snap := CatalogSnapshot{Models: []ModelTemplate{m}, DraftModels: drafts}
	for name, bv := range curatedBackends(t) {
		snap.Backends = append(snap.Backends, Backend{Name: name, Versions: []BackendVersion{bv}})
	}
	return snap
}

// wantValidate fails unless Validate succeeds (want "") or its error contains want.
func wantValidate(t *testing.T, name string, snap CatalogSnapshot, want string) {
	t.Helper()
	err := snap.Validate()
	if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
		t.Errorf("%s: err = %v, want %q", name, err, want)
	}
}

func TestValidateRejectsUnsupportedParser(t *testing.T) {
	m := ModelTemplate{Slug: "gemma-2-9b", DefaultContext: 8192, ContextMax: 8192, Quantizations: []Quantization{{Name: "bf16"}},
		Presets: []Preset{{Name: "vllm-bf16", Quantization: "bf16", Backend: "vllm", BackendVersion: "v0.31.0", ToolParser: "llama3"}}}
	wantValidate(t, "parser in the flag table", curatedSnap(t, m), "")
	m.Presets[0].ToolParser = "gemma2"
	wantValidate(t, "parser not in the flag table", curatedSnap(t, m), `gemma-2-9b/vllm-bf16 on vllm v0.31.0: has no tool parser "gemma2"`)
}

func eagle3Drafts() []DraftModel {
	redhat, lmsys := *vllmEagle3, *sglangEagle3
	redhat.Targets, redhat.Backends = []string{"llama-3.1-8b"}, []string{"vllm"}
	lmsys.Targets, lmsys.Backends = []string{"llama-3.1-8b"}, []string{"sglang"}
	return []DraftModel{redhat, lmsys}
}

func TestValidateRejectsDraftForOtherBackend(t *testing.T) {
	m := llama8b()
	m.Presets = []Preset{{Name: "eagle3", Quantization: "bf16", Backend: "vllm", BackendVersion: "v0.31.0",
		Speculative: &Speculative{Method: "eagle3", DraftModel: vllmEagle3.Slug, Tokens: 3}}}
	wantValidate(t, "vllm preset with the vllm draft", curatedSnap(t, m, eagle3Drafts()...), "")
	m.Presets[0].Backend, m.Presets[0].BackendVersion = "sglang", "v0.5.21"
	wantValidate(t, "sglang preset with the vllm draft", curatedSnap(t, m, eagle3Drafts()...),
		`llama-3.1-8b/eagle3 on sglang v0.5.21: draft model "eagle3-llama-3.1-8b-redhat" does not load on sglang`)
}

func TestValidateRejectsContextAboveMax(t *testing.T) {
	m := llama8b()
	m.Presets = []Preset{{Name: "vllm-bf16", Quantization: "bf16", Backend: "vllm", BackendVersion: "v0.31.0", Context: 131072}}
	wantValidate(t, "context at max", curatedSnap(t, m), "")
	m.Presets[0].Context = 131073
	wantValidate(t, "context above max", curatedSnap(t, m), "llama-3.1-8b/vllm-bf16 on vllm v0.31.0: context 131073 above context_max 131072")
}

func TestValidateRejectsUnknownTopologyTransfer(t *testing.T) {
	m := llama70b()
	m.Presets = []Preset{{Name: "sglang-pd", Quantization: "bf16", Backend: "sglang", BackendVersion: "v0.5.21",
		Topology: Topology{Kind: "disaggregated", Prefill: 2, Decode: 2, Transfer: "nixl"}}}
	wantValidate(t, "nixl", curatedSnap(t, m), "")
	m.Presets[0].Topology.Transfer = "ucx"
	wantValidate(t, "ucx", curatedSnap(t, m), `llama-3.1-70b/sglang-pd on sglang v0.5.21: has no kv transfer "ucx"`)
}

// Each case breaks one rule the renderer relies on but cannot check itself.
func TestValidateRejectsInconsistentPresetsAndDrafts(t *testing.T) {
	cases := []struct {
		name string
		edit func(*ModelTemplate, []DraftModel)
		want string
	}{
		{"valid", func(*ModelTemplate, []DraftModel) {}, ""},
		{"disaggregated without decode gpus", func(m *ModelTemplate, _ []DraftModel) {
			m.Presets[0].Topology = Topology{Kind: "disaggregated", Prefill: 2, Transfer: "nixl"}
		}, "llama-3.1-8b/vllm-bf16 on vllm v0.31.0: disaggregated topology needs prefill_gpus and decode_gpus >= 1 and no tensor_parallel"},
		{"disaggregated with tensor parallel", func(m *ModelTemplate, _ []DraftModel) {
			m.Presets[0].TensorParallel = 4
			m.Presets[0].Topology = Topology{Kind: "disaggregated", Prefill: 2, Decode: 2, Transfer: "nixl"}
		}, "disaggregated topology needs prefill_gpus and decode_gpus >= 1 and no tensor_parallel"},
		{"tool template without vendors", func(m *ModelTemplate, _ []DraftModel) { m.Presets[0].Vendors = nil },
			`llama-3.1-8b/vllm-bf16 on vllm v0.31.0: chat template "llama3.1-tools" needs vendors [nvidia]`},
		{"tool template on amd too", func(m *ModelTemplate, _ []DraftModel) { m.Presets[0].Vendors = []string{"nvidia", "amd"} },
			`chat template "llama3.1-tools" needs vendors [nvidia]`},
		{"min backend version compares numerically", func(m *ModelTemplate, _ []DraftModel) { m.Presets[0].MinBackendVersion = "v0.9.0" }, ""},
		{"min backend version equal", func(m *ModelTemplate, _ []DraftModel) { m.Presets[0].MinBackendVersion = "v0.31" }, ""},
		{"min backend version newer", func(m *ModelTemplate, _ []DraftModel) { m.Presets[0].MinBackendVersion = "v0.31.1" },
			"llama-3.1-8b/vllm-bf16 on vllm v0.31.0: min_backend_version v0.31.1 is newer than the backend version"},
		{"min backend version not a version", func(m *ModelTemplate, _ []DraftModel) { m.Presets[0].MinBackendVersion = "latest" },
			`version "latest" is not dot-separated numbers`},
		{"unknown draft", func(m *ModelTemplate, _ []DraftModel) { m.Presets[1].Speculative.DraftModel = "nope" },
			`llama-3.1-8b/eagle3 on vllm v0.31.0: unknown draft model "nope"`},
		{"draft for another model", func(_ *ModelTemplate, d []DraftModel) { d[0].Targets = []string{"llama-3.1-70b"} },
			`draft model "eagle3-llama-3.1-8b-redhat" does not target llama-3.1-8b`},
		{"draft of another method", func(_ *ModelTemplate, d []DraftModel) { d[0].Method = "eagle" },
			`draft model "eagle3-llama-3.1-8b-redhat" is eagle, not eagle3`},
		{"draft from oci", func(_ *ModelTemplate, d []DraftModel) {
			d[0].Source = ModelSource{Type: SourceOCI, Repo: "registry.example/eagle3", Revision: "sha256:abc"}
		}, `draft model "eagle3-llama-3.1-8b-redhat": source type "oci", want huggingface or modelscope`},
		{"unpinned draft", func(_ *ModelTemplate, d []DraftModel) { d[1].Source.Revision = "" }, "revision required"},
		{"draft for an unknown model", func(_ *ModelTemplate, d []DraftModel) { d[1].Targets = append(d[1].Targets, "nope") },
			`draft model "eagle3-llama-3.1-8b-sglang": unknown target "nope"`},
		{"duplicate draft", func(_ *ModelTemplate, d []DraftModel) { d[1].Slug = d[0].Slug },
			`draft model "eagle3-llama-3.1-8b-redhat": duplicate`},
	}
	for _, c := range cases {
		m, drafts := llama8b(), eagle3Drafts()
		m.Presets = []Preset{
			{Name: "vllm-bf16", Quantization: "bf16", Backend: "vllm", BackendVersion: "v0.31.0", Context: 8192,
				ToolParser: "llama3", ChatTemplate: "llama3.1-tools", Vendors: []string{"nvidia"}},
			{Name: "eagle3", Quantization: "bf16", Backend: "vllm", BackendVersion: "v0.31.0",
				Speculative: &Speculative{Method: "eagle3", DraftModel: vllmEagle3.Slug, Tokens: 3}},
		}
		c.edit(&m, drafts)
		wantValidate(t, c.name, curatedSnap(t, m, drafts...), c.want)
	}
}

// Regenerating the committed data must reproduce the embedded snapshot: the
// generator is deterministic and snapshots/v3 is not stale.
func TestCommittedSnapshotMatchesData(t *testing.T) {
	var families []Family
	var nodeTypes []NodeType
	var models []ModelTemplate
	var drafts []DraftModel
	var backends []Backend
	for path, v := range map[string]any{"data/families.json": &families, "data/node_types.json": &nodeTypes, "data/models.json": &models,
		"data/draft_models.json": &drafts, "data/backends.json": &backends} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, v); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
	snap, _, err := Generate(families, nodeTypes, models, drafts, backends)
	if err != nil {
		t.Fatal(err)
	}
	committed, err := SnapshotV3()
	if err != nil {
		t.Fatal(err)
	}
	snap.Version = committed.Version
	if snap.Digest() != committed.Digest() {
		t.Fatal("snapshots/v3/catalog.json is stale: run go run ./cmd/catalog-generate")
	}
}
