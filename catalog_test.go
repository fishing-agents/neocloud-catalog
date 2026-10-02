package catalog

import (
	"testing"
)

// llama8b uses the published Llama 3.1 8B architecture: 32 layers, 32 attention
// heads, 8 KV heads (GQA), head dim 128.
func llama8b() ModelTemplate {
	return ModelTemplate{
		Slug:           "llama-3.1-8b",
		Name:           "Llama 3.1 8B",
		Family:         "llama",
		ParamsB:        8,
		DefaultContext: 8192,
		ContextMax:     131072,
		Quantizations:  []Quantization{{Name: "bf16", WeightsGB: 16.1}, {Name: "fp8", WeightsGB: 9.1}},
		Architecture:   Architecture{Layers: 32, AttentionHeads: 32, KVHeads: 8, HeadDim: 128},
	}
}

func TestKVBytesPerToken(t *testing.T) {
	// 2 (K and V) x 32 layers x 8 KV heads x 128 dim x 2 bytes.
	if got := llama8b().Architecture.KVBytesPerToken(); got != 131072 {
		t.Fatalf("kv bytes/token = %v, want 131072", got)
	}
}

func TestVRAMRequired(t *testing.T) {
	m := llama8b()
	// 16.1GB weights + 131072B x 8192 tokens = 16.1 + 1.0737GB.
	vram, ok := VRAMRequired(m, "bf16", 8192)
	if !ok || vram < 17.17 || vram > 17.18 {
		t.Fatalf("vram = %.4f (%v), want ~17.174", vram, ok)
	}
	if _, ok := VRAMRequired(m, "gguf-q4", 8192); ok {
		t.Fatalf("unknown quant should report not found")
	}
}

func TestFit(t *testing.T) {
	m := llama8b()
	row := ModelFit{ModelSlug: m.Slug, DeviceFamily: "L4", Quantization: "bf16", GPUCount: 1, MaxContext: 41961, VRAMRequiredGB: 21.6}
	if !Fit(m, row, "bf16", 32768) {
		t.Fatalf("expected fit within the row's context")
	}
	if Fit(m, row, "bf16", 41962) {
		t.Fatalf("expected no fit one token over the row's context")
	}
	if Fit(m, row, "fp8", 8192) {
		t.Fatalf("a bf16 row must not answer for fp8")
	}
}

func TestDigestStableAndContentSensitive(t *testing.T) {
	m := llama8b()
	snap := CatalogSnapshot{SchemaVersion: SchemaVersion, Version: "2026-08-25.1", Models: []ModelTemplate{m},
		Fits: []ModelFit{{ModelSlug: m.Slug, DeviceFamily: "L4", Quantization: "bf16", GPUCount: 1, MaxContext: 8192, VRAMRequiredGB: 24}}}
	d1 := snap.Digest()
	if d2 := snap.Digest(); d1 != d2 {
		t.Fatalf("digest unstable across calls: %s vs %s", d1, d2)
	}
	snap2 := snap
	snap2.Version = "2026-08-25.2"
	if d3 := snap2.Digest(); d3 == d1 {
		t.Fatalf("digest should change when version changes")
	}
}

func TestVRAMRequiredUsesStoredKVWithoutArchitecture(t *testing.T) {
	m := ModelTemplate{Quantizations: []Quantization{{Name: "bf16", WeightsGB: 10}}, KVBytesPerToken: 1e6}
	if vram, _ := VRAMRequired(m, "bf16", 1000); vram != 11 {
		t.Fatalf("vram = %v, want 11 from stored kv_bytes_per_token", vram)
	}
}
