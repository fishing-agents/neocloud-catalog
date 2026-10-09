// Package gpustack exports the catalog as a GPUStack model catalog: the YAML
// document GPUStack reads from --model-catalog-file or a catalog source.
package gpustack

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	catalog "github.com/fishman/neocloud-catalog"
	"github.com/fishman/neocloud-catalog/adapters"
)

// Format is the GPUStack model catalog, one model-catalog.yaml.
const Format = "gpustack"

// Reasons the gpustack format skips a preset.
const (
	// SkipNoSource also covers a source of several files, as GPUStack's
	// source names at most one.
	SkipNoSource = "no Hugging Face or ModelScope source"
	SkipBackend  = "backend GPUStack cannot run"
	// SkipTopology covers multi-node, which GPUStack's scheduler decides on
	// itself, and disaggregated presets, whose GPUStack prefill/decode roles
	// are not mapped.
	SkipTopology = "topology GPUStack catalog cannot express"
)

// backends maps catalog backends to GPUStack's BackendEnum values. It has no
// llama.cpp value (schemas/models.py), so llama.cpp runs as a Custom image.
var backends = map[string]string{"vllm": "vLLM", "sglang": "SGLang", "llamacpp": "Custom"}

// GPUStack's model-set vocabulary: CategoryEnum (schemas/models.py) for the
// task, and the capabilities its own catalog (assets/model-catalog.yaml) uses
// besides context/<N>K, which stands for long-context. json-schema has none.
var (
	categories   = map[string]string{"chat": "llm", "completion": "llm", "vision-chat": "llm", "embedding": "embedding", "rerank": "reranker"}
	capabilities = map[string]string{"tools": "tools", "reasoning": "reasoning", "vision": "vision"}
)

func init() { adapters.Register(adapter{}) }

type adapter struct{}

func (adapter) Name() string { return "gpustack" }

func (adapter) Capabilities() adapters.Capabilities {
	return adapters.Capabilities{
		Backends: []string{"vllm", "sglang", "llamacpp"},
		Sources:  []string{catalog.SourceHuggingFace, catalog.SourceModelScope},
	}
}

func (adapter) Formats() []string { return []string{Format} }

func (adapter) SkipReasons(format string) []string {
	if format != Format {
		return nil
	}
	return []string{SkipNoSource, SkipBackend, SkipTopology}
}

// The document is JSON, which GPUStack's yaml.safe_load reads as YAML. Field
// names are GPUStack's (schemas/model_sets.py, schemas/models.py).
type document struct {
	ModelSets   []modelSet `json:"model_sets"`
	DraftModels []struct{} `json:"draft_models"`
}

type modelSet struct {
	Name          string   `json:"name"`
	Description   string   `json:"description,omitempty"`
	Home          string   `json:"home"`
	Size          float64  `json:"size,omitempty"`
	ActivatedSize float64  `json:"activated_size,omitempty"`
	Categories    []string `json:"categories"`
	Capabilities  []string `json:"capabilities"`
	Licenses      []string `json:"licenses,omitempty"`
	ReleaseDate   string   `json:"release_date,omitempty"`
	Specs         []spec   `json:"specs"`
}

type spec struct {
	Mode                string            `json:"mode"`
	Quantization        string            `json:"quantization"`
	GPUFilters          *gpuFilters       `json:"gpu_filters,omitempty"`
	Source              string            `json:"source"`
	HuggingFaceRepoID   string            `json:"huggingface_repo_id,omitempty"`
	HuggingFaceFilename string            `json:"huggingface_filename,omitempty"`
	ModelScopeModelID   string            `json:"model_scope_model_id,omitempty"`
	ModelScopeFilePath  string            `json:"model_scope_file_path,omitempty"`
	Backend             string            `json:"backend"`
	ImageName           string            `json:"image_name"`
	RunCommand          string            `json:"run_command,omitempty"`
	BackendParameters   []string          `json:"backend_parameters,omitempty"`
	Env                 map[string]string `json:"env,omitempty"`
}

type gpuFilters struct {
	Vendor            []string `json:"vendor"`
	ComputeCapability string   `json:"compute_capability,omitempty"`
}

func (a adapter) Export(snap catalog.CatalogSnapshot, format string, _ adapters.Options) (adapters.Export, error) {
	if format != Format {
		return adapters.Export{}, fmt.Errorf("gpustack: unknown format %q", format)
	}
	var e adapters.Export
	doc := document{DraftModels: []struct{}{}}
	for _, m := range snap.Models {
		set := modelSet{
			Name:         m.Name,
			Description:  m.Description,
			Home:         m.Home,
			Size:         m.ParamsB,
			Categories:   []string{cmp.Or(categories[m.Task], "unknown")},
			Capabilities: []string{fmt.Sprintf("context/%dK", m.ContextMax/1024)},
			ReleaseDate:  m.ReleaseDate,
		}
		if m.ParamsActiveB != m.ParamsB {
			set.ActivatedSize = m.ParamsActiveB
		}
		for _, c := range m.Capabilities {
			if c, ok := capabilities[c]; ok {
				set.Capabilities = append(set.Capabilities, c)
			}
		}
		if m.License != "" {
			set.Licenses = []string{m.License}
		}
		// GPUStack fills backend_parameters of a deployment made outside the
		// catalog from the last spec with the same repository, whatever its
		// backend (scheduler/evaluator.py set_default_spec); its default backend
		// is vLLM, so vLLM specs go last, plain ones after speculative ones.
		var groups [3][]spec
		for _, p := range m.Presets {
			r := adapters.Ref{Model: m, Preset: p}
			specs, reason, err := a.specs(snap, r)
			if err != nil {
				return adapters.Export{}, err
			}
			if reason != "" {
				e.Skipped = append(e.Skipped, adapters.Skip{PresetKey: r.Key(), Reason: reason})
				continue
			}
			if set.Home == "" {
				set.Home = home(specs[0])
			}
			group := 0
			if p.Backend == "vllm" {
				group = 2
				if p.Speculative != nil {
					group = 1
				}
			}
			groups[group] = append(groups[group], specs...)
			e.Exported = append(e.Exported, r.Key())
		}
		set.Specs = slices.Concat(groups[:]...)
		if len(set.Specs) > 0 {
			doc.ModelSets = append(doc.ModelSets, set)
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return adapters.Export{}, fmt.Errorf("gpustack: encode: %w", err)
	}
	e.Artifacts = []adapters.Artifact{{Path: "model-catalog.yaml", Body: buf.Bytes()}}
	return e, nil
}

// specs maps one preset to a spec per GPU vendor it has an image for, or names
// the reason it cannot be expressed.
func (a adapter) specs(snap catalog.CatalogSnapshot, r adapters.Ref) ([]spec, string, error) {
	p := r.Preset
	backend, ok := backends[p.Backend]
	if !ok {
		return nil, SkipBackend, nil
	}
	if k := p.Topology.Kind; k != "" && k != "single" {
		return nil, SkipTopology, nil
	}
	src, ok := r.Source(a.Capabilities().Sources...)
	if !ok || len(src.Files) > 1 {
		return nil, SkipNoSource, nil
	}
	bv, ok := adapters.BackendVersion(snap, p.Backend, p.BackendVersion)
	if !ok {
		return nil, "", fmt.Errorf("gpustack: %s/%s: backend %s %s not in the snapshot", r.Model.Slug, p.Name, p.Backend, p.BackendVersion)
	}
	base := spec{
		// GPUStack offers one spec per mode (routes/model_sets.py
		// filter_specs_by_gpu), so every preset needs its own.
		Mode:         p.Name,
		Quantization: strings.ToUpper(p.Quantization),
		Backend:      backend,
		Env:          p.Env,
	}
	var file string
	if len(src.Files) == 1 {
		file = src.Files[0]
	}
	if src.Type == catalog.SourceHuggingFace {
		base.Source, base.HuggingFaceRepoID, base.HuggingFaceFilename = "huggingface", src.Repo, file
	} else {
		base.Source, base.ModelScopeModelID, base.ModelScopeFilePath = "model_scope", src.Repo, file
	}
	q, _ := r.Quantization()
	cc := minComputeCapability(snap, q.Kind, bv)
	var out []spec
	for _, v := range adapters.Vendors(p, bv) {
		// GPUStack matches a spec to GPUs by vendor; a CPU image has none.
		if v == "cpu" {
			continue
		}
		// GPUStack's scheduler sizes the GPU count, so no fit sets tensor
		// parallelism.
		args, err := catalog.Render(r.Model, p, bv, catalog.ModelFit{}, adapters.Draft(snap, p), v)
		if err != nil {
			return nil, "", fmt.Errorf("gpustack: %w", err)
		}
		s := base
		if backend == "Custom" {
			s.RunCommand = runCommand(bv.Command, args)
		} else {
			s.BackendParameters = parameters(args)
		}
		// GPUStack rejects image_name together with backend_version
		// (routes/models.py validate_model_in), and the pinned image is the pin.
		s.ImageName = bv.Images[v]
		// Catalog vendor names are GPUStack's (gpustack_runtime
		// ManufacturerEnum). Its compute capability check fails a GPU without
		// an NVIDIA one, so only NVIDIA specs carry it.
		s.GPUFilters = &gpuFilters{Vendor: []string{v}}
		if v == "nvidia" && cc != "" {
			s.GPUFilters.ComputeCapability = ">=" + cc
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil, SkipBackend, nil
	}
	return out, "", nil
}

// minComputeCapability is the lowest NVIDIA compute capability a quantization
// kind runs on with bv: emulated from the kind's emulated_from where bv
// emulates it, else on the lowest family with its native dtype, never below
// bv's minimum; "" when nothing bounds it.
func minComputeCapability(snap catalog.CatalogSnapshot, kind string, bv catalog.BackendVersion) string {
	qs := bv.Quantizations[kind]
	floor := qs.EmulatedFrom
	if floor == "" && qs.NativeDType != "" {
		for _, f := range snap.Families {
			if f.Vendor == "nvidia" && slices.Contains(f.DTypes, qs.NativeDType) && (floor == "" || ccLess(f.ComputeCapability, floor)) {
				floor = f.ComputeCapability
			}
		}
	}
	if bv.MinComputeCapability != "" && (floor == "" || ccLess(floor, bv.MinComputeCapability)) {
		floor = bv.MinComputeCapability
	}
	return floor
}

// ccLess compares compute capabilities numerically ("9.0" < "10.0").
func ccLess(a, b string) bool {
	parts := func(cc string) []int {
		var n []int
		for f := range strings.SplitSeq(cc, ".") {
			i, _ := strconv.Atoi(f)
			n = append(n, i)
		}
		return n
	}
	return slices.Compare(parts(a), parts(b)) < 0
}

func home(s spec) string {
	if s.Source == "huggingface" {
		return "https://huggingface.co/" + s.HuggingFaceRepoID
	}
	return "https://modelscope.cn/models/" + s.ModelScopeModelID
}

// shlexSpecial are the characters GPUStack's shlex.split acts on.
const shlexSpecial = " \t\n\v\f\r'\"\\"

// parameters writes argv the way GPUStack's catalog does, "--flag=value" for a
// flag with exactly one value. GPUStack re-splits an item that starts with a
// flag through shlex (utils/command.py flatten_to_argv), so a value shlex would
// change becomes an item of its own, which GPUStack passes through verbatim.
func parameters(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if k, v, ok := strings.Cut(a, "="); ok && isKey(k) && strings.ContainsAny(v, shlexSpecial) {
			out = append(out, k, v)
			continue
		}
		if isKey(a) && !strings.Contains(a, "=") && i+1 < len(args) && !isKey(args[i+1]) &&
			(i+2 == len(args) || isKey(args[i+2])) && !strings.ContainsAny(args[i+1], shlexSpecial) {
			out = append(out, a+"="+args[i+1])
			i++
			continue
		}
		out = append(out, a)
	}
	return out
}

// isKey is GPUStack's is_parameter_key: a flag, but not "--" or a negative number.
func isKey(t string) bool {
	if len(t) < 2 || t[0] != '-' || t == "--" {
		return false
	}
	_, err := strconv.ParseFloat(t, 64)
	return err != nil
}

// runCommand is a Custom spec's run_command: GPUStack starts the image's own
// entrypoint (worker/backends/custom.py), which in the pinned images is the
// command's binary, with the rest of the command and the rendered arguments,
// and fills {{model_path}}, {{port}} and {{model_name}} (the served name).
func runCommand(command, args []string) string {
	var tokens []string
	for _, t := range command[min(1, len(command)):] {
		tokens = append(tokens, strings.ReplaceAll(t, "{{model}}", "{{model_name}}"))
	}
	return shellJoin(append(tokens, args...))
}

// shellJoin quotes the tokens GPUStack's shlex.split would otherwise change.
func shellJoin(tokens []string) string {
	quoted := make([]string, len(tokens))
	for i, t := range tokens {
		if t == "" || strings.ContainsAny(t, shlexSpecial) {
			t = "'" + strings.ReplaceAll(t, "'", `'"'"'`) + "'"
		}
		quoted[i] = t
	}
	return strings.Join(quoted, " ")
}
