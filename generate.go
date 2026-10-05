package catalog

import (
	"errors"
	"fmt"
	"math"
	"sort"
)

// GPUMemoryUtilization is the share of each GPU's memory a model may use for
// weights and KV cache; vLLM's default, leaving the rest for activations.
const GPUMemoryUtilization = 0.9

// tpSizes are the tensor-parallel GPU counts considered, smallest first.
var tpSizes = []int{1, 2, 4, 8}

// FamilyCapability is the curated per-family GPU memory used to derive fits.
type FamilyCapability struct {
	Name     string  `json:"name"`
	MemoryGB float64 `json:"memory_gb"`
}

// Generate validates the curated input and derives the fit matrix. For each
// model, quantization, and family it emits the smallest legal GPU count whose
// memory budget holds the weights plus the model's default context; max context
// is what the remaining budget holds, capped at the model's ceiling. Pure and
// deterministic, so the snapshot digest is stable for identical input.
func Generate(templates []ModelTemplate, families []FamilyCapability, backends []Backend) (CatalogSnapshot, error) {
	snap := CatalogSnapshot{SchemaVersion: SchemaVersion, Models: make([]ModelTemplate, len(templates)), Fits: []ModelFit{}, Backends: backends}
	for i, t := range templates {
		t.KVBytesPerToken = t.Architecture.KVBytesPerToken()
		snap.Models[i] = t
		if t.KVBytesPerToken <= 0 {
			continue
		}
		for _, q := range t.Quantizations {
			for _, fam := range families {
				if f, ok := fit(t, q, fam); ok {
					snap.Fits = append(snap.Fits, f)
				}
			}
		}
	}
	sort.Slice(snap.Fits, func(i, j int) bool {
		a, b := snap.Fits[i], snap.Fits[j]
		if a.ModelSlug != b.ModelSlug {
			return a.ModelSlug < b.ModelSlug
		}
		if a.DeviceFamily != b.DeviceFamily {
			return a.DeviceFamily < b.DeviceFamily
		}
		return a.Quantization < b.Quantization
	})
	if err := snap.Validate(); err != nil {
		return CatalogSnapshot{}, err
	}
	return snap, nil
}

// Validate checks that every model source has a known type and is pinned, and
// that every preset names a quantization of its model and an existing backend
// version. Embedders can run it on a snapshot fetched from an upstream.
func (s CatalogSnapshot) Validate() error {
	versions := map[[2]string]bool{}
	for _, b := range s.Backends {
		for _, v := range b.Versions {
			versions[[2]string{b.Name, v.Version}] = true
		}
	}
	var errs []error
	for _, m := range s.Models {
		quants := map[string]bool{}
		for _, q := range m.Quantizations {
			quants[q.Name] = true
			for _, src := range q.Sources {
				if err := src.validate(); err != nil {
					errs = append(errs, fmt.Errorf("%s %s: %w", m.Slug, q.Name, err))
				}
			}
		}
		presets := map[string]bool{}
		for _, p := range m.Presets {
			switch {
			case presets[p.Name]:
				errs = append(errs, fmt.Errorf("%s: duplicate preset %q", m.Slug, p.Name))
			case !quants[p.Quantization]:
				errs = append(errs, fmt.Errorf("%s: preset %q: unknown quantization %q", m.Slug, p.Name, p.Quantization))
			case !versions[[2]string{p.Backend, p.BackendVersion}]:
				errs = append(errs, fmt.Errorf("%s: preset %q: unknown backend version %s %s", m.Slug, p.Name, p.Backend, p.BackendVersion))
			}
			presets[p.Name] = true
		}
	}
	return errors.Join(errs...)
}

func (src ModelSource) validate() error {
	switch {
	case src.Type != SourceHuggingFace && src.Type != SourceModelScope && src.Type != SourceOCI && src.Type != SourceLocal:
		return fmt.Errorf("source %q: unknown type %q", src.Repo, src.Type)
	case src.Repo == "":
		return fmt.Errorf("%s source: repo required", src.Type)
	case src.Revision == "" && src.Type != SourceLocal:
		return fmt.Errorf("%s source %q: revision required", src.Type, src.Repo)
	}
	return nil
}

func fit(t ModelTemplate, q Quantization, fam FamilyCapability) (ModelFit, bool) {
	for _, gpus := range tpDegrees(t.Architecture.AttentionHeads) {
		budget := fam.MemoryGB * GPUMemoryUtilization * float64(gpus)
		ctx := min(int((budget-q.WeightsGB)*1e9/t.KVBytesPerToken), t.ContextMax)
		if ctx < min(t.DefaultContext, t.ContextMax) {
			continue
		}
		return ModelFit{
			ModelSlug:      t.Slug,
			DeviceFamily:   fam.Name,
			Quantization:   q.Name,
			GPUCount:       gpus,
			MaxContext:     ctx,
			VRAMRequiredGB: math.Ceil((q.WeightsGB+t.KVBytesPerToken*float64(ctx)/1e9)*100) / 100,
		}, true
	}
	return ModelFit{}, false
}

// tpDegrees are the GPU counts tensor parallelism allows: those dividing the
// attention head count.
func tpDegrees(heads int) []int {
	var out []int
	for _, n := range tpSizes {
		if heads > 0 && heads%n == 0 {
			out = append(out, n)
		}
	}
	return out
}
