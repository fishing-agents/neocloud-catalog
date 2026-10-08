package catalog

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Extension adds a feature to the catalog without the core knowing it. Name is
// its namespace (e.g. neocloud.io/interruption), the only Extensions key it may
// write. Generate derives payloads during generation; Validate checks them in a
// generated or merged snapshot.
type Extension interface {
	Name() string
	Validate(snap CatalogSnapshot) error
	Generate(snap *CatalogSnapshot) error
}

type registry struct {
	exts map[string]Extension
}

func newRegistry() *registry { return &registry{exts: map[string]Extension{}} }

func (r *registry) register(e Extension) {
	if _, dup := r.exts[e.Name()]; dup {
		panic("catalog: extension " + e.Name() + " registered twice")
	}
	r.exts[e.Name()] = e
}

var extensions = newRegistry()

// RegisterExtension adds e to the extensions Validate and RunExtensions run.
// It panics if an extension of the same name is already registered.
func RegisterExtension(e Extension) { extensions.register(e) }

// RunExtensions runs Generate then Validate of every registered extension on a
// generated snapshot, failing if an extension writes outside its namespace.
func RunExtensions(snap *CatalogSnapshot) error { return runExtensions(extensions, snap) }

func runExtensions(reg *registry, snap *CatalogSnapshot) error {
	for _, name := range slices.Sorted(maps.Keys(reg.exts)) {
		e := reg.exts[name]
		before := payloads(snap)
		if err := e.Generate(snap); err != nil {
			return fmt.Errorf("extension %s: generate: %w", name, err)
		}
		after := payloads(snap)
		for k, p := range after {
			if old, ok := before[k]; k.namespace != name && (!ok || old != p) {
				return fmt.Errorf("extension %s wrote namespace %s on %s", name, k.namespace, k.object)
			}
		}
		for k := range before {
			if _, ok := after[k]; !ok && k.namespace != name {
				return fmt.Errorf("extension %s removed namespace %s on %s", name, k.namespace, k.object)
			}
		}
		if err := e.Validate(*snap); err != nil {
			return fmt.Errorf("extension %s: %w", name, err)
		}
	}
	return nil
}

type payloadKey struct{ object, namespace string }

// payloads copies every extension payload of the snapshot.
func payloads(snap *CatalogSnapshot) map[payloadKey]string {
	out := map[payloadKey]string{}
	for object, ext := range extensionTargets(snap) {
		for ns, p := range *ext {
			out[payloadKey{object, ns}] = string(p)
		}
	}
	return out
}

// extensionTargets indexes the Extensions of every object an overlay can
// address by "<kind> <id>", with the ids Overlay documents.
func extensionTargets(snap *CatalogSnapshot) map[string]*Extensions {
	out := map[string]*Extensions{}
	for i := range snap.Families {
		out["family "+snap.Families[i].Name] = &snap.Families[i].Extensions
	}
	for i := range snap.Models {
		m := &snap.Models[i]
		out["model "+m.Slug] = &m.Extensions
		for j := range m.Presets {
			out["preset "+m.Slug+"/"+m.Presets[j].Name] = &m.Presets[j].Extensions
		}
	}
	for i := range snap.Fits {
		out["fit "+FitKey(snap.Fits[i])] = &snap.Fits[i].Extensions
	}
	for i := range snap.Backends {
		b := &snap.Backends[i]
		for j := range b.Versions {
			out["backend "+b.Name+"/"+b.Versions[j].Version] = &b.Versions[j].Extensions
		}
	}
	return out
}

// Overlay holds extension payloads for a public snapshot, keyed by the ids of
// its objects, and pinned to that snapshot's digest.
type Overlay struct {
	SchemaVersion  string                `json:"schema_version"`
	SnapshotDigest string                `json:"snapshot_digest"`
	Families       map[string]Extensions `json:"families,omitempty"`
	Models         map[string]Extensions `json:"models,omitempty"`
	Presets        map[string]Extensions `json:"presets,omitempty"`
	Fits           map[string]Extensions `json:"fits,omitempty"`
	Backends       map[string]Extensions `json:"backends,omitempty"`
}

// FitKey identifies a fit: "<slug>/<family>/<profile|->/<quant>/<kv dtype>".
func FitKey(f ModelFit) string {
	profile := "-"
	if f.Profile != nil {
		profile = *f.Profile
	}
	return strings.Join([]string{f.ModelSlug, f.DeviceFamily, profile, f.Quantization, f.KVCacheDType}, "/")
}

// Merge returns a copy of public with the overlay's payloads added. It refuses
// an overlay of another schema major or for another snapshot digest, an id the
// snapshot lacks, and a namespace an object already holds: it adds payloads
// only and never changes a public field.
func Merge(public CatalogSnapshot, o Overlay) (CatalogSnapshot, error) {
	if major, _, _ := strings.Cut(o.SchemaVersion, "."); major != schemaMajor() {
		return CatalogSnapshot{}, fmt.Errorf("overlay schema %q, want major %s", o.SchemaVersion, schemaMajor())
	}
	if d := public.Digest(); o.SnapshotDigest != d {
		return CatalogSnapshot{}, fmt.Errorf("overlay for snapshot digest %s, not %s", o.SnapshotDigest, d)
	}
	b, err := json.Marshal(public)
	if err != nil {
		return CatalogSnapshot{}, err
	}
	var merged CatalogSnapshot
	if err := json.Unmarshal(b, &merged); err != nil {
		return CatalogSnapshot{}, err
	}

	targets := extensionTargets(&merged)
	var errs []error
	for _, kind := range []struct {
		name     string
		payloads map[string]Extensions
	}{{"family", o.Families}, {"model", o.Models}, {"preset", o.Presets}, {"fit", o.Fits}, {"backend", o.Backends}} {
		for _, id := range slices.Sorted(maps.Keys(kind.payloads)) {
			ext, ok := targets[kind.name+" "+id]
			if !ok {
				errs = append(errs, fmt.Errorf("overlay: unknown %s %q", kind.name, id))
				continue
			}
			for _, ns := range slices.Sorted(maps.Keys(kind.payloads[id])) {
				if _, dup := (*ext)[ns]; dup {
					errs = append(errs, fmt.Errorf("overlay: %s %q already holds namespace %s", kind.name, id, ns))
					continue
				}
				if *ext == nil {
					*ext = Extensions{}
				}
				(*ext)[ns] = slices.Clone(kind.payloads[id][ns])
			}
		}
	}
	if err := errors.Join(errs...); err != nil {
		return CatalogSnapshot{}, err
	}
	return merged, nil
}

func schemaMajor() string {
	major, _, _ := strings.Cut(SchemaVersion, ".")
	return major
}
