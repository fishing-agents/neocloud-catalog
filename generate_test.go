package catalog

import (
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func llama70b() ModelTemplate {
	return ModelTemplate{
		Slug: "llama-3.1-70b", Task: "chat", ParamsB: 70, ParamsActiveB: 70, DefaultContext: 8192, ContextMax: 131072,
		Quantizations: []Quantization{{Name: "bf16", Kind: "float", RequiresDType: "bf16", WeightsGB: 141.1}},
		Architecture:  Architecture{Layers: 80, AttentionHeads: 64, KVHeads: 8, HeadDim: 128},
	}
}

// curatedFamilies returns the named families from data/families.json.
func curatedFamilies(t *testing.T, names ...string) []Family {
	t.Helper()
	b, err := os.ReadFile("data/families.json")
	if err != nil {
		t.Fatal(err)
	}
	var all []Family
	if err := json.Unmarshal(b, &all); err != nil {
		t.Fatal(err)
	}
	var out []Family
	for _, f := range all {
		if slices.Contains(names, f.Name) {
			out = append(out, f)
		}
	}
	if len(out) != len(names) {
		t.Fatalf("data/families.json lacks some of %v", names)
	}
	return out
}

func vllmPreset(name, quant string, context int) Preset {
	return Preset{Name: name, Purpose: "standard", Quantization: quant, Backend: "vllm", BackendVersion: "v0.31.0", Context: context}
}

func backendOf(name string, bv BackendVersion) []Backend {
	return []Backend{{Name: name, Versions: []BackendVersion{bv}}}
}

// fitsByKey indexes fits by "<slug>/<family>/<quantization>/<kv dtype>".
func fitsByKey(snap CatalogSnapshot) map[string]ModelFit {
	out := map[string]ModelFit{}
	for _, f := range snap.Fits {
		out[f.ModelSlug+"/"+f.DeviceFamily+"/"+f.Quantization+"/"+f.KVCacheDType] = f
	}
	return out
}

func TestGenerateFits(t *testing.T) {
	small, big := llama8b(), llama70b()
	small.Presets = []Preset{vllmPreset("vllm-bf16", "bf16", 8192)}
	big.Presets = []Preset{vllmPreset("vllm-bf16", "bf16", 8192)}
	snap, _, err := Generate(curatedFamilies(t, "L4", "A100", "MI300X"), nil, []ModelTemplate{small, big}, nil,
		backendOf("vllm", curatedBackends(t)["vllm"]))
	if err != nil {
		t.Fatal(err)
	}
	got := fitsByKey(snap)
	cases := []struct {
		key       string
		gpus, ctx int
		vram      float64
	}{
		// (24 x 0.9 - 16.1)GB / 131072B per token; vram is weights + KV at max_context.
		{"llama-3.1-8b/L4/bf16/auto", 1, 41961, 21.6},
		{"llama-3.1-8b/A100/bf16/auto", 1, 131072, 33.28},
		// 141.1GB does not fit one A100 (72GB budget): two, with (144 - 141.1)GB of KV.
		{"llama-3.1-70b/A100/bf16/auto", 2, 8850, 144},
		{"llama-3.1-70b/MI300X/bf16/auto", 1, 96740, 172.8},
		{"llama-3.1-70b/L4/bf16/auto", 8, 96740, 172.8},
	}
	for _, c := range cases {
		f, ok := got[c.key]
		if !ok {
			t.Errorf("%s: missing fit", c.key)
			continue
		}
		if f.GPUCount != c.gpus || f.Nodes != 1 || f.MaxContext != c.ctx || f.VRAMRequiredGB != c.vram {
			t.Errorf("%s = %d GPUs on %d nodes, ctx %d, vram %v; want %d on 1, %d, %v", c.key, f.GPUCount, f.Nodes, f.MaxContext, f.VRAMRequiredGB, c.gpus, c.ctx, c.vram)
		}
	}
	if _, ok := got["llama-3.1-8b/L4/fp8/auto"]; ok {
		t.Error("fit for the fp8 checkpoint, which no preset runs")
	}
	if kv := snap.Models[0].KVBytesPerToken; kv != 131072 {
		t.Errorf("snapshot kv_bytes_per_token = %v, want derived 131072", kv)
	}
}

func TestGenerateRespectsTensorParallelHeads(t *testing.T) {
	// 6 attention heads: TP 4 and 8 are illegal, so the 2-GPU option is the last
	// one tried before giving up.
	m := ModelTemplate{Slug: "odd", Task: "chat", DefaultContext: 1024, ContextMax: 1024,
		Quantizations: []Quantization{{Name: "bf16", Kind: "float", WeightsGB: 30}},
		Architecture:  Architecture{Layers: 1, AttentionHeads: 6, KVHeads: 6, HeadDim: 64},
		Presets:       []Preset{vllmPreset("vllm-bf16", "bf16", 1024)}}
	small := Family{Name: "small", Vendor: "nvidia", ComputeCapability: "9.0", DTypes: []string{"bf16"}, MemoryGB: 10}
	snap, _, err := Generate([]Family{small}, nil, []ModelTemplate{m}, nil, backendOf("vllm", curatedBackends(t)["vllm"]))
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Fits) != 0 {
		t.Fatalf("fits = %+v, want none: 30GB needs 4 GPUs of 9GB but 6 heads forbid TP 4", snap.Fits)
	}
}

// fitModes generates llama-3.1-8b's vLLM bf16 and fp8 presets on every curated
// family and returns the mode of each "<family>/<quantization>" fit.
func fitModes(t *testing.T, vllm BackendVersion) map[string]FitMode {
	t.Helper()
	m := llama8b()
	m.Presets = []Preset{vllmPreset("vllm-bf16", "bf16", 8192), vllmPreset("vllm-fp8", "fp8", 8192)}
	families := curatedFamilies(t, "A100", "A10G", "H100", "H200", "L4", "MI300X", "RTX-4090")
	snap, _, err := Generate(families, nil, []ModelTemplate{m}, nil, backendOf("vllm", vllm))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]FitMode{}
	for _, f := range snap.Fits {
		out[f.DeviceFamily+"/"+f.Quantization] = f.Mode
	}
	return out
}

func TestFitsRespectVendorAndDtype(t *testing.T) {
	vllm := curatedBackends(t)["vllm"]
	rocm := vllm.Images["amd"]
	delete(vllm.Images, "amd")
	want := map[string]FitMode{"A100/bf16": FitNative, "A100/fp8": FitEmulated, "A10G/fp8": FitEmulated,
		"H100/fp8": FitNative, "L4/fp8": FitNative, "RTX-4090/fp8": FitNative}
	got := fitModes(t, vllm)
	for k, mode := range want {
		if got[k] != mode {
			t.Errorf("NVIDIA-only images: %s mode = %q, want %q", k, got[k], mode)
		}
	}
	for k := range got {
		if strings.HasPrefix(k, "MI300X/") {
			t.Errorf("NVIDIA-only images: fit %s", k)
		}
	}

	vllm.Images["amd"] = rocm
	got = fitModes(t, vllm)
	if got["MI300X/bf16"] != FitNative || got["MI300X/fp8"] != FitNative {
		t.Errorf("with the ROCm image: MI300X modes bf16 %q, fp8 %q; want native", got["MI300X/bf16"], got["MI300X/fp8"])
	}

	// Emulation from 8.6 keeps the A10G (8.6) and drops the A100 (8.0).
	vllm.Quantizations["fp8"] = QuantSupport{NativeDType: "fp8", EmulatedFrom: "8.6"}
	got = fitModes(t, vllm)
	if mode, ok := got["A100/fp8"]; ok || got["A10G/fp8"] != FitEmulated {
		t.Errorf("emulated from 8.6: A100 fp8 %q (present %v), A10G fp8 %q; want absent, emulated", mode, ok, got["A10G/fp8"])
	}
}

func TestKVFP8DoublesContext(t *testing.T) {
	m := llama8b()
	kvFP8 := vllmPreset("vllm-bf16-kv-fp8", "bf16", 8192)
	kvFP8.KVCacheDType = "fp8"
	m.Presets = []Preset{vllmPreset("vllm-bf16", "bf16", 8192), kvFP8, vllmPreset("vllm-fp8", "fp8", 8192)}
	snap, _, err := Generate(curatedFamilies(t, "L4", "H100"), nil, []ModelTemplate{m}, nil, backendOf("vllm", curatedBackends(t)["vllm"]))
	if err != nil {
		t.Fatal(err)
	}
	got := fitsByKey(snap)
	auto, fp8 := got["llama-3.1-8b/L4/bf16/auto"], got["llama-3.1-8b/L4/bf16/fp8"]
	if auto.MaxContext == 0 || fp8.MaxContext < 2*auto.MaxContext || fp8.MaxContext > m.ContextMax {
		t.Errorf("L4 bf16 max context: kv fp8 %d, auto %d; want at least double, within %d", fp8.MaxContext, auto.MaxContext, m.ContextMax)
	}
	if c := got["llama-3.1-8b/H100/bf16/fp8"].MaxContext; c != m.ContextMax {
		t.Errorf("H100 bf16 kv fp8 max context = %d, want capped at %d", c, m.ContextMax)
	}
	if _, ok := got["llama-3.1-8b/L4/fp8/fp8"]; ok {
		t.Error("kv fp8 fit for the fp8 checkpoint, which no preset runs with an fp8 KV cache")
	}
}

// TestFitTopology: multi-node presets size fits across their nodes, and a
// disaggregated preset needs the model to fit on each side.
func TestFitTopology(t *testing.T) {
	// 812GB plus 8192 tokens at 516096B exceeds eight H100 (576GB budget): two
	// nodes of eight (1152GB). Sixteen L4 hold 345.6GB.
	huge := ModelTemplate{Slug: "llama-3.1-405b", Task: "chat", ParamsB: 405, ParamsActiveB: 405, DefaultContext: 8192, ContextMax: 131072,
		Quantizations: []Quantization{{Name: "bf16", Kind: "float", WeightsGB: 812}},
		Architecture:  Architecture{Layers: 126, AttentionHeads: 128, KVHeads: 8, HeadDim: 128},
		Presets: []Preset{{Name: "sglang-2node", Purpose: "standard", Quantization: "bf16", Backend: "sglang", BackendVersion: "v0.5.21", Context: 8192,
			Topology: Topology{Kind: "multi-node", Nodes: 2}}}}
	// 141.1 + 2.68GB holds on each side of two H100 (144GB), not of two L4 (43.2GB).
	pd := llama70b()
	pd.Presets = []Preset{{Name: "sglang-pd", Purpose: "standard", Quantization: "bf16", Backend: "sglang", BackendVersion: "v0.5.21", Context: 8192,
		Topology: Topology{Kind: "disaggregated", Prefill: 2, Decode: 2, Transfer: "nixl"}}}
	snap, _, err := Generate(curatedFamilies(t, "H100", "L4"), nil, []ModelTemplate{huge, pd}, nil, backendOf("sglang", curatedBackends(t)["sglang"]))
	if err != nil {
		t.Fatal(err)
	}
	got := fitsByKey(snap)
	if f := got["llama-3.1-405b/H100/bf16/auto"]; f.GPUCount != 16 || f.Nodes != 2 {
		t.Errorf("405b on H100 = %d GPUs on %d nodes, want 16 on 2", f.GPUCount, f.Nodes)
	}
	if f := got["llama-3.1-70b/H100/bf16/auto"]; f.GPUCount != 2 || f.Nodes != 1 {
		t.Errorf("70b PD on H100 = %d GPUs on %d nodes, want 2 on 1", f.GPUCount, f.Nodes)
	}
	for _, k := range []string{"llama-3.1-405b/L4/bf16/auto", "llama-3.1-70b/L4/bf16/auto"} {
		if _, ok := got[k]; ok {
			t.Errorf("fit %s, want none", k)
		}
	}
}

func TestGenerateFailsOnContradiction(t *testing.T) {
	cases := []struct {
		name string
		edit func(*BackendVersion, []DraftModel)
		want string
	}{
		{"consistent", func(*BackendVersion, []DraftModel) {}, ""},
		{"no image for the families the quantization fits", func(bv *BackendVersion, _ []DraftModel) { delete(bv.Images, "nvidia") },
			"llama-3.1-8b/vllm-bf16 on vllm v0.31.0: runs on none of the families its quantization fits (H100)"},
		// 16.1 + 60 + 1.07GB at 8192 tokens exceeds one H100's 72GB budget.
		{"draft does not fit", func(_ *BackendVersion, d []DraftModel) { d[0].WeightsGB = 60 },
			"llama-3.1-8b/vllm-bf16-eagle3 on vllm v0.31.0: weights, draft and 8192 tokens of KV exceed 1 x H100"},
	}
	for _, c := range cases {
		m, drafts, vllm := llama8b(), eagle3Drafts(), curatedBackends(t)["vllm"]
		eagle := vllmPreset("vllm-bf16-eagle3", "bf16", 8192)
		eagle.Speculative = &Speculative{Method: "eagle3", DraftModel: vllmEagle3.Slug, Tokens: 3}
		m.Presets = []Preset{vllmPreset("vllm-bf16", "bf16", 8192), eagle}
		drafts[0].WeightsGB = 1.9
		c.edit(&vllm, drafts)
		_, _, err := Generate(curatedFamilies(t, "H100"), nil, []ModelTemplate{m}, drafts, backendOf("vllm", vllm))
		if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
}

// An unparsable NVIDIA compute capability would fail every comparison and drop
// fits silently; AMD gfx targets are not numbers and stay accepted.
func TestGenerateRejectsUnparsableComputeCapability(t *testing.T) {
	cases := []struct {
		name string
		edit func(*Family, *BackendVersion)
		want string
	}{
		{"parsable, gfx on amd", func(*Family, *BackendVersion) {}, ""},
		{"family", func(f *Family, _ *BackendVersion) { f.ComputeCapability = "sm_90" }, `family H100: compute capability "sm_90" is not numeric`},
		{"backend minimum", func(_ *Family, bv *BackendVersion) { bv.MinComputeCapability = "7,5" },
			`backend vllm v0.31.0: min_compute_capability "7,5" is not numeric`},
		{"backend emulation", func(_ *Family, bv *BackendVersion) {
			bv.Quantizations["fp8"] = QuantSupport{NativeDType: "fp8", EmulatedFrom: "ampere"}
		}, `backend vllm v0.31.0: fp8 emulated_from "ampere" is not numeric`},
	}
	for _, c := range cases {
		fams, vllm := curatedFamilies(t, "H100", "MI300X"), curatedBackends(t)["vllm"]
		c.edit(&fams[0], &vllm)
		m := llama8b()
		m.Presets = []Preset{vllmPreset("vllm-bf16", "bf16", 8192)}
		_, _, err := Generate(fams, nil, []ModelTemplate{m}, nil, backendOf("vllm", vllm))
		if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
}

func TestDeploymentGuidance(t *testing.T) {
	// llama-3.1-8b bf16 at 8192 tokens (16.1 + 1.07GB) decodes on one A at
	// 0.8 x 300 / 17.17 = 14.0 tokens/s, below 20 though A is the smallest; on
	// one B at 46.6, 22 sequences at 20 (443 tokens/s per GPU); on one C at
	// 156.1, 53 sequences (1946 per GPU). Its Q4_K_M (4.9GB) would win both:
	// 40.2 on A (24GB), 63 sequences on C (2327 per GPU), but gguf fits never
	// drive guidance. llama-3.1-70b bf16 (141.1 + 2.68GB) reaches 20 only on two
	// C (31.7, 2 sequences). huge's bf16 preset fits no family and its decent
	// Q4_K_M fits do not count: both are reported.
	fam := func(name string, memory, bandwidth float64) Family {
		return Family{Name: name, Vendor: "nvidia", ComputeCapability: "9.0", DTypes: []string{"bf16"}, MemoryGB: memory, MemoryBandwidthGBs: bandwidth}
	}
	cloud := func(id, family string, gpus int) NodeType {
		return NodeType{ID: id, Family: family, GPUs: gpus, Source: "https://example.com/" + id}
	}
	withPreset := func(m ModelTemplate) ModelTemplate {
		m.Quantizations = m.Quantizations[:1]
		m.Presets = []Preset{vllmPreset("vllm-bf16", "bf16", 8192)}
		return m
	}
	withGGUF := func(m ModelTemplate, weights float64) ModelTemplate {
		m.Quantizations = append(m.Quantizations, Quantization{Name: "Q4_K_M", Kind: "gguf", WeightsGB: weights})
		m.Presets = append(m.Presets, Preset{Name: "llamacpp-q4km", Purpose: "standard", Quantization: "Q4_K_M", Backend: "llamacpp", BackendVersion: "v0.6.0", Context: 8192})
		return m
	}
	huge := withPreset(llama70b())
	huge.Slug, huge.Quantizations[0].WeightsGB = "huge", 2000
	bvs := curatedBackends(t)
	snap, report, err := Generate(
		[]Family{fam("A", 24, 300), fam("B", 48, 1000), fam("C", 80, 3350)},
		[]NodeType{cloud("cloud/b1", "B", 1), cloud("cloud/b4", "B", 4), cloud("cloud/c1", "C", 1), cloud("cloud/c8", "C", 8)},
		[]ModelTemplate{withGGUF(withPreset(llama8b()), 4.9), withPreset(llama70b()), withGGUF(huge, 30)}, nil,
		append(backendOf("vllm", bvs["vllm"]), backendOf("llamacpp", bvs["llamacpp"])...))
	if err != nil {
		t.Fatal(err)
	}
	fits := fitsByKey(snap)
	rec := func(key string, nodeTypes ...string) *Recommendation {
		f := fits[key]
		if f.Performance == nil {
			t.Fatalf("%s: no fit with a performance estimate", key)
		}
		return &Recommendation{Family: f.DeviceFamily, GPUs: f.GPUCount, Nodes: 1, Quantization: "bf16", Preset: "vllm-bf16", Context: 8192,
			NodeTypes: nodeTypes, DecodeTokensPerSec: f.Performance.DecodeTokensPerSec, AggregateTokensPerSec: f.Performance.AggregateTokensPerSec,
			Source: "estimate", Confidence: "estimate", FitKeys: []string{FitKey(f)}}
	}
	want := []Deployment{
		{Minimum: rec("llama-3.1-8b/B/bf16/auto", "cloud/b1", "cloud/b4", "other/Bx1"),
			Optimal: rec("llama-3.1-8b/C/bf16/auto", "cloud/c1", "cloud/c8", "other/Cx1")},
		{Minimum: rec("llama-3.1-70b/C/bf16/auto", "cloud/c8", "other/Cx2"), Optimal: rec("llama-3.1-70b/C/bf16/auto", "cloud/c8", "other/Cx2")},
		{},
	}
	for i, w := range want {
		if got := snap.Models[i].Deployment; !reflect.DeepEqual(got, w) {
			gj, _ := json.Marshal(got)
			wj, _ := json.Marshal(w)
			t.Errorf("%s deployment = %s\nwant %s", snap.Models[i].Slug, gj, wj)
		}
	}
	for _, k := range []string{"llama-3.1-8b/A/Q4_K_M/auto", "llama-3.1-8b/C/Q4_K_M/auto", "huge/B/Q4_K_M/auto"} {
		if p := fits[k].Performance; p == nil || p.ConcurrentSequences == 0 {
			t.Errorf("%s: performance %+v, want a decent estimate that guidance ignores", k, p)
		}
	}
	wantReport := []string{"huge/vllm-bf16: fits no family", "huge: no deployment minimum: no float or fp8 fit decodes 20 tokens/s per sequence"}
	if !slices.Equal(report, wantReport) {
		t.Errorf("report = %q, want %q", report, wantReport)
	}
}

func TestGenerateRejectsUnresolvedPresetsAndUnpinnedSources(t *testing.T) {
	backends := []Backend{{Name: "vllm", Versions: []BackendVersion{{Version: "v1", Quantizations: map[string]QuantSupport{"float": {NativeDType: "bf16"}}}}}}
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
		m.Presets = []Preset{{Name: "vllm-bf16", Purpose: "standard", Quantization: "bf16", Backend: "vllm", BackendVersion: "v1"}}
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
	m := ModelTemplate{Slug: "gemma-2-9b", Task: "chat", DefaultContext: 8192, ContextMax: 8192, Quantizations: []Quantization{{Name: "bf16", Kind: "float"}},
		Presets: []Preset{{Name: "vllm-bf16", Purpose: "standard", Quantization: "bf16", Backend: "vllm", BackendVersion: "v0.31.0", ToolParser: "llama3"}}}
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
	m.Presets = []Preset{{Name: "eagle3", Purpose: "standard", Quantization: "bf16", Backend: "vllm", BackendVersion: "v0.31.0",
		Speculative: &Speculative{Method: "eagle3", DraftModel: vllmEagle3.Slug, Tokens: 3}}}
	wantValidate(t, "vllm preset with the vllm draft", curatedSnap(t, m, eagle3Drafts()...), "")
	m.Presets[0].Backend, m.Presets[0].BackendVersion = "sglang", "v0.5.21"
	wantValidate(t, "sglang preset with the vllm draft", curatedSnap(t, m, eagle3Drafts()...),
		`llama-3.1-8b/eagle3 on sglang v0.5.21: draft model "eagle3-llama-3.1-8b-redhat" does not load on sglang`)
}

func TestValidateRejectsContextAboveMax(t *testing.T) {
	m := llama8b()
	m.Presets = []Preset{{Name: "vllm-bf16", Purpose: "standard", Quantization: "bf16", Backend: "vllm", BackendVersion: "v0.31.0", Context: 131072}}
	wantValidate(t, "context at max", curatedSnap(t, m), "")
	m.Presets[0].Context = 131073
	wantValidate(t, "context above max", curatedSnap(t, m), "llama-3.1-8b/vllm-bf16 on vllm v0.31.0: context 131073 above context_max 131072")
}

func TestValidateRejectsUnknownTopologyTransfer(t *testing.T) {
	m := llama70b()
	m.Presets = []Preset{{Name: "sglang-pd", Purpose: "standard", Quantization: "bf16", Backend: "sglang", BackendVersion: "v0.5.21",
		Topology: Topology{Kind: "disaggregated", Prefill: 2, Decode: 2, Transfer: "nixl"}}}
	wantValidate(t, "nixl", curatedSnap(t, m), "")
	m.Presets[0].Topology.Transfer = "ucx"
	wantValidate(t, "ucx", curatedSnap(t, m), `llama-3.1-70b/sglang-pd on sglang v0.5.21: has no kv transfer "ucx"`)
}

// Each case breaks one rule the renderer relies on but cannot check itself, or
// a closed vocabulary.
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
		{"multi-node on two nodes", func(m *ModelTemplate, _ []DraftModel) { m.Presets[0].Topology = Topology{Kind: "multi-node", Nodes: 2} }, ""},
		{"multi-node on one node", func(m *ModelTemplate, _ []DraftModel) { m.Presets[0].Topology = Topology{Kind: "multi-node", Nodes: 1} },
			"llama-3.1-8b/vllm-bf16 on vllm v0.31.0: multi-node topology needs nodes >= 2"},
		{"quantization kind the backend does not run", func(m *ModelTemplate, _ []DraftModel) {
			m.Quantizations = append(m.Quantizations, Quantization{Name: "Q4_K_M", Kind: "gguf", WeightsGB: 4.9})
			m.Presets[0].Quantization = "Q4_K_M"
		}, `llama-3.1-8b/vllm-bf16 on vllm v0.31.0: quantization kind "gguf" not supported`},
		{"every capability", func(m *ModelTemplate, _ []DraftModel) {
			m.Capabilities = []string{"tools", "reasoning", "vision", "long-context", "json-schema"}
		}, ""},
		{"task outside the vocabulary", func(m *ModelTemplate, _ []DraftModel) { m.Task = "chatt" }, `llama-3.1-8b: task "chatt", want one of`},
		{"no task", func(m *ModelTemplate, _ []DraftModel) { m.Task = "" }, `llama-3.1-8b: task "", want one of`},
		{"capability outside the vocabulary", func(m *ModelTemplate, _ []DraftModel) { m.Capabilities = []string{"tools", "function-calling"} },
			`llama-3.1-8b: capability "function-calling", want one of`},
		{"purpose outside the vocabulary", func(m *ModelTemplate, _ []DraftModel) { m.Presets[1].Purpose = "fast" },
			`llama-3.1-8b: preset "eagle3": purpose "fast", want one of`},
		{"long-context purpose", func(m *ModelTemplate, _ []DraftModel) { m.Presets[0].Purpose = "long-context" }, ""},
		{"requires_dtype other than the kind's native dtype", func(m *ModelTemplate, _ []DraftModel) { m.Quantizations[0].RequiresDType = "fp16" },
			`llama-3.1-8b/vllm-bf16 on vllm v0.31.0: quantization bf16 requires_dtype "fp16", but its kind float is native "bf16"`},
		{"no requires_dtype", func(m *ModelTemplate, _ []DraftModel) { m.Quantizations[0].RequiresDType = "" }, ""},
	}
	for _, c := range cases {
		m, drafts := llama8b(), eagle3Drafts()
		m.Presets = []Preset{
			{Name: "vllm-bf16", Purpose: "standard", Quantization: "bf16", Backend: "vllm", BackendVersion: "v0.31.0", Context: 8192,
				ToolParser: "llama3", ChatTemplate: "llama3.1-tools"},
			{Name: "eagle3", Purpose: "standard", Quantization: "bf16", Backend: "vllm", BackendVersion: "v0.31.0",
				Speculative: &Speculative{Method: "eagle3", DraftModel: vllmEagle3.Slug, Tokens: 3}},
		}
		c.edit(&m, drafts)
		wantValidate(t, c.name, curatedSnap(t, m, drafts...), c.want)
	}
}

// A chat template path is per vendor image, so Validate renders a preset once
// per vendor it runs on.
func TestValidateRendersEveryVendor(t *testing.T) {
	m := llama8b()
	m.Presets = []Preset{{Name: "vllm-bf16", Purpose: "standard", Quantization: "bf16", Backend: "vllm", BackendVersion: "v0.31.0",
		ToolParser: "llama3", ChatTemplate: "llama3.1-tools"}}
	snap := curatedSnap(t, m)
	wantValidate(t, "a path per vendor", snap, "")
	for _, b := range snap.Backends {
		if b.Name == "vllm" {
			delete(b.Versions[0].Flags.ChatTemplates["llama3.1-tools"], "amd")
		}
	}
	wantValidate(t, "no amd path", snap, `llama-3.1-8b/vllm-bf16 on vllm v0.31.0: has no chat template "llama3.1-tools" for vendor "amd"`)
	snap.Models[0].Presets[0].Vendors = []string{"nvidia"}
	wantValidate(t, "nvidia only", snap, "")
}

// Regenerating the committed data must reproduce the embedded snapshot: the
// generator is deterministic and snapshots/v4 is not stale.
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
	committed, err := SnapshotV4()
	if err != nil {
		t.Fatal(err)
	}
	snap.Version = committed.Version
	if snap.Digest() != committed.Digest() {
		t.Fatal("snapshots/v4/catalog.json is stale: run go run ./cmd/catalog-generate")
	}
}
