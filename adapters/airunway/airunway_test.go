package airunway

import (
	"bytes"
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
	snap, err := catalog.SnapshotV3()
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

// On the committed snapshot the only skips are SGLang presets in recipes.
func TestCoverage(t *testing.T) {
	a, snap := committed(t)
	for _, format := range a.Formats() {
		e, _ := export(t, a, snap, format)
		if err := adapters.Coverage(a, snap, format, e); err != nil {
			t.Error(err)
		}
		var want []adapters.Skip
		for _, r := range adapters.Presets(snap) {
			if format == FormatRecipes && r.Preset.Backend == "sglang" {
				want = append(want, adapters.Skip{PresetKey: r.Key(), Reason: SkipNotVLLM})
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
	snap := catalog.CatalogSnapshot{
		Models: []catalog.ModelTemplate{{
			Slug: "m", DefaultContext: 4096, ContextMax: 8192,
			Quantizations: []catalog.Quantization{
				{Name: "bf16", Sources: []catalog.ModelSource{ms, hf("org/m")}},
				{Name: "fp8", Sources: []catalog.ModelSource{ms}},
				{Name: "int4", Sources: []catalog.ModelSource{hf("org/m-int4")}},
			},
			Presets: []catalog.Preset{
				{Name: "vllm-bf16", Quantization: "bf16", Backend: "vllm", BackendVersion: "v1"},
				{Name: "vllm-bf16-tools", Quantization: "bf16", Backend: "vllm", BackendVersion: "v1"},
				{Name: "sglang-bf16", Quantization: "bf16", Backend: "sglang", BackendVersion: "v1"},
				{Name: "vllm-fp8", Quantization: "fp8", Backend: "vllm", BackendVersion: "v1"},
				{Name: "vllm-int4", Quantization: "int4", Backend: "vllm", BackendVersion: "v1"},
				{Name: "tgi-bf16", Quantization: "bf16", Backend: "tgi", BackendVersion: "v1"},
			},
		}},
		Fits: []catalog.ModelFit{{ModelSlug: "m", Quantization: "bf16", DeviceFamily: "H100", GPUCount: 1, MaxContext: 8192}},
		Backends: []catalog.Backend{
			{Name: "vllm", Versions: []catalog.BackendVersion{{Version: "v1", Image: "vllm:v1"}}},
			{Name: "sglang", Versions: []catalog.BackendVersion{{Version: "v1", Image: "sglang:v1"}}},
			{Name: "tgi", Versions: []catalog.BackendVersion{{Version: "v1", Image: "tgi:v1"}}},
		},
	}
	key := func(p string) adapters.PresetKey { return adapters.PresetKey{Model: "m", Preset: p} }
	for format, want := range map[string]adapters.Export{
		FormatRecipes: {
			Exported: []adapters.PresetKey{key("vllm-bf16")},
			Skipped: []adapters.Skip{
				{PresetKey: key("vllm-bf16-tools"), Reason: SkipRepoTaken},
				{PresetKey: key("sglang-bf16"), Reason: SkipNotVLLM},
				{PresetKey: key("vllm-fp8"), Reason: SkipRecipeNoHF},
				{PresetKey: key("vllm-int4"), Reason: SkipNoFit},
				{PresetKey: key("tgi-bf16"), Reason: SkipNotVLLM},
			},
		},
		FormatDeployments: {
			Exported: []adapters.PresetKey{key("vllm-bf16"), key("vllm-bf16-tools"), key("sglang-bf16")},
			Skipped: []adapters.Skip{
				{PresetKey: key("vllm-fp8"), Reason: SkipNoHF},
				{PresetKey: key("vllm-int4"), Reason: SkipNoFit},
				{PresetKey: key("tgi-bf16"), Reason: SkipBackend},
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
// the pinned image with the preset's arguments at the model's default context.
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
		rel, _ := adapters.BackendVersion(snap, r.Preset.Backend, r.Preset.BackendVersion)
		fit, _ := adapters.ReferenceFit(snap, r.Model.Slug, r.Preset.Quantization, adapters.Options{})
		var rc recipe
		decode(t, files, src.Repo+".json", &rc)
		cmd := rc.RecommendedCommand
		want := append(append([]string{"vllm", "serve", src.Repo}, r.Preset.Args...), "--max-model-len="+strconv.Itoa(r.Model.DefaultContext))
		if rc.HFID != src.Repo || !slices.Equal(cmd.Argv, want) || cmd.DockerImage != rel.Image || cmd.HardwareProfile.GPUCount != fit.GPUCount {
			t.Errorf("%s/%s: recipe %s runs %v on %s with %d GPUs, want %v on %s with %d",
				r.Model.Slug, r.Preset.Name, rc.HFID, cmd.Argv, cmd.DockerImage, cmd.HardwareProfile.GPUCount, want, rel.Image, fit.GPUCount)
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
// SGLang on Dynamo with Dynamo's own image and an explicit tensor-parallel
// size, vLLM on the vLLM provider with the pinned image.
func TestDeployments(t *testing.T) {
	a, snap := committed(t)
	e, files := export(t, a, snap, FormatDeployments)
	for _, r := range adapters.Presets(snap) {
		k := r.Key()
		if !slices.Contains(e.Exported, k) {
			continue
		}
		src, _ := r.Source(catalog.SourceHuggingFace)
		rel, _ := adapters.BackendVersion(snap, r.Preset.Backend, r.Preset.BackendVersion)
		fit, _ := adapters.ReferenceFit(snap, r.Model.Slug, r.Preset.Quantization, adapters.Options{})
		var md modelDeployment
		var body apiBody
		decode(t, files, "deployments/"+k.Model+"/"+k.Preset+".yaml", &md)
		decode(t, files, "api/"+k.Model+"/"+k.Preset+".json", &body)
		s, en := md.Spec, md.Spec.Engine
		if s.Model.ID != src.Repo || en.Type != r.Preset.Backend || en.ContextLength != r.Model.DefaultContext || s.Resources.GPU.Count != fit.GPUCount {
			t.Errorf("%s: runs %s on %s at context %d with %d GPUs", md.Metadata.Name, s.Model.ID, en.Type, en.ContextLength, s.Resources.GPU.Count)
		}
		for _, flag := range []string{"max-model-len", "context-length"} {
			if _, ok := en.Args[flag]; ok {
				t.Errorf("%s: context passed as arg %s instead of contextLength", md.Metadata.Name, flag)
			}
		}
		switch r.Preset.Backend {
		case "sglang":
			if s.Provider.Name != "dynamo" || md.Metadata.Annotations[RequiredProviderAnnotation] != "dynamo" || en.Image != "" {
				t.Errorf("%s: SGLang on provider %q, annotated %q, image %q; AI Runway runs SGLang only on Dynamo with its own image",
					md.Metadata.Name, s.Provider.Name, md.Metadata.Annotations[RequiredProviderAnnotation], en.Image)
			}
			if tp := en.Args["tp-size"]; fit.GPUCount > 1 && tp != strconv.Itoa(fit.GPUCount) {
				t.Errorf("%s: tp-size %q on %d GPUs; Dynamo does not derive it", md.Metadata.Name, tp, fit.GPUCount)
			}
		case "vllm":
			if s.Provider.Name != "vllm" || md.Metadata.Annotations[RequiredProviderAnnotation] != "vllm" || en.Image != rel.Image {
				t.Errorf("%s: vLLM on provider %q with image %q, want vllm with %s", md.Metadata.Name, s.Provider.Name, en.Image, rel.Image)
			}
		}
		if body.Name != md.Metadata.Name || body.ModelID != s.Model.ID || body.Engine != en.Type || body.Provider != s.Provider.Name ||
			body.ImageRef != en.Image || body.ContextLength != en.ContextLength || body.Resources.GPU != s.Resources.GPU.Count ||
			!maps.Equal(body.EngineArgs, en.Args) || !slices.Equal(body.EngineExtraArgs, en.ExtraArgs) {
			t.Errorf("%s: API body %+v does not match its manifest", md.Metadata.Name, body)
		}
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
