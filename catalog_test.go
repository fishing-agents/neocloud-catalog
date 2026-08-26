package catalog

import (
	"testing"
)

// llama8b is a small, known fixture: 8B params, fp16 ~16GB weights, 8k default
// context, ~0.5 KB/token KV cache.
func llama8b() ModelTemplate {
	return ModelTemplate{
		Slug:            "llama-3.1-8b",
		Name:            "Llama 3.1 8B",
		Family:          "llama",
		ParamsB:         8,
		DefaultContext:  8192,
		ContextMax:      131072,
		Quantizations:   []Quantization{{Name: "fp16", WeightsGB: 16}, {Name: "fp8", WeightsGB: 8}},
		KVBytesPerToken: 512,
	}
}

func TestVRAMRequired(t *testing.T) {
	m := llama8b()
	// fp16 at 8192 context: 16GB weights + 512B*8192/1e9 ~= 16 + 4.19e-3.
	vram, ok := VRAMRequired(m, "fp16", 8192)
	if !ok {
		t.Fatalf("fp16 quant not found")
	}
	if vram < 16 || vram > 16.1 {
		t.Fatalf("vram = %.4f, want ~16.004", vram)
	}
	if _, ok := VRAMRequired(m, "gguf-q4", 8192); ok {
		t.Fatalf("unknown quant should report not found")
	}
}

func TestFit(t *testing.T) {
	m := llama8b()
	// A 24GB full GPU fits the model at 8k context but not at the model's 128k max.
	full := ModelFit{ModelSlug: m.Slug, DeviceFamily: "A10G", MaxContext: 8192, VRAMRequiredGB: 24}
	if !Fit(m, full, "fp16", 8192) {
		t.Fatalf("expected fit at max context")
	}
	if Fit(m, full, "fp16", 8193) {
		t.Fatalf("expected no-fit one context over the ceiling")
	}
	// A 16GB GPU fits only with the fp8 weights.
	tight := ModelFit{ModelSlug: m.Slug, DeviceFamily: "A10G", MaxContext: 131072, VRAMRequiredGB: 16}
	if Fit(m, tight, "fp16", 8192) {
		t.Fatalf("fp16 should not fit 16GB at 8k")
	}
	if !Fit(m, tight, "fp8", 8192) {
		t.Fatalf("fp8 should fit 16GB at 8k")
	}
}

func TestDigestStableAndContentSensitive(t *testing.T) {
	m := llama8b()
	snap := CatalogSnapshot{SchemaVersion: SchemaVersion, Version: "2026-08-25.1", Models: []ModelTemplate{m}, Fits: []ModelFit{{ModelSlug: m.Slug, DeviceFamily: "A10G", MaxContext: 8192, VRAMRequiredGB: 24}}}
	d1 := snap.Digest()
	d2 := snap.Digest()
	if d1 != d2 {
		t.Fatalf("digest unstable across calls: %s vs %s", d1, d2)
	}
	// Identical content, different version, different digest.
	snap2 := snap
	snap2.Version = "2026-08-25.2"
	if d3 := snap2.Digest(); d3 == d1 {
		t.Fatalf("digest should change when version changes")
	}
}
