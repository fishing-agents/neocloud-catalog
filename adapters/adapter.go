// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

// Package adapters exports the neocloud catalog to model-serving platforms, one
// registered adapter per platform.
package adapters

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"

	catalog "github.com/fishman/neocloud-catalog"
)

// DefaultReferenceFamily sizes presets on platforms without a fit matrix.
const DefaultReferenceFamily = "H100"

// Capabilities is what a platform can express.
type Capabilities struct {
	Backends  []string
	Sources   []string
	FitMatrix bool // a GPU count and context per device family
}

// Artifact is one exported file, relative to the export root.
type Artifact struct {
	Path string
	Body []byte
}

// PresetKey names a catalog preset.
type PresetKey struct {
	Model, Preset string
}

// Skip is a preset an export cannot express, and why.
type Skip struct {
	PresetKey
	Reason string
}

// Export is one format's output; every preset is either exported or skipped.
type Export struct {
	Artifacts []Artifact
	Exported  []PresetKey
	Skipped   []Skip
}

// Options tune an export.
type Options struct {
	// ReferenceFamily defaults to DefaultReferenceFamily.
	ReferenceFamily string
}

// Adapter exports the catalog to one platform.
type Adapter interface {
	Name() string
	Capabilities() Capabilities
	Formats() []string
	// SkipReasons are the documented reasons; any other fails Coverage.
	SkipReasons(format string) []string
	Export(snap catalog.CatalogSnapshot, format string, opts Options) (Export, error)
}

var (
	mu         sync.RWMutex
	registry   = map[string]Adapter{}
	extensions []ExportExtension
)

// Register makes an adapter available by name; it panics on a duplicate name.
func Register(a Adapter) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := registry[a.Name()]; dup {
		panic("adapters: duplicate adapter " + a.Name())
	}
	registry[a.Name()] = a
}

// Get returns the adapter registered under name.
func Get(name string) (Adapter, bool) {
	mu.RLock()
	defer mu.RUnlock()
	a, ok := registry[name]
	return a, ok
}

// Names lists the registered adapters, sorted.
func Names() []string {
	mu.RLock()
	defer mu.RUnlock()
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// ExportExtension renders one catalog extension's payloads into a platform format
// after the adapter's own rendering. Public builds register none.
type ExportExtension interface {
	Namespace() string
	Adapter() string
	Apply(format string, snap catalog.CatalogSnapshot, e *Export) error
}

// RegisterExportExtension panics on a second extension of one namespace for one adapter.
func RegisterExportExtension(x ExportExtension) {
	mu.Lock()
	defer mu.Unlock()
	if slices.ContainsFunc(extensions, func(y ExportExtension) bool {
		return y.Namespace() == x.Namespace() && y.Adapter() == x.Adapter()
	}) {
		panic("adapters: duplicate export extension " + x.Namespace() + " for " + x.Adapter())
	}
	extensions = append(extensions, x)
	slices.SortFunc(extensions, func(a, b ExportExtension) int {
		return cmp.Or(cmp.Compare(a.Adapter(), b.Adapter()), cmp.Compare(a.Namespace(), b.Namespace()))
	})
}

// ExportExtensions lists the registered export extensions by adapter, then namespace.
func ExportExtensions() []ExportExtension {
	mu.RLock()
	defer mu.RUnlock()
	return slices.Clone(extensions)
}

// Coverage checks that every preset is exported or skipped exactly once, and every
// skip has a documented reason.
func Coverage(a Adapter, snap catalog.CatalogSnapshot, format string, e Export) error {
	seen := map[PresetKey]string{}
	var errs []error
	mark := func(k PresetKey, how string) {
		if prev, dup := seen[k]; dup {
			errs = append(errs, fmt.Errorf("%s/%s: %s and %s", k.Model, k.Preset, prev, how))
		}
		seen[k] = how
	}
	for _, k := range e.Exported {
		mark(k, "exported")
	}
	reasons := a.SkipReasons(format)
	for _, s := range e.Skipped {
		mark(s.PresetKey, "skipped")
		if !slices.Contains(reasons, s.Reason) {
			errs = append(errs, fmt.Errorf("%s/%s: undocumented skip reason %q", s.Model, s.Preset, s.Reason))
		}
	}
	for _, r := range Presets(snap) {
		if _, ok := seen[r.Key()]; !ok {
			errs = append(errs, fmt.Errorf("%s/%s: neither exported nor skipped", r.Model.Slug, r.Preset.Name))
		}
		delete(seen, r.Key())
	}
	for k := range seen {
		errs = append(errs, fmt.Errorf("%s/%s: not a preset of the snapshot", k.Model, k.Preset))
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s %s coverage: %w", a.Name(), format, errors.Join(errs...))
	}
	return nil
}

// Ref is a preset with its model.
type Ref struct {
	Model  catalog.ModelTemplate
	Preset catalog.Preset
}

// Presets lists every preset in catalog order.
func Presets(snap catalog.CatalogSnapshot) []Ref {
	var refs []Ref
	for _, m := range snap.Models {
		for _, p := range m.Presets {
			refs = append(refs, Ref{Model: m, Preset: p})
		}
	}
	return refs
}

// Key names the preset.
func (r Ref) Key() PresetKey {
	return PresetKey{Model: r.Model.Slug, Preset: r.Preset.Name}
}

// Quantization returns the preset's checkpoint.
func (r Ref) Quantization() (catalog.Quantization, bool) {
	i := slices.IndexFunc(r.Model.Quantizations, func(q catalog.Quantization) bool { return q.Name == r.Preset.Quantization })
	if i < 0 {
		return catalog.Quantization{}, false
	}
	return r.Model.Quantizations[i], true
}

// Source returns the first checkpoint source, in preference order, of one of types.
func (r Ref) Source(types ...string) (catalog.ModelSource, bool) {
	q, ok := r.Quantization()
	if !ok {
		return catalog.ModelSource{}, false
	}
	for _, s := range q.Sources {
		if slices.Contains(types, s.Type) {
			return s, true
		}
	}
	return catalog.ModelSource{}, false
}

// BackendVersion returns the named backend release.
func BackendVersion(snap catalog.CatalogSnapshot, backend, version string) (catalog.BackendVersion, bool) {
	for _, b := range snap.Backends {
		if b.Name != backend {
			continue
		}
		for _, v := range b.Versions {
			if v.Version == version {
				return v, true
			}
		}
	}
	return catalog.BackendVersion{}, false
}

// Vendors lists the vendors with an image for the preset, limited to its Vendors, sorted.
func Vendors(p catalog.Preset, bv catalog.BackendVersion) []string {
	return slices.DeleteFunc(slices.Sorted(maps.Keys(bv.Images)), func(v string) bool {
		return len(p.Vendors) > 0 && !slices.Contains(p.Vendors, v)
	})
}

// Draft returns the preset's draft model, or nil.
func Draft(snap catalog.CatalogSnapshot, p catalog.Preset) *catalog.DraftModel {
	if p.Speculative == nil {
		return nil
	}
	i := slices.IndexFunc(snap.DraftModels, func(d catalog.DraftModel) bool { return d.Slug == p.Speculative.DraftModel })
	if i < 0 {
		return nil
	}
	return &snap.DraftModels[i]
}

// ReferenceFit returns the preset's single-node whole-GPU fit on the reference
// family, else the best on vendors: native first, fewest GPUs, largest context,
// then family name.
func ReferenceFit(snap catalog.CatalogSnapshot, r Ref, vendors []string, opts Options) (catalog.ModelFit, bool) {
	family := cmp.Or(opts.ReferenceFamily, DefaultReferenceFamily)
	vendor := map[string]string{}
	for _, f := range snap.Families {
		vendor[f.Name] = f.Vendor
	}
	kv := cmp.Or(r.Preset.KVCacheDType, "auto")
	var fits []catalog.ModelFit
	for _, f := range snap.Fits {
		if f.ModelSlug == r.Model.Slug && f.Quantization == r.Preset.Quantization && f.KVCacheDType == kv && f.Profile == nil && f.Nodes <= 1 &&
			slices.Contains(vendors, vendor[f.DeviceFamily]) {
			if f.DeviceFamily == family {
				return f, true
			}
			fits = append(fits, f)
		}
	}
	if len(fits) == 0 {
		return catalog.ModelFit{}, false
	}
	emulated := func(f catalog.ModelFit) int {
		if f.Mode == catalog.FitNative {
			return 0
		}
		return 1
	}
	return slices.MinFunc(fits, func(a, b catalog.ModelFit) int {
		return cmp.Or(cmp.Compare(emulated(a), emulated(b)), cmp.Compare(a.GPUCount, b.GPUCount),
			cmp.Compare(b.MaxContext, a.MaxContext), cmp.Compare(a.DeviceFamily, b.DeviceFamily))
	}), true
}
