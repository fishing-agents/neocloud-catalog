package catalog

import "testing"

// TestGenerateFits pins the fit math: 24GB family, fp16 16GB weights, 512B/token
// KV cache. (24-16)GB = 8GB for KV = 15,625,000 tokens, capped at the 128k ceiling.
func TestGenerateFits(t *testing.T) {
	snap := Generate(
		[]ModelTemplate{llama8b()},
		[]FamilyCapability{{Name: "A10G", MemoryGB: 24}},
	)
	if len(snap.Fits) != 1 {
		t.Fatalf("want 1 fit, got %d", len(snap.Fits))
	}
	f := snap.Fits[0]
	if f.ModelSlug != "llama-3.1-8b" || f.DeviceFamily != "A10G" {
		t.Fatalf("unexpected fit: %+v", f)
	}
	if f.MaxContext != 131072 {
		t.Fatalf("max_context = %d, want 131072", f.MaxContext)
	}
	if f.VRAMRequiredGB != 16 {
		t.Fatalf("vram = %v, want 16", f.VRAMRequiredGB)
	}
}
