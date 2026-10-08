package adapters

import (
	"cmp"
	"slices"
	"strings"
	"testing"

	catalog "github.com/fishman/neocloud-catalog"
)

type fake struct{ reasons []string }

func (fake) Name() string                                                    { return "fake" }
func (fake) Capabilities() Capabilities                                      { return Capabilities{} }
func (fake) Formats() []string                                               { return []string{"f"} }
func (f fake) SkipReasons(string) []string                                   { return f.reasons }
func (fake) Export(catalog.CatalogSnapshot, string, Options) (Export, error) { return Export{}, nil }

func snapshot() catalog.CatalogSnapshot {
	return catalog.CatalogSnapshot{Models: []catalog.ModelTemplate{
		{Slug: "m", Presets: []catalog.Preset{{Name: "vllm-bf16"}, {Name: "sglang-bf16"}}},
	}}
}

func TestCoverage(t *testing.T) {
	a := fake{reasons: []string{"no sglang"}}
	v := PresetKey{"m", "vllm-bf16"}
	s := PresetKey{"m", "sglang-bf16"}
	for name, c := range map[string]struct {
		e       Export
		wantErr string
	}{
		"exported and documented skip": {Export{Exported: []PresetKey{v}, Skipped: []Skip{{s, "no sglang"}}}, ""},
		"silently missing":             {Export{Exported: []PresetKey{v}}, "neither exported nor skipped"},
		"undocumented reason":          {Export{Exported: []PresetKey{v}, Skipped: []Skip{{s, "lazy"}}}, "undocumented"},
		"exported and skipped":         {Export{Exported: []PresetKey{v, s}, Skipped: []Skip{{s, "no sglang"}}}, "exported and skipped"},
		"exported twice":               {Export{Exported: []PresetKey{v, v, s}}, "exported and exported"},
		"unknown preset":               {Export{Exported: []PresetKey{v, s, {"m", "tgi"}}}, "not a preset"},
	} {
		err := Coverage(a, snapshot(), "f", c.e)
		if c.wantErr == "" && err != nil || c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)) {
			t.Errorf("%s: err = %v, want %q", name, err, c.wantErr)
		}
	}
}

func TestReferenceFit(t *testing.T) {
	prof := "1g.10gb"
	fit := func(family string, gpus, ctx int, mode catalog.FitMode) catalog.ModelFit {
		return catalog.ModelFit{ModelSlug: "m", Quantization: "fp8", KVCacheDType: "auto", DeviceFamily: family, Mode: mode, GPUCount: gpus, MaxContext: ctx}
	}
	snap := catalog.CatalogSnapshot{
		Families: []catalog.Family{{Name: "A100", Vendor: "nvidia"}, {Name: "L4", Vendor: "nvidia"}, {Name: "A10G", Vendor: "nvidia"}, {Name: "MI300X", Vendor: "amd"}},
		Fits: []catalog.ModelFit{
			fit("A100", 2, 8192, catalog.FitEmulated),
			fit("A10G", 1, 4096, catalog.FitEmulated),
			fit("L4", 4, 2048, catalog.FitNative),
			fit("MI300X", 1, 32768, catalog.FitNative),
			{ModelSlug: "m", Quantization: "fp8", KVCacheDType: "auto", DeviceFamily: "A100", Profile: &prof, Mode: catalog.FitEmulated, GPUCount: 1, MaxContext: 99999},
			{ModelSlug: "m", Quantization: "fp8", KVCacheDType: "fp8", DeviceFamily: "A10G", Mode: catalog.FitEmulated, GPUCount: 1, MaxContext: 8192},
			{ModelSlug: "m", Quantization: "bf16", KVCacheDType: "auto", DeviceFamily: "H100", Mode: catalog.FitNative, GPUCount: 1, MaxContext: 8192},
		},
	}
	ref := func(kv string) Ref {
		return Ref{Model: catalog.ModelTemplate{Slug: "m"}, Preset: catalog.Preset{Quantization: "fp8", KVCacheDType: kv}}
	}
	nvidia, both := []string{"nvidia"}, []string{"amd", "nvidia"}
	for name, c := range map[string]struct {
		r       Ref
		vendors []string
		opts    Options
		family  string
	}{
		"reference family, whole GPUs, even emulated": {ref(""), nvidia, Options{ReferenceFamily: "A100"}, "A100"},
		"native before fewer GPUs":                    {ref(""), nvidia, Options{}, "L4"},
		"other vendors excluded":                      {ref("auto"), both, Options{}, "MI300X"},
		"reference family of another vendor ignored":  {ref(""), nvidia, Options{ReferenceFamily: "MI300X"}, "L4"},
		"the preset's KV cache dtype":                 {ref("fp8"), nvidia, Options{}, "A10G"},
	} {
		f, ok := ReferenceFit(snap, c.r, c.vendors, c.opts)
		if !ok || f.DeviceFamily != c.family || f.Profile != nil || f.KVCacheDType != cmp.Or(c.r.Preset.KVCacheDType, "auto") {
			t.Errorf("%s: fit = %+v %v, want %s", name, f, ok, c.family)
		}
	}
	if f, ok := ReferenceFit(snap, ref(""), []string{"cpu"}, Options{}); ok {
		t.Errorf("a vendor without fits returned %+v", f)
	}
}

func TestVendors(t *testing.T) {
	bv := catalog.BackendVersion{Images: map[string]string{"nvidia": "n", "cpu": "c", "amd": "a"}}
	if got := Vendors(catalog.Preset{}, bv); !slices.Equal(got, []string{"amd", "cpu", "nvidia"}) {
		t.Errorf("vendors = %v, want every image's vendor, sorted", got)
	}
	if got := Vendors(catalog.Preset{Vendors: []string{"nvidia", "intel"}}, bv); !slices.Equal(got, []string{"nvidia"}) {
		t.Errorf("vendors = %v, want the preset's vendors that have an image", got)
	}
}

func TestRegistry(t *testing.T) {
	Register(fake{})
	if a, ok := Get("fake"); !ok || a.Name() != "fake" {
		t.Fatal("registered adapter not found")
	}
	defer func() {
		if recover() == nil {
			t.Error("duplicate registration did not panic")
		}
	}()
	Register(fake{})
}

type fakeExt struct{ ns, adapter string }

func (x fakeExt) Namespace() string                                  { return x.ns }
func (x fakeExt) Adapter() string                                    { return x.adapter }
func (fakeExt) Apply(string, catalog.CatalogSnapshot, *Export) error { return nil }

func TestExportExtensionRegistry(t *testing.T) {
	b, a := fakeExt{"example.com/b", "gpustack"}, fakeExt{"example.com/a", "gpustack"}
	RegisterExportExtension(b)
	RegisterExportExtension(a)
	if got := ExportExtensions(); !slices.Equal(got, []ExportExtension{a, b}) {
		t.Fatalf("export extensions = %v, want both, by namespace", got)
	}
	defer func() {
		if recover() == nil {
			t.Error("a second extension of one namespace for one adapter did not panic")
		}
	}()
	RegisterExportExtension(a)
}
