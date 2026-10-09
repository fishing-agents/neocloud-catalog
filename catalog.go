// Package catalog is the model-template catalog core, split out of neocloud as a
// public, CNCF-donation-ready module. It is pure and dependency-free (stdlib
// only): no GORM, no tenancy, no HTTP. The serialized CatalogSnapshot is the
// embeddable artifact clients pull and pin by digest. Curation and fit derivation
// live here (the outsourced generation); neocloud ingests the committed snapshot.
package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// SchemaVersion is the versioned wire format. Embedders gate on the major: bump
// it on a breaking change to the snapshot shape, the minor on an addition.
const SchemaVersion = "4.1.0"

// Extensions holds payloads of registered extensions keyed by their namespaced
// name (e.g. "neocloud.io/interruption"). The core never interprets them; it
// preserves them and the digest covers them.
type Extensions map[string]json.RawMessage

// Family is a device family (a GPU model, or CPU) with the hardware facts fits
// and estimates derive from. Sources lists the URLs backing the values.
type Family struct {
	Name               string             `json:"name"`
	Vendor             string             `json:"vendor"`
	Architecture       string             `json:"architecture"`
	ComputeCapability  string             `json:"compute_capability"`
	DTypes             []string           `json:"dtypes"`
	MemoryGB           float64            `json:"memory_gb"`
	MemoryBandwidthGBs float64            `json:"memory_bandwidth_gbs"`
	TFLOPS             map[string]float64 `json:"tflops,omitempty"`
	Interconnect       string             `json:"interconnect,omitempty"`
	InterconnectGBs    float64            `json:"interconnect_gbs,omitempty"`
	Sources            []string           `json:"sources"`
	Extensions         Extensions         `json:"extensions,omitempty"`
}

// Quantization is one published checkpoint of a model. Kind is its format;
// RequiresDType is the tensor dtype it needs natively to run at full speed.
// Sources lists where its weights can be fetched, in preference order.
type Quantization struct {
	Name          string        `json:"name"`
	Kind          string        `json:"kind"`
	RequiresDType string        `json:"requires_dtype"`
	WeightsGB     float64       `json:"weights_gb"`
	Sources       []ModelSource `json:"sources,omitempty"`
}

// Model source types.
const (
	SourceHuggingFace = "huggingface"
	SourceModelScope  = "modelscope"
	SourceOCI         = "oci"
	SourceLocal       = "local"
)

// ModelSource locates a checkpoint's weights. Repo is the repository id
// (huggingface, modelscope), the OCI reference without tag or digest (oci), or
// a path on the nodes (local). Revision pins the content: a commit or tag
// (huggingface, modelscope) or a manifest digest (oci). Files is the manifest
// of the files to fetch at that revision; without one the whole revision is.
type ModelSource struct {
	Type     string      `json:"type"`
	Repo     string      `json:"repo"`
	Revision string      `json:"revision,omitempty"`
	Files    []ModelFile `json:"files,omitempty"`
}

// ModelFile is one manifest entry: a path in the source, its size in bytes and
// its digest, "sha256:<hex>" of the content (Git LFS oid) or "git-oid:<hex>",
// the Git blob id of a file stored in Git itself.
type ModelFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"`
}

// Preset is a deployable template: a model at one quantization served by one
// backend version, stated in engine-neutral fields that the backend version's
// flag table renders. MinBackendVersion is the oldest version that supports
// every field used. TensorParallel 0 takes the GPU count from the fit; a nil
// PrefixCaching keeps the engine default. ExtraArgs are passed verbatim. Vendors
// limits it to GPU vendors (empty means any the backend supports).
type Preset struct {
	Name              string            `json:"name"`
	Quantization      string            `json:"quantization"`
	Backend           string            `json:"backend"`
	BackendVersion    string            `json:"backend_version"`
	MinBackendVersion string            `json:"min_backend_version,omitempty"`
	Purpose           string            `json:"purpose"`
	Context           int               `json:"context"`
	TensorParallel    int               `json:"tensor_parallel,omitempty"`
	KVCacheDType      string            `json:"kv_cache_dtype,omitempty"`
	PrefixCaching     *bool             `json:"prefix_caching,omitempty"`
	ToolParser        string            `json:"tool_parser,omitempty"`
	ReasoningParser   string            `json:"reasoning_parser,omitempty"`
	ChatTemplate      string            `json:"chat_template,omitempty"`
	Speculative       *Speculative      `json:"speculative,omitempty"`
	Topology          Topology          `json:"topology,omitzero"`
	Env               map[string]string `json:"env,omitempty"`
	ExtraArgs         []string          `json:"extra_args,omitempty"`
	Vendors           []string          `json:"vendors,omitempty"`
	Extensions        Extensions        `json:"extensions,omitempty"`
}

// Speculative configures speculative decoding. DraftModel names an entry of
// the snapshot's draft models when the method needs one.
type Speculative struct {
	Method     string `json:"method"`
	DraftModel string `json:"draft_model,omitempty"`
	Tokens     int    `json:"tokens"`
}

// Topology is how a preset spreads over nodes: single (the default), multi-node
// (Nodes, in Pipeline pipeline-parallel stages, default one per node) or
// disaggregated (Prefill and Decode GPUs exchanging KV cache over Transfer).
type Topology struct {
	Kind     string `json:"kind"`
	Nodes    int    `json:"nodes,omitempty"`
	Pipeline int    `json:"pipeline,omitempty"`
	Prefill  int    `json:"prefill_gpus,omitempty"`
	Decode   int    `json:"decode_gpus,omitempty"`
	Transfer string `json:"transfer,omitempty"`
}

// DraftModel is a speculative-decoding draft for the Targets model slugs,
// loadable by the listed backends.
type DraftModel struct {
	Slug      string      `json:"slug"`
	Method    string      `json:"method"`
	Targets   []string    `json:"targets"`
	Source    ModelSource `json:"source"`
	WeightsGB float64     `json:"weights_gb"`
	Backends  []string    `json:"backends"`
}

// Backend is an inference server (vLLM, SGLang, ...) and its released versions.
type Backend struct {
	Name     string           `json:"name"`
	Versions []BackendVersion `json:"versions"`
}

// BackendVersion pins a backend release to a container image per vendor. Command
// may use the placeholders {{model_path}}, {{model}} and {{port}}; HealthPath is
// the HTTP readiness path on that port. Quantizations maps the quantization
// kinds it runs to their dtype support; Flags renders presets into its argv.
type BackendVersion struct {
	Version              string                  `json:"version"`
	Images               map[string]string       `json:"images"`
	Command              []string                `json:"command"`
	HealthPath           string                  `json:"health_path"`
	MinComputeCapability string                  `json:"min_compute_capability,omitempty"`
	Quantizations        map[string]QuantSupport `json:"quantizations"`
	Flags                FlagTable               `json:"flags"`
	Defaults             map[string]string       `json:"defaults,omitempty"`
	Extensions           Extensions              `json:"extensions,omitempty"`
}

// QuantSupport is how a backend version runs a quantization kind: natively on
// families with NativeDType, or emulated (weight-only) from the compute
// capability EmulatedFrom.
type QuantSupport struct {
	NativeDType  string `json:"native_dtype"`
	EmulatedFrom string `json:"emulated_from,omitempty"`
}

// FlagTable maps engine-neutral preset fields to one backend version's flags.
// An empty flag means the field is not expressible on that version. The parser,
// template, KV cache dtype, speculative method and KV transfer maps translate
// neutral names to the engine's; ChatTemplates maps a template to its path per
// image vendor, as each vendor's image lays out its files differently.
// Speculative, Disaggregation and MultiNode name how the engine takes the
// setting: "json:<flag>" is vLLM's JSON-valued flag, "vllm" and "sglang" their
// fixed flag sets. SeparateValues renders "flag value" as two arguments for
// engines that reject "--flag=value".
type FlagTable struct {
	Context            string                       `json:"context"`
	TensorParallel     string                       `json:"tensor_parallel,omitempty"`
	PipelineParallel   string                       `json:"pipeline_parallel,omitempty"`
	KVCacheDType       string                       `json:"kv_cache_dtype,omitempty"`
	KVCacheDTypes      map[string]string            `json:"kv_cache_dtypes,omitempty"`
	PrefixCachingOn    string                       `json:"prefix_caching_on,omitempty"`
	PrefixCachingOff   string                       `json:"prefix_caching_off,omitempty"`
	ToolParser         string                       `json:"tool_parser,omitempty"`
	ToolParserNeeds    []string                     `json:"tool_parser_needs,omitempty"`
	ToolParsers        map[string]string            `json:"tool_parsers,omitempty"`
	ReasoningParser    string                       `json:"reasoning_parser,omitempty"`
	ReasoningParsers   map[string]string            `json:"reasoning_parsers,omitempty"`
	ChatTemplate       string                       `json:"chat_template,omitempty"`
	ChatTemplates      map[string]map[string]string `json:"chat_templates,omitempty"`
	Speculative        string                       `json:"speculative,omitempty"`
	SpeculativeMethods map[string]string            `json:"speculative_methods,omitempty"`
	Disaggregation     string                       `json:"disaggregation,omitempty"`
	KVTransfers        map[string]string            `json:"kv_transfers,omitempty"`
	MultiNode          string                       `json:"multi_node,omitempty"`
	SeparateValues     bool                         `json:"separate_values,omitempty"`
}

// Architecture is the subset of a model's config that sizes its KV cache and
// constrains tensor parallelism (attention heads must divide the GPU count).
type Architecture struct {
	Layers         int `json:"layers"`
	AttentionHeads int `json:"attention_heads"`
	KVHeads        int `json:"kv_heads"`
	HeadDim        int `json:"head_dim"`
}

// KVBytesPerToken is K and V for every layer and KV head at a 2-byte (16-bit)
// cache dtype, vLLM's default for 16-bit models.
func (a Architecture) KVBytesPerToken() float64 {
	return float64(2 * a.Layers * a.KVHeads * a.HeadDim * 2)
}

// ModelTemplate describes a model an operator can launch: its metadata, its
// weights per quantization, its architecture, and the context limits the vendor
// curates. ParamsActiveB equals ParamsB for dense models. KVBytesPerToken is
// derived from Architecture by Generate.
type ModelTemplate struct {
	Slug            string         `json:"slug"`
	Name            string         `json:"name"`
	Family          string         `json:"family"`
	Description     string         `json:"description,omitempty"`
	Task            string         `json:"task"`
	Capabilities    []string       `json:"capabilities,omitempty"`
	ParamsB         float64        `json:"params_b"`
	ParamsActiveB   float64        `json:"params_active_b"`
	ReleaseDate     string         `json:"release_date,omitempty"`
	Home            string         `json:"home,omitempty"`
	Icon            string         `json:"icon,omitempty"`
	Aliases         []string       `json:"aliases,omitempty"`
	DefaultContext  int            `json:"default_context"`
	ContextMax      int            `json:"context_max"`
	Quantizations   []Quantization `json:"quantizations"`
	Architecture    Architecture   `json:"architecture"`
	KVBytesPerToken float64        `json:"kv_bytes_per_token"`
	// License is the SPDX id or name of the model licence; Gated marks sources
	// that need an authenticated, licence-accepted account to download.
	License    string     `json:"license,omitempty"`
	Gated      bool       `json:"gated,omitempty"`
	Presets    []Preset   `json:"presets,omitempty"`
	Deployment Deployment `json:"deployment,omitzero"`
	Extensions Extensions `json:"extensions,omitempty"`
}

// kvBytesPerToken prefers the stored value (snapshots assembled elsewhere may
// omit architecture) and derives it otherwise.
func (t ModelTemplate) kvBytesPerToken() float64 {
	if t.KVBytesPerToken > 0 {
		return t.KVBytesPerToken
	}
	return t.Architecture.KVBytesPerToken()
}

// FitMode is whether a fit runs its quantization on the family's native dtype
// or emulated (weight-only, slower).
type FitMode string

// Fit modes.
const (
	FitNative   FitMode = "native"
	FitEmulated FitMode = "emulated"
)

// ModelFit states that a model at a quantization and KV cache dtype runs on
// GPUCount GPUs of a device family (tensor parallel) across Nodes, with the
// largest context that fits and the VRAM that context needs across those GPUs,
// rounded up to 0.01 GB. Profile is nil for whole GPUs.
type ModelFit struct {
	ModelSlug      string       `json:"model_slug"`
	DeviceFamily   string       `json:"device_family"`
	Profile        *string      `json:"profile,omitempty"`
	Quantization   string       `json:"quantization"`
	KVCacheDType   string       `json:"kv_cache_dtype"`
	Mode           FitMode      `json:"mode"`
	GPUCount       int          `json:"gpu_count"`
	Nodes          int          `json:"nodes"`
	MaxContext     int          `json:"max_context"`
	VRAMRequiredGB float64      `json:"vram_required_gb"`
	Performance    *Performance `json:"performance,omitempty"`
	Extensions     Extensions   `json:"extensions,omitempty"`
}

// Performance is a per-fit throughput figure at Context tokens per sequence:
// decode of one sequence alone, the most concurrent sequences that each still
// decode DecentDecodeTokensPerSec, and their aggregate (see Estimate). Source is
// "estimate" in the catalog; observed values come from the collection service.
type Performance struct {
	Source                string  `json:"source"`
	DecodeTokensPerSec    float64 `json:"decode_tokens_per_sec"`
	ConcurrentSequences   int     `json:"concurrent_sequences"`
	AggregateTokensPerSec float64 `json:"aggregate_tokens_per_sec"`
	Context               int     `json:"context"`
}

// Recommendation is deployment guidance, deliberately coarser than run metrics.
type Recommendation struct {
	Family                string   `json:"family"`
	GPUs                  int      `json:"gpus"`
	Nodes                 int      `json:"nodes"`
	Quantization          string   `json:"quantization"`
	Preset                string   `json:"preset"`
	Context               int      `json:"context"`
	NodeTypes             []string `json:"node_types,omitempty"`
	DecodeTokensPerSec    float64  `json:"decode_tokens_per_sec"`
	AggregateTokensPerSec float64  `json:"aggregate_tokens_per_sec"`
	Source                string   `json:"source"`
	Confidence            string   `json:"confidence"`
	FitKeys               []string `json:"fit_keys"`
}

// Deployment is a model's minimum and optimal recommended setups.
type Deployment struct {
	Minimum *Recommendation `json:"minimum,omitempty"`
	Optimal *Recommendation `json:"optimal,omitempty"`
}

// NodeType is one entry of the closed node vocabulary: a cloud instance id
// ("aws/p5.48xlarge") or the fallback "other/<family>x<gpus>". Network,
// storage and CPU facts are derived from the id later, outside the catalog.
type NodeType struct {
	ID     string `json:"id"`
	Family string `json:"family"`
	GPUs   int    `json:"gpus"`
	Source string `json:"source,omitempty"`
}

// CatalogSnapshot is the immutable, versioned payload served to clients. Version
// changes when the catalog changes; Digest is the content hash clients pin and
// the ETag used for conditional gets.
type CatalogSnapshot struct {
	SchemaVersion string          `json:"schema_version"`
	Version       string          `json:"version"`
	GeneratedAt   time.Time       `json:"generated_at"`
	Families      []Family        `json:"families"`
	NodeTypes     []NodeType      `json:"node_types"`
	Models        []ModelTemplate `json:"models"`
	DraftModels   []DraftModel    `json:"draft_models"`
	Fits          []ModelFit      `json:"fits"`
	Backends      []Backend       `json:"backends"`
}

// VRAMRequired computes the VRAM a model needs at a given context for the named
// quantization: weights plus the KV cache that grows with context. Returns false
// if the quantization is unknown.
func VRAMRequired(t ModelTemplate, quant string, context int) (float64, bool) {
	for _, q := range t.Quantizations {
		if q.Name == quant {
			return q.WeightsGB + t.kvBytesPerToken()*float64(context)/1e9, true
		}
	}
	return 0, false
}

// Fit reports whether a model at a quantization runs on a fit row at the given
// context: same quantization, within the row's context, within its VRAM at the
// row's KV cache dtype.
func Fit(t ModelTemplate, f ModelFit, quant string, context int) bool {
	if quant != f.Quantization || context > f.MaxContext {
		return false
	}
	for _, q := range t.Quantizations {
		if q.Name == quant {
			return q.WeightsGB+kvGB(t, f.KVCacheDType, context) <= f.VRAMRequiredGB
		}
	}
	return false
}

// Digest returns the sha256 of the snapshot's content: every field except
// GeneratedAt, including extension payloads, so identical content yields an
// identical digest regardless of when it was assembled.
func (s CatalogSnapshot) Digest() string {
	s.GeneratedAt = time.Time{}
	h := sha256.New()
	_ = json.NewEncoder(h).Encode(s)
	return hex.EncodeToString(h.Sum(nil))
}
