package catalog

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// Extension adds a feature the core does not know. Name is its namespace, the only
// Extensions key it may write.
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

// RegisterExtension registers e; it panics on a duplicate name.
func RegisterExtension(e Extension) { extensions.register(e) }

// RunExtensions runs every registered extension's Generate then Validate, failing
// if one writes outside its namespace or changes the public snapshot.
func RunExtensions(snap *CatalogSnapshot) error { return runExtensions(extensions, snap) }

func runExtensions(reg *registry, snap *CatalogSnapshot) error {
	names := slices.Sorted(maps.Keys(reg.exts))
	for _, name := range names {
		before := payloads(snap)
		public, err := publicDigest(snap)
		if err != nil {
			return err
		}
		if err := reg.exts[name].Generate(snap); err != nil {
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
		switch d, err := publicDigest(snap); {
		case err != nil:
			return fmt.Errorf("extension %s: %w", name, err)
		case d != public:
			return fmt.Errorf("extension %s changed the public snapshot", name)
		}
	}
	for _, name := range names {
		if err := reg.exts[name].Validate(*snap); err != nil {
			return fmt.Errorf("extension %s: %w", name, err)
		}
	}
	return nil
}

func publicDigest(snap *CatalogSnapshot) (string, error) {
	b, err := json.Marshal(snap)
	if err != nil {
		return "", err
	}
	var c CatalogSnapshot
	if err := json.Unmarshal(b, &c); err != nil {
		return "", err
	}
	for _, ext := range extensionTargets(&c) {
		*ext = nil
	}
	return c.Digest(), nil
}

type payloadKey struct{ object, namespace string }

func payloads(snap *CatalogSnapshot) map[payloadKey]string {
	out := map[payloadKey]string{}
	for object, ext := range extensionTargets(snap) {
		for ns, p := range *ext {
			out[payloadKey{object, ns}] = string(p)
		}
	}
	return out
}

// extensionTargets indexes every overlay-addressable Extensions by "<kind> <id>".
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

// Overlay holds extension payloads keyed by object id, pinned to a public snapshot digest.
type Overlay struct {
	SchemaVersion  string                `json:"schema_version"`
	SnapshotDigest string                `json:"snapshot_digest"`
	Families       map[string]Extensions `json:"families,omitempty"`
	Models         map[string]Extensions `json:"models,omitempty"`
	Presets        map[string]Extensions `json:"presets,omitempty"`
	Fits           map[string]Extensions `json:"fits,omitempty"`
	Backends       map[string]Extensions `json:"backends,omitempty"`
}

// FitKey identifies a fit: "<slug>/<family>/<profile|->/<quant>/<kv dtype>",
// plus "/<n>-node" when n > 1.
func FitKey(f ModelFit) string {
	profile := "-"
	if f.Profile != nil {
		profile = *f.Profile
	}
	key := strings.Join([]string{f.ModelSlug, f.DeviceFamily, profile, f.Quantization, f.KVCacheDType}, "/")
	if f.Nodes > 1 {
		key += "/" + strconv.Itoa(f.Nodes) + "-node"
	}
	return key
}

// Merge adds the overlay's payloads to a copy of public. It refuses a different
// schema major or digest, unknown ids, and namespaces an object already holds.
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
