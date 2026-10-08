package catalog

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
)

// GPUMemoryUtilization is the share of each GPU's memory a fit may use for
// weights and KV cache: a conservative budget below vLLM's 0.92 default,
// leaving the rest for activations.
const GPUMemoryUtilization = 0.9

// tpSizes are the tensor-parallel GPU counts per node considered, smallest
// first.
var tpSizes = []int{1, 2, 4, 8}

// kvDTypes are the KV cache dtypes fits are derived for.
var kvDTypes = []string{"auto", "fp8"}

// guidanceKinds are the quantization kinds whose fits drive deployment
// guidance; other kinds (gguf, int4) keep their per-fit estimates only.
var guidanceKinds = []string{"float", "fp8"}

// The closed vocabularies of preset purposes, model tasks and capabilities.
var (
	purposes     = []string{"standard", "throughput", "latency", "long-context"}
	tasks        = []string{"chat", "completion", "embedding", "rerank", "vision-chat"}
	capabilities = []string{"tools", "reasoning", "vision", "long-context", "json-schema"}
)

// Generate validates the curated input and assembles the snapshot: families,
// the node-type vocabulary of the cloud node types plus generated fallbacks,
// models with their deployment guidance, draft models, backends and the fit
// matrix, then runs the registered extensions. A fit exists for a model,
// quantization, KV cache dtype and family where some preset with that
// quantization and KV cache dtype runs on the family (presetMode). It is the
// smallest layout (GPUs, then nodes) such a preset needs to hold the weights
// plus the KV cache of its context in the memory budget, with the largest
// context that layout holds, capped at the model's ceiling, and the estimate at
// that preset's context. A preset whose quantization fits families its backend
// version runs on none of, or an unparsable NVIDIA compute capability, fails
// generation. The report lists the models without a deployment minimum and the
// presets whose quantization fits no family. Pure and deterministic, so the
// snapshot digest is stable for identical input.
func Generate(families []Family, nodeTypes []NodeType, templates []ModelTemplate, drafts []DraftModel, backends []Backend) (CatalogSnapshot, []string, error) {
	vocab, err := NodeTypeVocabulary(families, nodeTypes)
	if err != nil {
		return CatalogSnapshot{}, nil, err
	}
	if err := checkComputeCapabilities(families, backends); err != nil {
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
	versions := map[[2]string]BackendVersion{}
	for _, b := range backends {
		for _, v := range b.Versions {
			versions[[2]string{b.Name, v.Version}] = v
		}
	}
	var errs []error
	var report []string
	for i, t := range templates {
		t.KVBytesPerToken = t.Architecture.KVBytesPerToken()
		var fits []candidate
		if t.KVBytesPerToken > 0 {
			var unfit []string
			fits, unfit, err = modelFits(t, families, versions)
			errs = append(errs, err)
			for _, p := range unfit {
				report = append(report, fmt.Sprintf("%s/%s: fits no family", t.Slug, p))
			}
		}
		t.Deployment = deployment(fits, vocab)
		if t.Deployment.Minimum == nil {
			report = append(report, fmt.Sprintf("%s: no deployment minimum: no float or fp8 fit decodes %d tokens/s per sequence", t.Slug, DecentDecodeTokensPerSec))
		}
		snap.Models[i] = t
		for _, c := range fits {
			snap.Fits = append(snap.Fits, c.fit)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return CatalogSnapshot{}, nil, err
	}
	slices.SortFunc(snap.Fits, func(a, b ModelFit) int {
		return cmp.Or(strings.Compare(a.ModelSlug, b.ModelSlug), strings.Compare(a.DeviceFamily, b.DeviceFamily),
			strings.Compare(a.Quantization, b.Quantization), strings.Compare(a.KVCacheDType, b.KVCacheDType))
	})
	// The core alone first: extensions validate once they generated payloads.
	if err := snap.validate(newRegistry()); err != nil {
		return CatalogSnapshot{}, nil, err
	}
	if err := RunExtensions(&snap); err != nil {
		return CatalogSnapshot{}, nil, err
	}
	return snap, report, nil
}

// checkComputeCapabilities refuses NVIDIA compute capabilities that do not
// parse as numbers, which would fail every comparison and drop fits silently.
func checkComputeCapabilities(families []Family, backends []Backend) error {
	bad := func(cc string) bool { _, err := versionNumbers(cc); return err != nil }
	var errs []error
	for _, f := range families {
		if f.Vendor == "nvidia" && bad(f.ComputeCapability) {
			errs = append(errs, fmt.Errorf("family %s: compute capability %q is not numeric", f.Name, f.ComputeCapability))
		}
	}
	for _, b := range backends {
		for _, v := range b.Versions {
			if v.MinComputeCapability != "" && bad(v.MinComputeCapability) {
				errs = append(errs, fmt.Errorf("backend %s %s: min_compute_capability %q is not numeric", b.Name, v.Version, v.MinComputeCapability))
			}
			for _, kind := range slices.Sorted(maps.Keys(v.Quantizations)) {
				if from := v.Quantizations[kind].EmulatedFrom; from != "" && bad(from) {
					errs = append(errs, fmt.Errorf("backend %s %s: %s emulated_from %q is not numeric", b.Name, v.Version, kind, from))
				}
			}
		}
	}
	return errors.Join(errs...)
}

// candidate is a fit with what deployment guidance ranks it by.
type candidate struct {
	fit    ModelFit
	preset string  // the preset it was sized for
	memory float64 // total GPU memory
	rank   int     // quantization order, then auto KV before fp8
	guides bool    // its quantization kind may drive guidance
}

// modelFits derives t's fits and names the presets whose quantization fits no
// family. It fails on a preset whose quantization fits families its backend
// version runs on none of; presets of unknown backend versions or quantization
// kinds are left to Validate.
func modelFits(t ModelTemplate, families []Family, versions map[[2]string]BackendVersion) ([]candidate, []string, error) {
	var out []candidate
	sized := map[int]bool{}
	fitting := map[int][]string{}
	runs := map[int]bool{}
	for qi, q := range t.Quantizations {
		for ki, kvd := range kvDTypes {
			for _, fam := range families {
				var best *Preset
				var gpus, nodes int
				var mode FitMode
				for pi := range t.Presets {
					p := &t.Presets[pi]
					bv, known := versions[[2]string{p.Backend, p.BackendVersion}]
					if _, kind := bv.Quantizations[q.Kind]; p.Quantization != q.Name || kvDType(*p) != kvd || !known || !kind {
						continue
					}
					sized[pi] = true
					g, n, ok := layout(t, q, fam, *p, kvd)
					if !ok {
						continue
					}
					fitting[pi] = append(fitting[pi], fam.Name)
					m := presetMode(fam, *p, q.Kind, bv)
					if m == "" {
						continue
					}
					runs[pi] = true
					if best == nil || g < gpus || g == gpus && n < nodes {
						best, gpus, nodes = p, g, n
					}
					if mode != FitNative {
						mode = m
					}
				}
				if best == nil {
					continue
				}
				maxCtx := int(min(contextHeld(t, q, fam, gpus, kvd), float64(t.ContextMax)))
				perf := Estimate(t, q, fam, gpus, presetContext(t, *best), kvd)
				out = append(out, candidate{
					fit: ModelFit{ModelSlug: t.Slug, DeviceFamily: fam.Name, Quantization: q.Name, KVCacheDType: kvd, Mode: mode,
						GPUCount: gpus, Nodes: nodes, MaxContext: maxCtx,
						VRAMRequiredGB: math.Ceil((q.WeightsGB+kvGB(t, kvd, maxCtx))*100) / 100, Performance: &perf},
					preset: best.Name, memory: fam.MemoryGB * float64(gpus), rank: qi*len(kvDTypes) + ki, guides: slices.Contains(guidanceKinds, q.Kind),
				})
			}
		}
	}
	var unfit []string
	var errs []error
	for pi, p := range t.Presets {
		if !sized[pi] {
			continue
		}
		switch fams := fitting[pi]; {
		case len(fams) == 0:
			unfit = append(unfit, p.Name)
		case !runs[pi]:
			errs = append(errs, fmt.Errorf("%s: runs on none of the families its quantization fits (%s)", presetID(t, p), strings.Join(fams, ", ")))
		}
	}
	return out, unfit, errors.Join(errs...)
}

// layout is the smallest GPU count, and the nodes it spans, on which preset p
// holds q's weights plus the KV cache of its context within fam's memory
// budget: tensor-parallel degrees dividing the attention heads on each of its
// nodes, else its explicit tensor times pipeline parallel; a disaggregated
// preset needs the model on each side, so its smaller side.
func layout(t ModelTemplate, q Quantization, fam Family, p Preset, kvd string) (gpus, nodes int, ok bool) {
	heads, top := t.Architecture.AttentionHeads, p.Topology
	nodes = 1
	if top.Kind == "multi-node" {
		nodes = top.Nodes
	}
	var sizes []int
	switch {
	case nodes < 1 || heads < 1:
	case top.Kind == "disaggregated":
		if top.Prefill > 0 && top.Decode > 0 && heads%top.Prefill == 0 && heads%top.Decode == 0 {
			sizes = []int{min(top.Prefill, top.Decode)}
		}
	case p.TensorParallel > 0:
		sizes = []int{p.TensorParallel * max(top.Pipeline, 1)}
	default:
		for _, d := range tpSizes {
			if heads%(nodes*d) == 0 {
				sizes = append(sizes, nodes*d)
			}
		}
	}
	for _, g := range sizes {
		if contextHeld(t, q, fam, g, kvd) >= float64(presetContext(t, p)) {
			return g, nodes, true
		}
	}
	return 0, 0, false
}

// contextHeld is the context, in tokens, gpus GPUs of fam hold next to q's
// weights within the memory budget.
func contextHeld(t ModelTemplate, q Quantization, fam Family, gpus int, kvd string) float64 {
	return (fam.MemoryGB*GPUMemoryUtilization*float64(gpus) - q.WeightsGB) * 1e9 / kvBytes(t, kvd)
}

// presetContext is the context a preset deploys with: its own, else the
// model's default, within the model's ceiling.
func presetContext(t ModelTemplate, p Preset) int {
	return min(cmp.Or(p.Context, t.DefaultContext), t.ContextMax)
}

func kvDType(p Preset) string { return cmp.Or(p.KVCacheDType, "auto") }

// presetMode is how preset p on backend version bv runs a quantization of kind
// on fam: native when the kind needs no dtype or one fam has, emulated on
// NVIDIA from the kind's EmulatedFrom compute capability, and not at all ("")
// when p excludes fam's vendor, bv has no image for it or fam is below bv's
// minimum compute capability. Compute capabilities apply to NVIDIA only: AMD gfx
// targets are not ordered against them.
func presetMode(fam Family, p Preset, kind string, bv BackendVersion) FitMode {
	qs, ok := bv.Quantizations[kind]
	nvidia := fam.Vendor == "nvidia"
	switch {
	case !ok, bv.Images[fam.Vendor] == "", len(p.Vendors) > 0 && !slices.Contains(p.Vendors, fam.Vendor),
		nvidia && bv.MinComputeCapability != "" && !atLeast(fam.ComputeCapability, bv.MinComputeCapability):
		return ""
	case qs.NativeDType == "" || slices.Contains(fam.DTypes, qs.NativeDType):
		return FitNative
	case nvidia && qs.EmulatedFrom != "" && atLeast(fam.ComputeCapability, qs.EmulatedFrom):
		return FitEmulated
	}
	return ""
}

// atLeast compares NVIDIA compute capabilities ("8.9") numerically.
func atLeast(cc, floor string) bool {
	c, err := compareVersions(cc, floor)
	return err == nil && c >= 0
}

// deployment picks a model's minimum and optimal setups among its fits of a
// guidance kind (spec section 5) whose estimate serves at least one sequence at
// DecentDecodeTokensPerSec: the minimum is the least total GPU memory (then
// fewest GPUs, family name, quantization order, auto KV first), the optimal the
// highest aggregate per GPU at its decent concurrency.
func deployment(cands []candidate, vocab []NodeType) Deployment {
	smaller := func(a, b *candidate) bool {
		return cmp.Or(cmp.Compare(a.memory, b.memory), cmp.Compare(a.fit.GPUCount, b.fit.GPUCount),
			strings.Compare(a.fit.DeviceFamily, b.fit.DeviceFamily), cmp.Compare(a.rank, b.rank)) < 0
	}
	perGPU := func(c *candidate) float64 { return c.fit.Performance.AggregateTokensPerSec / float64(c.fit.GPUCount) }
	var minimum, optimal *candidate
	for i := range cands {
		c := &cands[i]
		if !c.guides || c.fit.Performance.ConcurrentSequences == 0 {
			continue
		}
		if minimum == nil || smaller(c, minimum) {
			minimum = c
		}
		if optimal == nil || perGPU(c) > perGPU(optimal) || perGPU(c) == perGPU(optimal) && smaller(c, optimal) {
			optimal = c
		}
	}
	return Deployment{Minimum: recommend(minimum, vocab), Optimal: recommend(optimal, vocab)}
}

func recommend(c *candidate, vocab []NodeType) *Recommendation {
	if c == nil {
		return nil
	}
	f, p := c.fit, c.fit.Performance
	return &Recommendation{Family: f.DeviceFamily, GPUs: f.GPUCount, Nodes: f.Nodes, Quantization: f.Quantization, Preset: c.preset,
		Context: p.Context, NodeTypes: exampleNodeTypes(vocab, f.DeviceFamily, (f.GPUCount+f.Nodes-1)/f.Nodes),
		DecodeTokensPerSec: p.DecodeTokensPerSec, AggregateTokensPerSec: p.AggregateTokensPerSec,
		Source: "estimate", Confidence: "estimate", FitKeys: []string{FitKey(f)}}
}

// exampleNodeTypes lists up to three node types of family with at least gpus
// GPUs each: cloud instances, fewest GPUs first, then the smallest fallback.
func exampleNodeTypes(vocab []NodeType, family string, gpus int) []string {
	var cloud []NodeType
	var fallback *NodeType
	for i, nt := range vocab {
		switch {
		case nt.Family != family || nt.GPUs < gpus:
		case !strings.HasPrefix(nt.ID, fallbackProvider):
			cloud = append(cloud, nt)
		case fallback == nil || nt.GPUs < fallback.GPUs:
			fallback = &vocab[i]
		}
	}
	slices.SortStableFunc(cloud, func(a, b NodeType) int { return cmp.Compare(a.GPUs, b.GPUs) })
	if fallback != nil {
		cloud = append(cloud, *fallback)
	}
	var out []string
	for _, nt := range cloud[:min(len(cloud), 3)] {
		out = append(out, nt.ID)
	}
	return out
}

// Validate checks that every model source has a known type and is pinned, that
// every draft model is pinned on a repository and targets known models, that
// model tasks, capabilities and preset purposes are in their closed
// vocabularies, and that every preset names a quantization of its model whose
// kind its backend version runs (natively in the quantization's requires_dtype,
// when set), an existing backend version, and a draft model for its backend,
// model and method that fits next to the model on every fit the preset deploys
// on, stays within the model's context, spans at least two nodes when
// multi-node and renders on its backend version. It then runs Validate of every
// registered extension. Payloads of namespaces no registered extension owns are
// preserved and ignored. Embedders can run it on a snapshot fetched from an
// upstream.
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
	families := map[string]Family{}
	for _, f := range s.Families {
		families[f.Name] = f
	}
	fits := map[string][]ModelFit{}
	for _, f := range s.Fits {
		fits[f.ModelSlug] = append(fits[f.ModelSlug], f)
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
		if !slices.Contains(tasks, m.Task) {
			errs = append(errs, fmt.Errorf("%s: task %q, want one of %s", m.Slug, m.Task, strings.Join(tasks, ", ")))
		}
		for _, c := range m.Capabilities {
			if !slices.Contains(capabilities, c) {
				errs = append(errs, fmt.Errorf("%s: capability %q, want one of %s", m.Slug, c, strings.Join(capabilities, ", ")))
			}
		}
		quants := map[string]Quantization{}
		for _, q := range m.Quantizations {
			quants[q.Name] = q
			for _, src := range q.Sources {
				if err := src.validate(); err != nil {
					errs = append(errs, fmt.Errorf("%s %s: %w", m.Slug, q.Name, err))
				}
			}
		}
		presets := map[string]bool{}
		for _, p := range m.Presets {
			bv, known := versions[[2]string{p.Backend, p.BackendVersion}]
			q, quant := quants[p.Quantization]
			if !slices.Contains(purposes, p.Purpose) {
				errs = append(errs, fmt.Errorf("%s: preset %q: purpose %q, want one of %s", m.Slug, p.Name, p.Purpose, strings.Join(purposes, ", ")))
			}
			switch {
			case presets[p.Name]:
				errs = append(errs, fmt.Errorf("%s: duplicate preset %q", m.Slug, p.Name))
			case !quant:
				errs = append(errs, fmt.Errorf("%s: preset %q: unknown quantization %q", m.Slug, p.Name, p.Quantization))
			case !known:
				errs = append(errs, fmt.Errorf("%s: preset %q: unknown backend version %s %s", m.Slug, p.Name, p.Backend, p.BackendVersion))
			default:
				if err := validatePreset(m, p, q, bv, drafts); err != nil {
					errs = append(errs, err)
				} else if p.Speculative != nil && p.Speculative.DraftModel != "" {
					errs = append(errs, draftFits(m, p, q, bv, drafts[p.Speculative.DraftModel], fits[m.Slug], families))
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
func validatePreset(m ModelTemplate, p Preset, q Quantization, bv BackendVersion, drafts map[string]*DraftModel) error {
	id := presetID(m, p)
	var errs []error
	if qs, ok := bv.Quantizations[q.Kind]; !ok {
		errs = append(errs, fmt.Errorf("%s: quantization kind %q not supported", id, q.Kind))
	} else if q.RequiresDType != "" && q.RequiresDType != qs.NativeDType {
		errs = append(errs, fmt.Errorf("%s: quantization %s requires_dtype %q, but its kind %s is native %q", id, q.Name, q.RequiresDType, q.Kind, qs.NativeDType))
	}
	if p.Context > m.ContextMax {
		errs = append(errs, fmt.Errorf("%s: context %d above context_max %d", id, p.Context, m.ContextMax))
	}
	// Each side's tensor parallel is its GPU count; a preset-wide one is ignored.
	if t := p.Topology; t.Kind == "disaggregated" && (t.Prefill < 1 || t.Decode < 1 || p.TensorParallel != 0) {
		errs = append(errs, fmt.Errorf("%s: disaggregated topology needs prefill_gpus and decode_gpus >= 1 and no tensor_parallel", id))
	}
	if t := p.Topology; t.Kind == "multi-node" && t.Nodes < 2 {
		errs = append(errs, fmt.Errorf("%s: multi-node topology needs nodes >= 2", id))
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

// draftFits checks that a speculative preset holds its draft's weights next to
// the model's and the KV cache of its context on every fit it deploys on: same
// quantization and KV cache dtype, a family it runs on, and its context within
// the fit's.
func draftFits(m ModelTemplate, p Preset, q Quantization, bv BackendVersion, draft *DraftModel, fits []ModelFit, families map[string]Family) error {
	ctx := presetContext(m, p)
	for _, f := range fits {
		fam, ok := families[f.DeviceFamily]
		if !ok || f.Quantization != p.Quantization || f.KVCacheDType != kvDType(p) || ctx > f.MaxContext || presetMode(fam, p, q.Kind, bv) == "" {
			continue
		}
		if q.WeightsGB+draft.WeightsGB+kvGB(m, f.KVCacheDType, ctx) > fam.MemoryGB*GPUMemoryUtilization*float64(f.GPUCount) {
			return fmt.Errorf("%s: weights, draft and %d tokens of KV exceed %d x %s", presetID(m, p), ctx, f.GPUCount, f.DeviceFamily)
		}
	}
	return nil
}

func presetID(m ModelTemplate, p Preset) string {
	return fmt.Sprintf("%s/%s on %s %s", m.Slug, p.Name, p.Backend, p.BackendVersion)
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
