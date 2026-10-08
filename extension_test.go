package catalog

import (
	"cmp"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

type testExt struct {
	name  string
	write string
}

func (e testExt) Name() string { return e.name }

func (e testExt) Validate(s CatalogSnapshot) error {
	for _, m := range s.Models {
		if p, ok := m.Extensions[e.name]; ok && string(p) != `{"ok":true}` {
			return fmt.Errorf("bad payload %s", p)
		}
	}
	return nil
}

func (e testExt) Generate(s *CatalogSnapshot) error {
	for i := range s.Models {
		if s.Models[i].Extensions == nil {
			s.Models[i].Extensions = Extensions{}
		}
		s.Models[i].Extensions[cmp.Or(e.write, e.name)] = json.RawMessage(`{"ok":true}`)
	}
	return nil
}

var okPayload = json.RawMessage(`{"ok":true}`)

// extSnap has one object of every kind an overlay can address.
func extSnap() CatalogSnapshot {
	m := llama8b()
	m.Slug = "m"
	m.Presets = []Preset{{Name: "p", Quantization: "bf16", Backend: "vllm", BackendVersion: "v1"}}
	return CatalogSnapshot{
		SchemaVersion: SchemaVersion,
		Version:       "2026-10-08T00:00:00Z",
		Families:      []Family{{Name: "L4", Vendor: "nvidia", MemoryGB: 24}},
		Models:        []ModelTemplate{m},
		Fits:          []ModelFit{{ModelSlug: "m", DeviceFamily: "L4", Quantization: "bf16", KVCacheDType: "auto", GPUCount: 1}},
		Backends:      []Backend{{Name: "vllm", Versions: []BackendVersion{{Version: "v1", Quantizations: map[string]QuantSupport{"float": {NativeDType: "bf16"}}}}}},
	}
}

func TestExtensionPayloadRoundTripsIntoDigest(t *testing.T) {
	snap := extSnap()
	before := snap.Digest()
	snap.Models[0].Extensions = Extensions{"x.io/a": okPayload}
	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	var got CatalogSnapshot
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Models[0].Extensions, snap.Models[0].Extensions) {
		t.Fatalf("extensions after round trip = %s, want %s", got.Models[0].Extensions, snap.Models[0].Extensions)
	}
	if got.Digest() != snap.Digest() {
		t.Fatal("digest changed across a JSON round trip")
	}
	if snap.Digest() == before {
		t.Fatal("digest does not cover extension payloads")
	}
}

func TestRunExtensionsRefusesForeignNamespace(t *testing.T) {
	reg := newRegistry()
	reg.register(testExt{name: "x.io/a", write: "x.io/b"})
	snap := extSnap()
	if err := runExtensions(reg, &snap); err == nil || !strings.Contains(err.Error(), "x.io/b") {
		t.Fatalf("err = %v, want refusal naming x.io/b", err)
	}

	reg = newRegistry()
	reg.register(testExt{name: "x.io/a"})
	snap = extSnap()
	if err := runExtensions(reg, &snap); err != nil {
		t.Fatal(err)
	}
	if p := snap.Models[0].Extensions["x.io/a"]; string(p) != `{"ok":true}` {
		t.Fatalf("payload = %s, want the extension's own write", p)
	}
}

type funcExt struct {
	name string
	gen  func(*CatalogSnapshot)
}

func (e funcExt) Name() string                      { return e.name }
func (e funcExt) Validate(CatalogSnapshot) error    { return nil }
func (e funcExt) Generate(s *CatalogSnapshot) error { e.gen(s); return nil }

func TestRunExtensionsRefusesPublicChanges(t *testing.T) {
	for name, gen := range map[string]func(*CatalogSnapshot){
		"public field": func(s *CatalogSnapshot) { s.Models[0].Name = "renamed" },
		"added object": func(s *CatalogSnapshot) {
			s.Fits = append(s.Fits, ModelFit{ModelSlug: "m", DeviceFamily: "H100", KVCacheDType: "auto"})
		},
		"removed object": func(s *CatalogSnapshot) { s.Families = nil },
	} {
		reg := newRegistry()
		reg.register(funcExt{"x.io/a", gen})
		snap := extSnap()
		if err := runExtensions(reg, &snap); err == nil || !strings.Contains(err.Error(), "extension x.io/a changed the public snapshot") {
			t.Errorf("%s: err = %v, want refusal", name, err)
		}
	}
}

func TestRegisterExtensionRefusesDuplicateName(t *testing.T) {
	reg := newRegistry()
	reg.register(testExt{name: "x.io/a"})
	defer func() {
		if recover() == nil {
			t.Fatal("registering x.io/a twice did not panic")
		}
	}()
	reg.register(testExt{name: "x.io/a"})
}

func TestValidateRunsRegisteredExtensions(t *testing.T) {
	reg := newRegistry()
	reg.register(testExt{name: "x.io/a"})
	snap := extSnap()
	snap.Models[0].Extensions = Extensions{"x.io/a": json.RawMessage(`{"ok":false}`)}
	if err := snap.validate(reg); err == nil || !strings.Contains(err.Error(), "bad payload") {
		t.Fatalf("err = %v, want the extension's validation error", err)
	}
}

func TestValidateIgnoresUnknownNamespace(t *testing.T) {
	snap := extSnap()
	snap.Models[0].Extensions = Extensions{"vendor.example/x": json.RawMessage(`{"anything":1}`)}
	if err := snap.Validate(); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	var got CatalogSnapshot
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if p := got.Models[0].Extensions["vendor.example/x"]; string(p) != `{"anything":1}` {
		t.Fatalf("unknown payload after round trip = %s", p)
	}
}

func TestMergeAddsPayloads(t *testing.T) {
	public := extSnap()
	o := Overlay{
		SchemaVersion:  SchemaVersion,
		SnapshotDigest: public.Digest(),
		Families:       map[string]Extensions{"L4": {"x.io/a": okPayload}},
		Models:         map[string]Extensions{"m": {"x.io/a": okPayload}},
		Presets:        map[string]Extensions{"m/p": {"x.io/a": okPayload}},
		Fits:           map[string]Extensions{"m/L4/-/bf16/auto": {"x.io/a": okPayload}},
		Backends:       map[string]Extensions{"vllm/v1": {"x.io/a": okPayload}},
	}
	merged, err := Merge(public, o)
	if err != nil {
		t.Fatal(err)
	}
	for name, ext := range map[string]Extensions{
		"family":          merged.Families[0].Extensions,
		"model":           merged.Models[0].Extensions,
		"preset":          merged.Models[0].Presets[0].Extensions,
		"fit":             merged.Fits[0].Extensions,
		"backend version": merged.Backends[0].Versions[0].Extensions,
	} {
		if string(ext["x.io/a"]) != `{"ok":true}` {
			t.Errorf("merged %s extensions = %s, want the overlay payload", name, ext)
		}
	}
	if public.Models[0].Extensions != nil || public.Models[0].Presets[0].Extensions != nil {
		t.Fatal("Merge mutated the public snapshot")
	}
	merged.Families[0].Extensions = nil
	merged.Models[0].Extensions = nil
	merged.Models[0].Presets[0].Extensions = nil
	merged.Fits[0].Extensions = nil
	merged.Backends[0].Versions[0].Extensions = nil
	if merged.Digest() != public.Digest() {
		t.Fatal("Merge changed a public field")
	}
}

func TestMergeRefuses(t *testing.T) {
	cases := []struct {
		name    string
		public  func(*CatalogSnapshot)
		overlay func(*Overlay)
		want    string
	}{
		{"wrong digest", nil, func(o *Overlay) { o.SnapshotDigest = "0000" }, "digest"},
		{"unknown model", nil, func(o *Overlay) { o.Models = map[string]Extensions{"nope": {"x.io/a": okPayload}} }, "unknown"},
		{"unknown preset", nil, func(o *Overlay) { o.Presets = map[string]Extensions{"m/nope": {"x.io/a": okPayload}} }, "unknown"},
		{"unknown fit", nil, func(o *Overlay) { o.Fits = map[string]Extensions{"m/L4/-/fp8/auto": {"x.io/a": okPayload}} }, "unknown"},
		{"schema 3", nil, func(o *Overlay) { o.SchemaVersion = "3.0.0" }, "schema"},
		{"namespace already public", func(s *CatalogSnapshot) {
			s.Models[0].Extensions = Extensions{"x.io/a": okPayload}
		}, func(o *Overlay) { o.Models = map[string]Extensions{"m": {"x.io/a": okPayload}} }, "already"},
	}
	for _, c := range cases {
		public := extSnap()
		if c.public != nil {
			c.public(&public)
		}
		o := Overlay{SchemaVersion: SchemaVersion, SnapshotDigest: public.Digest()}
		c.overlay(&o)
		if _, err := Merge(public, o); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
}

// The embedded file is walked rather than SnapshotV4's decoded value: decoding
// would drop an "extensions" key on an object whose type has no such field
// (node types).
func TestPublicSnapshotHasNoExtensions(t *testing.T) {
	var v any
	if err := json.Unmarshal(snapshotJSON, &v); err != nil {
		t.Fatal(err)
	}
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch v := v.(type) {
		case map[string]any:
			for k, e := range v {
				if k == "extensions" {
					t.Errorf("public snapshot has extensions at %s", path)
				}
				walk(path+"."+k, e)
			}
		case []any:
			for i, e := range v {
				walk(fmt.Sprintf("%s[%d]", path, i), e)
			}
		}
	}
	walk("$", v)
}
