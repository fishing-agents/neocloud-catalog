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
	snap, err := Generate(
		[]ModelTemplate{llama8b(), llama70b()},
		[]FamilyCapability{{Name: "L4", MemoryGB: 24}, {Name: "A100", MemoryGB: 80}, {Name: "MI300X", MemoryGB: 192}},
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
	snap, err := Generate([]ModelTemplate{m}, []FamilyCapability{{Name: "small", MemoryGB: 10}}, nil)
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
		_, err := Generate([]ModelTemplate{m}, nil, backends)
		if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
}

// Regenerating the committed data must reproduce the embedded snapshot: the
// generator is deterministic and snapshots/v3 is not stale.
func TestCommittedSnapshotMatchesData(t *testing.T) {
	var models []ModelTemplate
	var families []FamilyCapability
	var backends []Backend
	for path, v := range map[string]any{"data/models.json": &models, "data/families.json": &families, "data/backends.json": &backends} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, v); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
	snap, err := Generate(models, families, backends)
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
