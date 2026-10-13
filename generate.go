// SPDX-License-Identifier: Apache-2.0

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

// GPUMemoryUtilization is the share of GPU memory a fit may use for weights and KV
// cache: below vLLM's 0.92 default to leave room for activations.
const GPUMemoryUtilization = 0.9

// tpSizes are the tensor-parallel GPU counts per node considered, smallest first.
var tpSizes = []int{1, 2, 4, 8}

var kvDTypes = []string{"auto", "fp8"}

// guidanceKinds are the quantization kinds whose fits drive deployment guidance.
var guidanceKinds = []string{"float", "fp8"}

var (
	purposes     = []string{"standard", "throughput", "latency", "long-context"}
	tasks        = []string{"chat", "completion", "embedding", "rerank", "vision-chat"}
	capabilities = []string{"tools", "reasoning", "vision", "long-context", "json-schema"}
)

// Generate validates the curated input, assembles the snapshot with its fit matrix
// and deployment guidance, and runs the registered extensions. Each fit is the
// smallest layout on which some preset holds weights plus its context's KV cache
// in the memory budget. The report lists models without a deployment minimum and
// presets that fit no family. Deterministic, so the digest is stable.
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
	perNode := map[string]int{}
	for _, nt := range vocab {
		perNode[nt.Family] = max(perNode[nt.Family], nt.GPUs)
	}
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
			fits, unfit, err = modelFits(t, families, perNode, versions)
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
			strings.Compare(a.Quantization, b.Quantization), strings.Compare(a.KVCacheDType, b.KVCacheDType), cmp.Compare(a.Nodes, b.Nodes))
	})
	// Core first: extensions validate after they have generated payloads.
	if err := snap.validate(newRegistry()); err != nil {
		return CatalogSnapshot{}, nil, err
	}
	if err := RunExtensions(&snap); err != nil {
		return CatalogSnapshot{}, nil, err
	}
	return snap, report, nil
}

// checkComputeCapabilities rejects non-numeric NVIDIA compute capabilities, which
// would fail every comparison and silently drop fits.
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

type candidate struct {
	fit    ModelFit
	preset string
	memory float64 // total GPU memory
	rank   int     // quantization order, then auto KV before fp8
	guides bool    // single-node fit of a guidance kind
}

// modelFits derives t's fits and names the presets that fit no family. It fails on
// a preset whose fitting families its backend version runs on none of; unknown
// backend versions and quantization kinds are left to Validate.
func modelFits(t ModelTemplate, families []Family, perNode map[string]int, versions map[[2]string]BackendVersion) ([]candidate, []string, error) {
	type choice struct {
		preset *Preset
		size   sizing
		mode   FitMode
	}
	var out []candidate
	sized := map[int]bool{}
	fitting := map[int][]string{}
	runs := map[int]bool{}
	for qi, q := range t.Quantizations {
		for ki, kvd := range kvDTypes {
			for _, fam := range families {
				best := map[int]*choice{} // by node count
				for pi := range t.Presets {
					p := &t.Presets[pi]
					bv, known := versions[[2]string{p.Backend, p.BackendVersion}]
					if _, kind := bv.Quantizations[q.Kind]; p.Quantization != q.Name || kvDType(*p) != kvd || !known || !kind {
						continue
					}
					sized[pi] = true
					s, ok := layout(t, q, fam, *p, kvd, perNode[fam.Name])
					if !ok {
						continue
					}
					fitting[pi] = append(fitting[pi], fam.Name)
					m := presetMode(fam, *p, q.Kind, bv)
					if m == "" {
						continue
					}
					runs[pi] = true
					c := best[s.nodes]
					if c == nil {
						c = &choice{}
						best[s.nodes] = c
					}
					if c.preset == nil || s.gpus < c.size.gpus {
						c.preset, c.size = p, s
					}
					if c.mode != FitNative {
						c.mode = m
					}
				}
				for _, nodes := range slices.Sorted(maps.Keys(best)) {
					c := best[nodes]
					gpus := c.size.gpus
					maxCtx := int(min(contextHeld(t, q, fam, gpus, kvd), float64(t.ContextMax)))
					perf := estimate(t, q, fam, gpus, c.size.stages, presetContext(t, *c.preset), kvd)
					out = append(out, candidate{
						fit: ModelFit{ModelSlug: t.Slug, DeviceFamily: fam.Name, Quantization: q.Name, KVCacheDType: kvd, Mode: c.mode,
							GPUCount: gpus, Nodes: nodes, MaxContext: maxCtx,
							VRAMRequiredGB: math.Ceil((q.WeightsGB+kvGB(t, kvd, maxCtx))*100) / 100, Performance: &perf},
						preset: c.preset.Name, memory: fam.MemoryGB * float64(gpus), rank: qi*len(kvDTypes) + ki,
						guides: nodes == 1 && slices.Contains(guidanceKinds, q.Kind),
					})
				}
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

// sizing is a preset's GPU count, the nodes it spans and its pipeline stages.
type sizing struct{ gpus, nodes, stages int }

// layout is the smallest sizing on which p holds q's weights plus its context's KV
// cache within fam's memory budget. Tensor-parallel degrees must divide the
// attention heads; a disaggregated preset needs the model on each side, so its
// smaller side counts.
func layout(t ModelTemplate, q Quantization, fam Family, p Preset, kvd string, perNode int) (sizing, bool) {
	heads, top := t.Architecture.AttentionHeads, p.Topology
	var sizes []sizing
	switch {
	case heads < 1:
	case top.Kind == "multi-node":
		gpus := top.Nodes * perNode
		if stages := cmp.Or(top.Pipeline, top.Nodes); top.Nodes > 1 && perNode > 0 && stages > 0 && gpus%stages == 0 {
			if tp := gpus / stages; heads%tp == 0 && (p.TensorParallel == 0 || p.TensorParallel == tp) {
				sizes = []sizing{{gpus, top.Nodes, stages}}
			}
		}
	case top.Kind == "disaggregated":
		if top.Prefill > 0 && top.Decode > 0 && heads%top.Prefill == 0 && heads%top.Decode == 0 {
			sizes = []sizing{{min(top.Prefill, top.Decode), 1, 1}}
		}
	case p.TensorParallel > 0:
		sizes = []sizing{{p.TensorParallel * max(top.Pipeline, 1), 1, 1}}
	default:
		for _, d := range tpSizes {
			if heads%d == 0 {
				sizes = append(sizes, sizing{d, 1, 1})
			}
		}
	}
	for _, s := range sizes {
		if contextHeld(t, q, fam, s.gpus, kvd) >= float64(presetContext(t, p)) {
			return s, true
		}
	}
	return sizing{}, false
}

// contextHeld is the tokens of KV cache gpus GPUs of fam hold next to q's weights.
func contextHeld(t ModelTemplate, q Quantization, fam Family, gpus int, kvd string) float64 {
	return (fam.MemoryGB*GPUMemoryUtilization*float64(gpus) - q.WeightsGB) * 1e9 / kvBytes(t, kvd)
}

// presetContext is p's context, else the model default, capped at ContextMax.
func presetContext(t ModelTemplate, p Preset) int {
	return min(cmp.Or(p.Context, t.DefaultContext), t.ContextMax)
}

func kvDType(p Preset) string { return cmp.Or(p.KVCacheDType, "auto") }

// presetMode is how p on bv runs a quantization kind on fam, or "" if it cannot.
// Compute capabilities apply to NVIDIA only: AMD gfx targets are not ordered.
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

func atLeast(cc, floor string) bool {
	c, err := compareVersions(cc, floor)
	return err == nil && c >= 0
}

// deployment picks the minimum (least total GPU memory) and optimal (highest
// aggregate per GPU) among single-node guidance fits decoding at least one
// sequence at DecentDecodeTokensPerSec.
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

// exampleNodeTypes lists up to three node types of family with at least gpus GPUs:
// cloud instances fewest GPUs first, then the smallest fallback.
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

// Validate checks the snapshot's sources, vocabularies, presets, draft models and
// fit keys, then runs every registered extension's Validate. Payloads of
// unregistered namespaces are preserved and ignored.
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
	var errs []error
	fits := map[string][]ModelFit{}
	fitKeys := map[string]bool{}
	for _, f := range s.Fits {
		fits[f.ModelSlug] = append(fits[f.ModelSlug], f)
		k := FitKey(f)
		if fitKeys[k] {
			errs = append(errs, fmt.Errorf("fit %s: duplicate", k))
		}
		fitKeys[k] = true
	}
	drafts := map[string]*DraftModel{}
	for i, d := range s.DraftModels {
		if drafts[d.Slug] != nil {
			errs = append(errs, fmt.Errorf("draft model %q: duplicate", d.Slug))
		}
		drafts[d.Slug] = &s.DraftModels[i]
		// Engines take a draft as repo id and revision; OCI refs and paths would not load.
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

// validatePreset renders p for every vendor it runs on, so a field its flag table
// cannot express fails generation instead of a deployment.
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
	// Each side's tensor parallel is its GPU count.
	if t := p.Topology; t.Kind == "disaggregated" && (t.Prefill < 1 || t.Decode < 1 || p.TensorParallel != 0) {
		errs = append(errs, fmt.Errorf("%s: disaggregated topology needs prefill_gpus and decode_gpus >= 1 and no tensor_parallel", id))
	}
	if t := p.Topology; t.Kind == "multi-node" && t.Nodes < 2 {
		errs = append(errs, fmt.Errorf("%s: multi-node topology needs nodes >= 2", id))
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
	images := slices.Sorted(maps.Keys(bv.Images))
	vendors := slices.DeleteFunc(slices.Clone(images), func(v string) bool {
		return bv.Images[v] == "" || len(p.Vendors) > 0 && !slices.Contains(p.Vendors, v)
	})
	if len(vendors) == 0 {
		errs = append(errs, fmt.Errorf("%s: runs on no vendor: images for %v, preset vendors %v", id, images, p.Vendors))
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	roles := []string{""}
	if p.Topology.Kind == "disaggregated" {
		roles = []string{"prefill", "decode"}
	}
	fit := ModelFit{ModelSlug: m.Slug, Quantization: p.Quantization, GPUCount: cmp.Or(p.TensorParallel, 1), Nodes: 1}
	if t := p.Topology; t.Kind == "multi-node" {
		// Span all nodes so pipeline parallel renders as deployed.
		fit.GPUCount, fit.Nodes = fit.GPUCount*cmp.Or(t.Pipeline, t.Nodes), t.Nodes
	}
	for _, vendor := range vendors {
		for _, role := range roles {
			if _, err := RenderRole(m, p, bv, fit, draft, vendor, role, 0); err != nil {
				return err
			}
		}
	}
	return nil
}

// draftFits checks that a speculative preset holds draft and model weights plus
// its context's KV cache on every fit it deploys on.
func draftFits(m ModelTemplate, p Preset, q Quantization, bv BackendVersion, draft *DraftModel, fits []ModelFit, families map[string]Family) error {
	ctx, nodes := presetContext(m, p), 1
	if p.Topology.Kind == "multi-node" {
		nodes = p.Topology.Nodes
	}
	for _, f := range fits {
		fam, ok := families[f.DeviceFamily]
		if !ok || f.Quantization != p.Quantization || f.KVCacheDType != kvDType(p) || f.Nodes != nodes || ctx > f.MaxContext || presetMode(fam, p, q.Kind, bv) == "" {
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

// compareVersions compares dot-separated numeric versions with optional "v":
// v0.9.0 < v0.31.0 and v0.31 == v0.31.0.
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
	var errs []error
	seen := make(map[string]bool, len(src.Files))
	for i, f := range src.Files {
		var err error
		switch {
		case f.Path == "":
			err = fmt.Errorf("file %d: path required", i)
		case seen[f.Path]:
			err = fmt.Errorf("file %d %q: duplicate path", i, f.Path)
		default:
			err = f.validate()
		}
		seen[f.Path] = true
		if err != nil {
			errs = append(errs, fmt.Errorf("%s source %q: %w", src.Type, src.Repo, err))
		}
	}
	return errors.Join(errs...)
}

// git-oid is SHA-1, or SHA-256 in SHA-256 repositories.
var digestHexLen = map[string][]int{"sha256": {64}, "git-oid": {40, 64}}

func (f ModelFile) validate() error {
	alg, sum, _ := strings.Cut(f.Digest, ":")
	switch {
	case f.Size <= 0:
		return fmt.Errorf("file %q: size %d, want > 0", f.Path, f.Size)
	case !slices.Contains(digestHexLen[alg], len(sum)) || strings.Trim(sum, "0123456789abcdef") != "":
		return fmt.Errorf("file %q: digest %q, want sha256:<hex> or git-oid:<hex>", f.Path, f.Digest)
	}
	return nil
}
