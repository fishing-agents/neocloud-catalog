// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"encoding/json"
	"testing"
)

// llama8b uses the published Llama 3.1 8B architecture.
func llama8b() ModelTemplate {
	return ModelTemplate{
		Slug:           "llama-3.1-8b",
		Name:           "Llama 3.1 8B",
		Family:         "llama",
		Task:           "chat",
		ParamsB:        8,
		ParamsActiveB:  8,
		DefaultContext: 8192,
		ContextMax:     131072,
		Quantizations: []Quantization{{Name: "bf16", Kind: "float", RequiresDType: "bf16", WeightsGB: 16.1},
			{Name: "fp8", Kind: "fp8", RequiresDType: "fp8", WeightsGB: 9.1}},
		Architecture: Architecture{Layers: 32, AttentionHeads: 32, KVHeads: 8, HeadDim: 128},
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
	// An fp8 KV row holds 60000 tokens that would not fit at 16 bits.
	fp8KV := ModelFit{ModelSlug: m.Slug, DeviceFamily: "L4", Quantization: "bf16", KVCacheDType: "fp8", GPUCount: 1, MaxContext: 83923, VRAMRequiredGB: 21.6}
	if !Fit(m, fp8KV, "bf16", 60000) {
		t.Fatalf("expected fit within an fp8 KV row's context")
	}
}

func TestDigestStableAndContentSensitive(t *testing.T) {
	m := llama8b()
	snap := CatalogSnapshot{SchemaVersion: SchemaVersion, Version: "2026-08-25.1", Models: []ModelTemplate{m},
		Fits:     []ModelFit{{ModelSlug: m.Slug, DeviceFamily: "L4", Quantization: "bf16", GPUCount: 1, MaxContext: 8192, VRAMRequiredGB: 24}},
		Backends: []Backend{{Name: "vllm", Versions: []BackendVersion{{Version: "v1", Images: map[string]string{"cuda": "vllm/vllm-openai:v1"}}}}}}
	d1 := snap.Digest()
	if d2 := snap.Digest(); d1 != d2 {
		t.Fatalf("digest unstable across calls: %s vs %s", d1, d2)
	}
	snap2 := snap
	snap2.Version = "2026-08-25.2"
	if d3 := snap2.Digest(); d3 == d1 {
		t.Fatalf("digest should change when version changes")
	}
	snap3 := snap
	snap3.Backends = []Backend{{Name: "vllm", Versions: []BackendVersion{{Version: "v1", Images: map[string]string{"cuda": "vllm/vllm-openai:v2"}}}}}
	if d4 := snap3.Digest(); d4 == d1 {
		t.Fatalf("digest should change when a backend image changes")
	}
}

func TestVRAMRequiredUsesStoredKVWithoutArchitecture(t *testing.T) {
	m := ModelTemplate{Quantizations: []Quantization{{Name: "bf16", WeightsGB: 10}}, KVBytesPerToken: 1e6}
	if vram, _ := VRAMRequired(m, "bf16", 1000); vram != 11 {
		t.Fatalf("vram = %v, want 11 from stored kv_bytes_per_token", vram)
	}
}

// A 4.0.0 snapshot decodes and validates and a 4.0.0 overlay merges: only the major gates.
func TestDecodesSchema40Snapshot(t *testing.T) {
	const doc = `{"schema_version": "4.0.0", "version": "2026-10-01T00:00:00Z", "generated_at": "2026-10-01T00:00:00Z",
		"families": [{"name": "L4", "vendor": "nvidia", "architecture": "ada", "compute_capability": "8.9", "dtypes": ["bf16"],
			"memory_gb": 24, "memory_bandwidth_gbs": 300, "sources": []}],
		"node_types": [], "draft_models": [], "backends": [],
		"models": [{"slug": "m", "name": "M", "family": "llama", "task": "chat", "params_b": 8, "params_active_b": 8,
			"default_context": 8192, "context_max": 8192, "kv_bytes_per_token": 131072,
			"architecture": {"layers": 32, "attention_heads": 32, "kv_heads": 8, "head_dim": 128},
			"quantizations": [{"name": "bf16", "kind": "float", "requires_dtype": "bf16", "weights_gb": 16.1,
				"sources": [{"type": "huggingface", "repo": "org/m", "revision": "abc123"}]}]}],
		"fits": [{"model_slug": "m", "device_family": "L4", "quantization": "bf16", "kv_cache_dtype": "auto", "mode": "native",
			"gpu_count": 1, "nodes": 1, "max_context": 8192, "vram_required_gb": 17.18}]}`
	var snap CatalogSnapshot
	if err := json.Unmarshal([]byte(doc), &snap); err != nil {
		t.Fatal(err)
	}
	if src := snap.Models[0].Quantizations[0].Sources[0]; src.Repo != "org/m" || src.Revision != "abc123" || src.Files != nil {
		t.Fatalf("source = %+v", src)
	}
	if err := snap.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	o := Overlay{SchemaVersion: "4.0.0", SnapshotDigest: snap.Digest(), Models: map[string]Extensions{"m": {"x.io/a": okPayload}}}
	if _, err := Merge(snap, o); err != nil {
		t.Fatalf("merge a 4.0.0 overlay: %v", err)
	}
}
