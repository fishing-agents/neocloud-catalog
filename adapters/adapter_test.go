package adapters

import (
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
	snap := catalog.CatalogSnapshot{Fits: []catalog.ModelFit{
		{ModelSlug: "m", Quantization: "bf16", DeviceFamily: "A100", GPUCount: 2, MaxContext: 8192},
		{ModelSlug: "m", Quantization: "bf16", DeviceFamily: "MI300X", GPUCount: 1, MaxContext: 32768},
		{ModelSlug: "m", Quantization: "bf16", DeviceFamily: "L4", GPUCount: 1, MaxContext: 4096},
		{ModelSlug: "m", Quantization: "bf16", DeviceFamily: "A100", Profile: &prof, GPUCount: 1, MaxContext: 99999},
		{ModelSlug: "m", Quantization: "fp8", DeviceFamily: "H100", GPUCount: 1, MaxContext: 8192},
	}}
	if f, ok := ReferenceFit(snap, "m", "bf16", Options{ReferenceFamily: "A100"}); !ok || f.GPUCount != 2 || f.Profile != nil {
		t.Errorf("A100 reference = %+v %v, want the whole-GPU A100 fit", f, ok)
	}
	if f, ok := ReferenceFit(snap, "m", "bf16", Options{}); !ok || f.DeviceFamily != "MI300X" {
		t.Errorf("no H100 fit = %+v %v, want the fewest GPUs with the largest context (MI300X)", f, ok)
	}
	if f, ok := ReferenceFit(snap, "m", "fp8", Options{}); !ok || f.DeviceFamily != "H100" {
		t.Errorf("default reference = %+v %v, want H100", f, ok)
	}
	if _, ok := ReferenceFit(snap, "m", "int4", Options{}); ok {
		t.Error("a quantization without fits returned one")
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
