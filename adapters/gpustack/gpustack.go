// Package gpustack exports the catalog as a GPUStack model catalog: the YAML
// document GPUStack reads from --model-catalog-file or a catalog source.
package gpustack

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	catalog "github.com/fishman/neocloud-catalog"
	"github.com/fishman/neocloud-catalog/adapters"
)

// Format is the GPUStack model catalog, one model-catalog.yaml.
const Format = "gpustack"

// Reasons the gpustack format skips a preset.
const (
	SkipNoSource = "no Hugging Face or ModelScope source"
	SkipBackend  = "backend GPUStack cannot run"
)

// backends maps catalog backends to GPUStack's BackendEnum values.
var backends = map[string]string{"vllm": "vLLM", "sglang": "SGLang"}

func init() { adapters.Register(adapter{}) }

type adapter struct{}

func (adapter) Name() string { return "gpustack" }

func (adapter) Capabilities() adapters.Capabilities {
	return adapters.Capabilities{
		Backends: []string{"vllm", "sglang"},
		Sources:  []string{catalog.SourceHuggingFace, catalog.SourceModelScope},
	}
}

func (adapter) Formats() []string { return []string{Format} }

func (adapter) SkipReasons(format string) []string {
	if format != Format {
		return nil
	}
	return []string{SkipNoSource, SkipBackend}
}

// The document is JSON, which GPUStack's yaml.safe_load reads as YAML. Field
// names are GPUStack's (schemas/model_sets.py, schemas/models.py).
type document struct {
	ModelSets   []modelSet `json:"model_sets"`
	DraftModels []struct{} `json:"draft_models"`
}

type modelSet struct {
	Name         string   `json:"name"`
	Home         string   `json:"home"`
	Size         float64  `json:"size,omitempty"`
	Categories   []string `json:"categories"`
	Capabilities []string `json:"capabilities"`
	Licenses     []string `json:"licenses,omitempty"`
	Specs        []spec   `json:"specs"`
}

type spec struct {
	Mode              string      `json:"mode"`
	Quantization      string      `json:"quantization"`
	GPUFilters        *gpuFilters `json:"gpu_filters,omitempty"`
	Source            string      `json:"source"`
	HuggingFaceRepoID string      `json:"huggingface_repo_id,omitempty"`
	ModelScopeModelID string      `json:"model_scope_model_id,omitempty"`
	Backend           string      `json:"backend"`
	ImageName         string      `json:"image_name"`
	BackendParameters []string    `json:"backend_parameters"`
}

type gpuFilters struct {
	Vendor []string `json:"vendor"`
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
			Size:         m.ParamsB,
			Categories:   []string{"llm"},
			Capabilities: []string{fmt.Sprintf("context/%dK", m.ContextMax/1024)},
		}
		if m.License != "" {
			set.Licenses = []string{m.License}
		}
		// GPUStack fills backend_parameters of a deployment made outside the
		// catalog from the last spec with the same repository, whatever its
		// backend (scheduler/evaluator.py set_default_spec); its default backend
		// is vLLM, so vLLM specs go last.
		var vllm []spec
		for _, p := range m.Presets {
			r := adapters.Ref{Model: m, Preset: p}
			s, reason, err := a.spec(snap, r)
			if err != nil {
				return adapters.Export{}, err
			}
			if reason != "" {
				e.Skipped = append(e.Skipped, adapters.Skip{PresetKey: r.Key(), Reason: reason})
				continue
			}
			if set.Home == "" {
				set.Home = home(s)
			}
			if p.Backend == "vllm" {
				vllm = append(vllm, s)
			} else {
				set.Specs = append(set.Specs, s)
			}
			e.Exported = append(e.Exported, r.Key())
		}
		set.Specs = append(set.Specs, vllm...)
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

// spec maps one preset, or names the reason it cannot be expressed.
func (a adapter) spec(snap catalog.CatalogSnapshot, r adapters.Ref) (spec, string, error) {
	p := r.Preset
	backend, ok := backends[p.Backend]
	if !ok {
		return spec{}, SkipBackend, nil
	}
	src, ok := r.Source(a.Capabilities().Sources...)
	if !ok {
		return spec{}, SkipNoSource, nil
	}
	bv, ok := adapters.BackendVersion(snap, p.Backend, p.BackendVersion)
	if !ok {
		return spec{}, "", fmt.Errorf("gpustack: %s/%s: backend %s %s not in the snapshot", r.Model.Slug, p.Name, p.Backend, p.BackendVersion)
	}
	ctx, _ := adapters.ContextArg(p.Backend, r.Model.DefaultContext)
	s := spec{
		// GPUStack offers one spec per mode (routes/model_sets.py
		// filter_specs_by_gpu), so every preset needs its own.
		Mode:         p.Name,
		Quantization: strings.ToUpper(p.Quantization),
		Backend:      backend,
		// GPUStack rejects image_name together with backend_version
		// (routes/models.py validate_model_in), and the pinned image is the pin.
		ImageName:         bv.Image,
		BackendParameters: append(parameters(p.Args), ctx),
	}
	if src.Type == catalog.SourceHuggingFace {
		s.Source, s.HuggingFaceRepoID = "huggingface", src.Repo
	} else {
		s.Source, s.ModelScopeModelID = "model_scope", src.Repo
	}
	// Catalog vendor names are GPUStack's (gpustack_runtime ManufacturerEnum),
	// which it compares lower-cased; a vendor it does not know matches no GPU.
	vendors := p.Vendors
	if len(vendors) == 0 {
		vendors = bv.Vendors
	}
	if len(vendors) > 0 {
		s.GPUFilters = &gpuFilters{Vendor: vendors}
	}
	return s, "", nil
}

func home(s spec) string {
	if s.Source == "huggingface" {
		return "https://huggingface.co/" + s.HuggingFaceRepoID
	}
	return "https://modelscope.cn/models/" + s.ModelScopeModelID
}

// parameters writes argv the way GPUStack's catalog does, "--flag=value" for a
// flag with exactly one value. GPUStack re-splits an item that starts with a
// flag through shlex (utils/command.py flatten_to_argv), so a value shlex would
// change stays an item of its own, which GPUStack passes through verbatim.
func parameters(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if isKey(a) && !strings.Contains(a, "=") && i+1 < len(args) && !isKey(args[i+1]) &&
			(i+2 == len(args) || isKey(args[i+2])) && !strings.ContainsAny(args[i+1], " \t\n\v\f\r'\"\\") {
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
