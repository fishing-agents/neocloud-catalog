package catalog

import (
	"slices"
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
	snap := Generate(
		[]ModelTemplate{llama8b(), llama70b()},
		[]FamilyCapability{{Name: "L4", MemoryGB: 24}, {Name: "A100", MemoryGB: 80}, {Name: "MI300X", MemoryGB: 192}},
	)
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
	snap := Generate([]ModelTemplate{m}, []FamilyCapability{{Name: "small", MemoryGB: 10}})
	if len(snap.Fits) != 0 {
		t.Fatalf("fits = %+v, want none: 30GB needs 4 GPUs of 9GB but 6 heads forbid TP 4", snap.Fits)
	}
	if !slices.Equal(tpDegrees(6), []int{1, 2}) {
		t.Fatalf("tp degrees for 6 heads = %v, want [1 2]", tpDegrees(6))
	}
}
