package airunway

import (
	"bytes"
	"cmp"
	"encoding/json"
	"flag"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	catalog "github.com/fishman/neocloud-catalog"
	"github.com/fishman/neocloud-catalog/adapters"
)

var update = flag.Bool("update", false, "rewrite testdata from the committed snapshot")

func committed(t *testing.T) (adapters.Adapter, catalog.CatalogSnapshot) {
	t.Helper()
	a, ok := adapters.Get("airunway")
	if !ok {
		t.Fatal("airunway is not registered")
	}
	snap, err := catalog.SnapshotV4()
	if err != nil {
		t.Fatal(err)
	}
	return a, snap
}

func export(t *testing.T, a adapters.Adapter, snap catalog.CatalogSnapshot, format string) (adapters.Export, map[string][]byte) {
	t.Helper()
	e, err := a.Export(snap, format, adapters.Options{})
	if err != nil {
		t.Fatalf("%s: %v", format, err)
	}
	files := map[string][]byte{}
	for _, art := range e.Artifacts {
		files[art.Path] = art.Body
	}
	return e, files
}

func decode(t *testing.T, files map[string][]byte, path string, v any) {
	t.Helper()
	b, ok := files[path]
	if !ok {
		t.Fatalf("%s not exported", path)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

func key(model, preset string) adapters.PresetKey {
	return adapters.PresetKey{Model: model, Preset: preset}
}

// nvidia is the only GPU vendor AI Runway's deployment API requests.
var nvidia = []string{"nvidia"}

func TestGolden(t *testing.T) {
	a, snap := committed(t)
	for _, format := range a.Formats() {
		_, files := export(t, a, snap, format)
		dir := filepath.Join("testdata", format)
		if *update {
			if err := os.RemoveAll(dir); err != nil {
				t.Fatal(err)
			}
			for path, body := range files {
				p := filepath.Join(dir, filepath.FromSlash(path))
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, body, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			continue
		}
		err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(dir, p)
			rel = filepath.ToSlash(rel)
			want, _ := os.ReadFile(p)
			got, ok := files[rel]
			switch {
			case !ok:
				t.Errorf("%s/%s is no longer exported", format, rel)
			case !bytes.Equal(got, want):
				t.Errorf("%s/%s differs from testdata (review, then go test ./airunway -update)", format, rel)
			}
			delete(files, rel)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		for path := range files {
			t.Errorf("%s/%s is missing from testdata (go test ./airunway -update)", format, path)
		}
	}
}

// On the committed snapshot recipes skip every non-vLLM preset, a second vLLM
// preset of a repo and the multi-node one; deployments skip llama.cpp and
// multi-node and export the SGLang disaggregated preset.
func TestCoverage(t *testing.T) {
	a, snap := committed(t)
	special := map[string]map[adapters.PresetKey]string{
		FormatRecipes: {
			key("llama-3.1-8b", "vllm-bf16-eagle3"): SkipRepoTaken,
			key("llama-3.1-70b", "vllm-bf16-2node"): SkipRecipeTopology,
		},
		FormatDeployments: {
			key("llama-3.1-8b", "llamacpp-q4km"):    SkipLlamaCpp,
			key("llama-3.1-70b", "vllm-bf16-2node"): SkipMultiNode,
			key("qwen-2.5-14b", "llamacpp-q4km"):    SkipLlamaCpp,
		},
	}
	for _, format := range a.Formats() {
		e, _ := export(t, a, snap, format)
		if err := adapters.Coverage(a, snap, format, e); err != nil {
			t.Error(err)
		}
		var want []adapters.Skip
		for _, r := range adapters.Presets(snap) {
			reason := special[format][r.Key()]
			if format == FormatRecipes && r.Preset.Backend != "vllm" {
				reason = SkipNotVLLM
			}
			if reason != "" {
				want = append(want, adapters.Skip{PresetKey: r.Key(), Reason: reason})
			}
		}
		if !slices.Equal(e.Skipped, want) {
			t.Errorf("%s skipped %v, want %v", format, e.Skipped, want)
		}
	}
}

func TestSkips(t *testing.T) {
	hf := func(repo string) catalog.ModelSource {
		return catalog.ModelSource{Type: catalog.SourceHuggingFace, Repo: repo}
	}
	ms := catalog.ModelSource{Type: catalog.SourceModelScope, Repo: "org/m"}
	images := map[string]string{"nvidia": "n:v1", "amd": "a:v1"}
	pd := func(prefill, decode int) catalog.Topology {
		return catalog.Topology{Kind: "disaggregated", Prefill: prefill, Decode: decode, Transfer: "nixl"}
	}
	fit := func(quant, family string) catalog.ModelFit {
		return catalog.ModelFit{ModelSlug: "m", Quantization: quant, KVCacheDType: "auto", DeviceFamily: family, Mode: catalog.FitNative, GPUCount: 1, MaxContext: 8192}
	}
	snap := catalog.CatalogSnapshot{
		Families: []catalog.Family{{Name: "H100", Vendor: "nvidia"}, {Name: "MI300X", Vendor: "amd"}},
		Models: []catalog.ModelTemplate{{
			Slug: "m", DefaultContext: 4096, ContextMax: 8192,
			Quantizations: []catalog.Quantization{
				{Name: "bf16", Sources: []catalog.ModelSource{ms, hf("org/m")}},
				{Name: "fp8", Sources: []catalog.ModelSource{ms}},
				{Name: "int4", Sources: []catalog.ModelSource{hf("org/m-int4")}},
				{Name: "w8", Sources: []catalog.ModelSource{hf("org/m-w8")}},
				{Name: "q4", Sources: []catalog.ModelSource{hf("org/m-gguf")}},
			},
			Presets: []catalog.Preset{
				{Name: "vllm-bf16", Quantization: "bf16", Backend: "vllm", BackendVersion: "v1"},
				{Name: "vllm-bf16-tools", Quantization: "bf16", Backend: "vllm", BackendVersion: "v1"},
				{Name: "sglang-bf16", Quantization: "bf16", Backend: "sglang", BackendVersion: "v1"},
				{Name: "vllm-fp8", Quantization: "fp8", Backend: "vllm", BackendVersion: "v1"},
				{Name: "vllm-int4", Quantization: "int4", Backend: "vllm", BackendVersion: "v1"},
				{Name: "vllm-w8", Quantization: "w8", Backend: "vllm", BackendVersion: "v1"},
				{Name: "tgi-bf16", Quantization: "bf16", Backend: "tgi", BackendVersion: "v1"},
				{Name: "llamacpp-q4", Quantization: "q4", Backend: "llamacpp", BackendVersion: "v1"},
				{Name: "vllm-bf16-2node", Quantization: "bf16", Backend: "vllm", BackendVersion: "v1", Topology: catalog.Topology{Kind: "multi-node", Nodes: 2}},
				{Name: "vllm-bf16-pd", Quantization: "bf16", Backend: "vllm", BackendVersion: "v1", Topology: pd(1, 1)},
				{Name: "sglang-bf16-pd", Quantization: "bf16", Backend: "sglang", BackendVersion: "v1", Topology: pd(1, 1)},
				{Name: "sglang-bf16-pd-uneven", Quantization: "bf16", Backend: "sglang", BackendVersion: "v1", Topology: pd(1, 2)},
			},
		}},
		// w8 fits only an AMD family, which AI Runway's deployment API cannot request.
		Fits: []catalog.ModelFit{fit("bf16", "H100"), fit("bf16", "MI300X"), fit("w8", "MI300X"), fit("q4", "H100")},
		Backends: []catalog.Backend{
			{Name: "vllm", Versions: []catalog.BackendVersion{{Version: "v1", Images: images,
				Flags: catalog.FlagTable{Context: "--max-model-len", TensorParallel: "--tensor-parallel-size", Disaggregation: "json:--kv-transfer-config",
					KVTransfers: map[string]string{"nixl": "NixlConnector"}, MultiNode: "vllm"}}}},
			{Name: "sglang", Versions: []catalog.BackendVersion{{Version: "v1", Images: images,
				Flags: catalog.FlagTable{Context: "--context-length", TensorParallel: "--tp-size", Disaggregation: "sglang", KVTransfers: map[string]string{"nixl": "nixl"}}}}},
			{Name: "llamacpp", Versions: []catalog.BackendVersion{{Version: "v1", Images: images, Flags: catalog.FlagTable{Context: "-c", SeparateValues: true}}}},
			{Name: "tgi", Versions: []catalog.BackendVersion{{Version: "v1", Images: images}}},
		},
	}
	k := func(p string) adapters.PresetKey { return key("m", p) }
	for format, want := range map[string]adapters.Export{
		FormatRecipes: {
			Exported: []adapters.PresetKey{k("vllm-bf16")},
			Skipped: []adapters.Skip{
				{PresetKey: k("vllm-bf16-tools"), Reason: SkipRepoTaken},
				{PresetKey: k("sglang-bf16"), Reason: SkipNotVLLM},
				{PresetKey: k("vllm-fp8"), Reason: SkipRecipeNoHF},
				{PresetKey: k("vllm-int4"), Reason: SkipNoFit},
				{PresetKey: k("vllm-w8"), Reason: SkipNoFit},
				{PresetKey: k("tgi-bf16"), Reason: SkipNotVLLM},
				{PresetKey: k("llamacpp-q4"), Reason: SkipNotVLLM},
				{PresetKey: k("vllm-bf16-2node"), Reason: SkipRecipeTopology},
				{PresetKey: k("vllm-bf16-pd"), Reason: SkipRecipeTopology},
				{PresetKey: k("sglang-bf16-pd"), Reason: SkipNotVLLM},
				{PresetKey: k("sglang-bf16-pd-uneven"), Reason: SkipNotVLLM},
			},
		},
		FormatDeployments: {
			Exported: []adapters.PresetKey{k("vllm-bf16"), k("vllm-bf16-tools"), k("sglang-bf16"), k("sglang-bf16-pd")},
			Skipped: []adapters.Skip{
				{PresetKey: k("vllm-fp8"), Reason: SkipNoHF},
				{PresetKey: k("vllm-int4"), Reason: SkipNoFit},
				{PresetKey: k("vllm-w8"), Reason: SkipNoFit},
				{PresetKey: k("tgi-bf16"), Reason: SkipBackend},
				{PresetKey: k("llamacpp-q4"), Reason: SkipLlamaCpp},
				{PresetKey: k("vllm-bf16-2node"), Reason: SkipMultiNode},
				{PresetKey: k("vllm-bf16-pd"), Reason: SkipDisaggregated},
				// Dynamo gives both roles one set of engine args, so the roles'
				// tensor-parallel sizes must agree.
				{PresetKey: k("sglang-bf16-pd-uneven"), Reason: SkipDisaggregated},
			},
		},
	} {
		e, err := adapter{}.Export(snap, format, adapters.Options{})
		if err != nil {
			t.Fatalf("%s: %v", format, err)
		}
		if err := adapters.Coverage(adapter{}, snap, format, e); err != nil {
			t.Error(err)
		}
		if !slices.Equal(e.Exported, want.Exported) || !slices.Equal(e.Skipped, want.Skipped) {
			t.Errorf("%s exported %v skipped %v, want %v and %v", format, e.Exported, e.Skipped, want.Exported, want.Skipped)
		}
	}
}

// Every vLLM preset is served by a recipe at its own repo whose command runs
// the pinned NVIDIA image with Render's arguments; the GPU count, which the
// Direct vLLM provider turns into tensor parallelism, comes from the reference
// fit.
func TestRecipes(t *testing.T) {
	a, snap := committed(t)
	e, files := export(t, a, snap, FormatRecipes)
	var index []indexEntry
	decode(t, files, "models.json", &index)
	if len(index) != len(e.Exported) {
		t.Errorf("models.json lists %d recipes, %d presets exported", len(index), len(e.Exported))
	}
	for _, r := range adapters.Presets(snap) {
		if !slices.Contains(e.Exported, r.Key()) {
			continue
		}
		src, _ := r.Source(catalog.SourceHuggingFace)
		bv, _ := adapters.BackendVersion(snap, r.Preset.Backend, r.Preset.BackendVersion)
		fit, _ := adapters.ReferenceFit(snap, r, nvidia, adapters.Options{})
		args, err := catalog.Render(r.Model, r.Preset, bv, catalog.ModelFit{}, adapters.Draft(snap, r.Preset), gpuVendor)
		if err != nil {
			t.Fatal(err)
		}
		var rc recipe
		decode(t, files, src.Repo+".json", &rc)
		cmd := rc.RecommendedCommand
		want := append([]string{"vllm", "serve", src.Repo}, args...)
		if rc.HFID != src.Repo || !slices.Equal(cmd.Argv, want) || cmd.DockerImage != bv.Images["nvidia"] || cmd.HardwareProfile.GPUCount != fit.GPUCount {
			t.Errorf("%s/%s: recipe %s runs %v on %s with %d GPUs, want %v on %s with %d",
				r.Model.Slug, r.Preset.Name, rc.HFID, cmd.Argv, cmd.DockerImage, cmd.HardwareProfile.GPUCount, want, bv.Images["nvidia"], fit.GPUCount)
		}
		for hw := range rc.Meta.Hardware {
			if hw == "mi300x" {
				t.Errorf("%s lists %s, a family of a vendor AI Runway does not request", rc.HFID, hw)
			}
		}
		if base := rc.Meta.DerivedFrom; base != "" {
			var b recipe
			decode(t, files, base+".json", &b)
			if v := b.Variants[r.Preset.Quantization]; v.ModelID != src.Repo || v.JSON != src.Repo+".json" {
				t.Errorf("base %s lists variant %s as %+v, want %s", base, r.Preset.Quantization, v, src.Repo)
			}
		}
	}
}

// A deployment runs on AI Runway only on the provider that runs its engine:
// SGLang on Dynamo with Dynamo's own image and Render's tensor-parallel size,
// vLLM on the vLLM provider with the pinned image, which derives tensor
// parallelism from the GPU count. The context is contextLength, never an arg.
// A disaggregated SGLang preset uses Dynamo's disaggregated mode, which adds
// each worker's role itself.
func TestDeployments(t *testing.T) {
	a, snap := committed(t)
	e, files := export(t, a, snap, FormatDeployments)
	disaggregated := 0
	for _, r := range adapters.Presets(snap) {
		k := r.Key()
		if !slices.Contains(e.Exported, k) {
			continue
		}
		src, _ := r.Source(catalog.SourceHuggingFace)
		bv, _ := adapters.BackendVersion(snap, r.Preset.Backend, r.Preset.BackendVersion)
		fit, _ := adapters.ReferenceFit(snap, r, nvidia, adapters.Options{})
		var md modelDeployment
		var body apiBody
		decode(t, files, "deployments/"+k.Model+"/"+k.Preset+".yaml", &md)
		decode(t, files, "api/"+k.Model+"/"+k.Preset+".json", &body)
		s, en := md.Spec, md.Spec.Engine
		if s.Model.ID != src.Repo || en.Type != r.Preset.Backend || en.ContextLength != cmp.Or(r.Preset.Context, r.Model.DefaultContext) {
			t.Errorf("%s: runs %s on %s at context %d", md.Metadata.Name, s.Model.ID, en.Type, en.ContextLength)
		}
		for _, flag := range []string{"max-model-len", "context-length"} {
			if _, ok := en.Args[flag]; ok {
				t.Errorf("%s: context passed as arg %s instead of contextLength", md.Metadata.Name, flag)
			}
		}
		rp := r.Preset
		rp.Context = 0
		draft := adapters.Draft(snap, rp)
		var tokens []string
		var err error
		switch r.Preset.Backend {
		case "sglang":
			if s.Provider.Name != "dynamo" || md.Metadata.Annotations[RequiredProviderAnnotation] != "dynamo" || en.Image != "" {
				t.Errorf("%s: SGLang on provider %q, annotated %q, image %q; AI Runway runs SGLang only on Dynamo with its own image",
					md.Metadata.Name, s.Provider.Name, md.Metadata.Annotations[RequiredProviderAnnotation], en.Image)
			}
			tokens, err = catalog.Render(r.Model, rp, bv, fit, draft, gpuVendor)
		case "vllm":
			if s.Provider.Name != "vllm" || md.Metadata.Annotations[RequiredProviderAnnotation] != "vllm" || en.Image != bv.Images["nvidia"] {
				t.Errorf("%s: vLLM on provider %q with image %q, want vllm with %s", md.Metadata.Name, s.Provider.Name, en.Image, bv.Images["nvidia"])
			}
			tokens, err = catalog.Render(r.Model, rp, bv, catalog.ModelFit{}, draft, gpuVendor)
		}
		if top := r.Preset.Topology; top.Kind == "disaggregated" {
			disaggregated++
			if s.Serving == nil || s.Serving.Mode != "disaggregated" || s.Resources != nil || s.Scaling == nil ||
				s.Scaling.Prefill.GPU.Count != top.Prefill || s.Scaling.Decode.GPU.Count != top.Decode {
				t.Errorf("%s: serving %+v scaling %+v resources %+v, want disaggregated with %d prefill and %d decode GPUs",
					md.Metadata.Name, s.Serving, s.Scaling, s.Resources, top.Prefill, top.Decode)
			}
			if _, ok := en.Args["disaggregation-mode"]; ok || en.Args["disaggregation-transfer-backend"] != bv.Flags.KVTransfers[top.Transfer] || en.Args["tp-size"] != strconv.Itoa(top.Prefill) {
				t.Errorf("%s: args %v, want the transfer backend and tp-size %d without a role", md.Metadata.Name, en.Args, top.Prefill)
			}
			tokens, err = catalog.RenderRole(r.Model, rp, bv, fit, draft, gpuVendor, "prefill", 0)
			tokens = slices.DeleteFunc(tokens, func(t string) bool { return t == "--disaggregation-mode=prefill" })
		} else if s.Serving != nil || s.Scaling != nil || s.Resources == nil || s.Resources.GPU.Count != fit.GPUCount {
			t.Errorf("%s: serving %+v scaling %+v resources %+v, want aggregated on %d GPUs", md.Metadata.Name, s.Serving, s.Scaling, s.Resources, fit.GPUCount)
		}
		if err != nil {
			t.Fatal(err)
		}
		if args, extra := engineArgs(tokens); !maps.Equal(en.Args, args) || !slices.Equal(en.ExtraArgs, extra) {
			t.Errorf("%s: args %v extra %q, want %v %q", md.Metadata.Name, en.Args, en.ExtraArgs, args, extra)
		}
		gpus, prefill, decode := 0, 0, 0
		if s.Resources != nil {
			gpus = s.Resources.GPU.Count
		}
		if s.Scaling != nil {
			prefill, decode = s.Scaling.Prefill.GPU.Count, s.Scaling.Decode.GPU.Count
		}
		mode := ""
		if s.Serving != nil {
			mode = s.Serving.Mode
		}
		bodyGPUs := 0
		if body.Resources != nil {
			bodyGPUs = body.Resources.GPU
		}
		if body.Name != md.Metadata.Name || body.ModelID != s.Model.ID || body.Engine != en.Type || body.Provider != s.Provider.Name ||
			body.ImageRef != en.Image || body.ContextLength != en.ContextLength || bodyGPUs != gpus || body.Mode != mode ||
			body.PrefillGpus != prefill || body.DecodeGpus != decode ||
			!maps.Equal(body.EngineArgs, en.Args) || !slices.Equal(body.EngineExtraArgs, en.ExtraArgs) {
			t.Errorf("%s: API body %+v does not match its manifest", md.Metadata.Name, body)
		}
	}
	if disaggregated == 0 {
		t.Error("no disaggregated preset exported")
	}
}

func TestEngineArgs(t *testing.T) {
	for _, c := range []struct {
		in    []string
		args  map[string]string
		extra []string
	}{
		{[]string{"--enable-auto-tool-choice", "--tool-call-parser", "llama3_json", "--max-num-seqs=8"},
			map[string]string{"enable-auto-tool-choice": "", "tool-call-parser": "llama3_json", "max-num-seqs": "8"}, nil},
		{[]string{"--lora-modules", "a=x", "--lora-modules", "b=y"}, nil, []string{"--lora-modules", "a=x", "--lora-modules", "b=y"}},
		{[]string{"--chat-template=", "--x"}, nil, []string{"--chat-template=", "--x"}},
	} {
		args, extra := engineArgs(c.in)
		if !maps.Equal(args, c.args) || !slices.Equal(extra, c.extra) {
			t.Errorf("engineArgs(%q) = %v, %q; want %v, %q", c.in, args, extra, c.args, c.extra)
		}
	}
}
