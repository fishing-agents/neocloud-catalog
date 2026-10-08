package catalog

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// GPUMemoryUtilization is the share of each GPU's memory a model may use for
// weights and KV cache; vLLM's default, leaving the rest for activations.
const GPUMemoryUtilization = 0.9

// tpSizes are the tensor-parallel GPU counts considered, smallest first.
var tpSizes = []int{1, 2, 4, 8}

// Generate validates the curated input and assembles the snapshot: families,
// the node-type vocabulary of the cloud node types plus generated fallbacks,
// models, draft models, backends and the fit matrix. For each model,
// quantization, and family it emits the smallest legal GPU count whose memory
// budget holds the weights plus the model's default context; max context is
// what the remaining budget holds, capped at the model's ceiling. The report
// lists findings for the curator that do not fail generation. Pure and
// deterministic, so the snapshot digest is stable for identical input.
func Generate(families []Family, nodeTypes []NodeType, templates []ModelTemplate, drafts []DraftModel, backends []Backend) (CatalogSnapshot, []string, error) {
	vocab, err := NodeTypeVocabulary(families, nodeTypes)
	if err != nil {
		return CatalogSnapshot{}, nil, err
	}
	if families == nil {
		families = []Family{}
	}
	if drafts == nil {
		drafts = []DraftModel{}
	}
	snap := CatalogSnapshot{SchemaVersion: SchemaVersion, Families: families, NodeTypes: vocab, Models: make([]ModelTemplate, len(templates)),
		DraftModels: drafts, Fits: []ModelFit{}, Backends: backends}
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
		return CatalogSnapshot{}, nil, err
	}
	return snap, nil, nil
}

// Validate checks that every model source has a known type and is pinned, that
// every draft model is pinned on a repository and targets known models, and
// that every preset names a quantization of its model, an existing backend
// version, and a draft model for its backend, model and method, stays within
// the model's context and renders on its backend version. It then runs
// Validate of every registered extension. Payloads of namespaces no registered
// extension owns are preserved and ignored. Embedders can run it on a snapshot
// fetched from an upstream.
func (s CatalogSnapshot) Validate() error { return s.validate(extensions) }

func (s CatalogSnapshot) validate(reg *registry) error {
	versions := map[[2]string]BackendVersion{}
	for _, b := range s.Backends {
		for _, v := range b.Versions {
			versions[[2]string{b.Name, v.Version}] = v
		}
	}
	models := map[string]bool{}
	for _, m := range s.Models {
		models[m.Slug] = true
	}
	var errs []error
	drafts := map[string]*DraftModel{}
	for i, d := range s.DraftModels {
		if drafts[d.Slug] != nil {
			errs = append(errs, fmt.Errorf("draft model %q: duplicate", d.Slug))
		}
		drafts[d.Slug] = &s.DraftModels[i]
		// Engines take a draft as a repository id and revision; an OCI
		// reference or a node path would be passed as one.
		if d.Source.Type != SourceHuggingFace && d.Source.Type != SourceModelScope {
			errs = append(errs, fmt.Errorf("draft model %q: source type %q, want huggingface or modelscope", d.Slug, d.Source.Type))
		} else if err := d.Source.validate(); err != nil {
			errs = append(errs, fmt.Errorf("draft model %q: %w", d.Slug, err))
		}
		for _, target := range d.Targets {
			if !models[target] {
				errs = append(errs, fmt.Errorf("draft model %q: unknown target %q", d.Slug, target))
			}
		}
	}
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
			bv, known := versions[[2]string{p.Backend, p.BackendVersion}]
			switch {
			case presets[p.Name]:
				errs = append(errs, fmt.Errorf("%s: duplicate preset %q", m.Slug, p.Name))
			case !quants[p.Quantization]:
				errs = append(errs, fmt.Errorf("%s: preset %q: unknown quantization %q", m.Slug, p.Name, p.Quantization))
			case !known:
				errs = append(errs, fmt.Errorf("%s: preset %q: unknown backend version %s %s", m.Slug, p.Name, p.Backend, p.BackendVersion))
			default:
				if err := validatePreset(m, p, bv, drafts); err != nil {
					errs = append(errs, err)
				}
			}
			presets[p.Name] = true
		}
	}
	for _, name := range slices.Sorted(maps.Keys(reg.exts)) {
		if err := reg.exts[name].Validate(s); err != nil {
			errs = append(errs, fmt.Errorf("extension %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

// validatePreset checks what the renderer relies on but cannot see, then
// renders the preset on a representative fit so a field its flag table cannot
// express fails generation instead of a deployment.
func validatePreset(m ModelTemplate, p Preset, bv BackendVersion, drafts map[string]*DraftModel) error {
	id := fmt.Sprintf("%s/%s on %s %s", m.Slug, p.Name, p.Backend, p.BackendVersion)
	var errs []error
	if p.Context > m.ContextMax {
		errs = append(errs, fmt.Errorf("%s: context %d above context_max %d", id, p.Context, m.ContextMax))
	}
	// Each side's tensor parallel is its GPU count; a preset-wide one is ignored.
	if t := p.Topology; t.Kind == "disaggregated" && (t.Prefill < 1 || t.Decode < 1 || p.TensorParallel != 0) {
		errs = append(errs, fmt.Errorf("%s: disaggregated topology needs prefill_gpus and decode_gpus >= 1 and no tensor_parallel", id))
	}
	// The template's path exists only in vLLM's CUDA image.
	if p.Backend == "vllm" && p.ChatTemplate == "llama3.1-tools" && !slices.Equal(p.Vendors, []string{"nvidia"}) {
		errs = append(errs, fmt.Errorf("%s: chat template %q needs vendors [nvidia]", id, p.ChatTemplate))
	}
	if p.MinBackendVersion != "" {
		switch c, err := compareVersions(p.MinBackendVersion, p.BackendVersion); {
		case err != nil:
			errs = append(errs, fmt.Errorf("%s: min_backend_version: %w", id, err))
		case c > 0:
			errs = append(errs, fmt.Errorf("%s: min_backend_version %s is newer than the backend version", id, p.MinBackendVersion))
		}
	}
	var draft *DraftModel
	if s := p.Speculative; s != nil && s.DraftModel != "" {
		draft = drafts[s.DraftModel]
		switch {
		case draft == nil:
			errs = append(errs, fmt.Errorf("%s: unknown draft model %q", id, s.DraftModel))
		case !slices.Contains(draft.Backends, p.Backend):
			errs = append(errs, fmt.Errorf("%s: draft model %q does not load on %s", id, draft.Slug, p.Backend))
		case !slices.Contains(draft.Targets, m.Slug):
			errs = append(errs, fmt.Errorf("%s: draft model %q does not target %s", id, draft.Slug, m.Slug))
		case draft.Method != s.Method:
			errs = append(errs, fmt.Errorf("%s: draft model %q is %s, not %s", id, draft.Slug, draft.Method, s.Method))
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	roles := []string{""}
	if p.Topology.Kind == "disaggregated" {
		roles = []string{"prefill", "decode"}
	}
	fit := ModelFit{ModelSlug: m.Slug, Quantization: p.Quantization, GPUCount: cmp.Or(p.TensorParallel, 1)}
	for _, role := range roles {
		if _, err := RenderRole(m, p, bv, fit, draft, role, 0); err != nil {
			return err
		}
	}
	return nil
}

// compareVersions orders versions of dot-separated numbers, with an optional
// "v" prefix, numerically: v0.9.0 is older than v0.31.0, and v0.31 equals
// v0.31.0.
func compareVersions(a, b string) (int, error) {
	na, err := versionNumbers(a)
	if err != nil {
		return 0, err
	}
	nb, err := versionNumbers(b)
	if err != nil {
		return 0, err
	}
	for len(na) < len(nb) {
		na = append(na, 0)
	}
	for len(nb) < len(na) {
		nb = append(nb, 0)
	}
	return slices.Compare(na, nb), nil
}

func versionNumbers(v string) ([]int, error) {
	var out []int
	for f := range strings.SplitSeq(strings.TrimPrefix(v, "v"), ".") {
		n, err := strconv.Atoi(f)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("version %q is not dot-separated numbers", v)
		}
		out = append(out, n)
	}
	return out, nil
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

func fit(t ModelTemplate, q Quantization, fam Family) (ModelFit, bool) {
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
