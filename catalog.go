// SPDX-License-Identifier: Apache-2.0

// Package catalog is the model-template catalog core: stdlib only, no storage or
// HTTP. The serialized CatalogSnapshot is the artifact clients pull and pin by digest.
package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// SchemaVersion is the snapshot wire format: bump the major on a breaking change,
// the minor on an addition.
const SchemaVersion = "4.1.0"

// Extensions holds registered extension payloads by namespaced name. The core
// preserves them uninterpreted; the digest covers them.
type Extensions map[string]json.RawMessage

// Family is a device family (a GPU model, or CPU) and its hardware facts.
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

// Quantization is one published checkpoint of a model; Sources are in preference order.
type Quantization struct {
	Name          string        `json:"name"`
	Kind          string        `json:"kind"`
	RequiresDType string        `json:"requires_dtype"`
	WeightsGB     float64       `json:"weights_gb"`
	Sources       []ModelSource `json:"sources,omitempty"`
}

const (
	SourceHuggingFace = "huggingface"
	SourceModelScope  = "modelscope"
	SourceOCI         = "oci"
	SourceLocal       = "local"
)

// ModelSource locates a checkpoint's weights. Repo is a repo id, an OCI reference
// without tag or digest, or a node path; Revision is a commit, tag or manifest
// digest. Without Files the whole revision is fetched.
type ModelSource struct {
	Type     string      `json:"type"`
	Repo     string      `json:"repo"`
	Revision string      `json:"revision,omitempty"`
	Files    []ModelFile `json:"files,omitempty"`
}

// ModelFile is one manifest entry. Digest is "sha256:<hex>" of the content (the
// Git LFS oid) or "git-oid:<hex>" (the Git blob id).
type ModelFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"`
}

// Preset is a model at one quantization on one backend version, in engine-neutral
// fields. TensorParallel 0 takes the fit's GPU count; nil PrefixCaching keeps the
// engine default; empty Vendors means any.
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

// Speculative configures speculative decoding; DraftModel names a snapshot draft model.
type Speculative struct {
	Method     string `json:"method"`
	DraftModel string `json:"draft_model,omitempty"`
	Tokens     int    `json:"tokens"`
}

// Topology is single (default), multi-node or disaggregated prefill/decode.
// Pipeline defaults to one stage per node.
type Topology struct {
	Kind     string `json:"kind"`
	Nodes    int    `json:"nodes,omitempty"`
	Pipeline int    `json:"pipeline,omitempty"`
	Prefill  int    `json:"prefill_gpus,omitempty"`
	Decode   int    `json:"decode_gpus,omitempty"`
	Transfer string `json:"transfer,omitempty"`
}

// DraftModel is a speculative-decoding draft for the Targets model slugs.
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

// BackendVersion pins a backend release to an image per vendor. Command may use
// {{model_path}}, {{model}} and {{port}}; HealthPath is served on that port.
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

// QuantSupport is how a backend version runs a quantization kind: natively, or
// weight-only emulated from compute capability EmulatedFrom.
type QuantSupport struct {
	NativeDType  string `json:"native_dtype"`
	EmulatedFrom string `json:"emulated_from,omitempty"`
}

// FlagTable maps engine-neutral preset fields to one backend version's flags; an
// empty flag means unsupported. ChatTemplates is keyed by image vendor because
// image layouts differ. Speculative, Disaggregation and MultiNode are "json:<flag>",
// "vllm" or "sglang". SeparateValues emits "--flag value" for engines rejecting "=".
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

// Architecture is the model config that sizes the KV cache and limits tensor parallelism.
type Architecture struct {
	Layers         int `json:"layers"`
	AttentionHeads int `json:"attention_heads"`
	KVHeads        int `json:"kv_heads"`
	HeadDim        int `json:"head_dim"`
}

// KVBytesPerToken assumes a 16-bit cache dtype, vLLM's default for 16-bit models.
func (a Architecture) KVBytesPerToken() float64 {
	return float64(2 * a.Layers * a.KVHeads * a.HeadDim * 2)
}

// ModelTemplate is a launchable model. ParamsActiveB equals ParamsB for dense
// models; Generate derives KVBytesPerToken from Architecture.
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
	// Gated sources need a licence-accepted account to download.
	License    string     `json:"license,omitempty"`
	Gated      bool       `json:"gated,omitempty"`
	Presets    []Preset   `json:"presets,omitempty"`
	Deployment Deployment `json:"deployment,omitzero"`
	Extensions Extensions `json:"extensions,omitempty"`
}

// kvBytesPerToken prefers the stored value: snapshots built elsewhere may omit Architecture.
func (t ModelTemplate) kvBytesPerToken() float64 {
	if t.KVBytesPerToken > 0 {
		return t.KVBytesPerToken
	}
	return t.Architecture.KVBytesPerToken()
}

// FitMode is whether a fit runs natively or emulated (weight-only, slower).
type FitMode string

const (
	FitNative   FitMode = "native"
	FitEmulated FitMode = "emulated"
)

// ModelFit states that a model at a quantization and KV cache dtype runs on
// GPUCount GPUs of a family across Nodes up to MaxContext. VRAMRequiredGB is
// rounded up to 0.01 GB; Profile is nil for whole GPUs.
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

// Performance is throughput at Context tokens per sequence; see Estimate. Source is
// "estimate" in the catalog.
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

// NodeType is a closed-vocabulary node: a cloud instance id ("aws/p5.48xlarge") or
// the fallback "other/<family>x<gpus>".
type NodeType struct {
	ID     string `json:"id"`
	Family string `json:"family"`
	GPUs   int    `json:"gpus"`
	Source string `json:"source,omitempty"`
}

// CatalogSnapshot is the immutable, versioned payload served to clients.
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

// VRAMRequired is weights plus KV cache at context; false if quant is unknown.
func VRAMRequired(t ModelTemplate, quant string, context int) (float64, bool) {
	for _, q := range t.Quantizations {
		if q.Name == quant {
			return q.WeightsGB + t.kvBytesPerToken()*float64(context)/1e9, true
		}
	}
	return 0, false
}

// Fit reports whether quant at context fits row f at f's KV cache dtype.
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

// Digest is the sha256 of every field except GeneratedAt, extensions included.
func (s CatalogSnapshot) Digest() string {
	s.GeneratedAt = time.Time{}
	h := sha256.New()
	_ = json.NewEncoder(h).Encode(s)
	return hex.EncodeToString(h.Sum(nil))
}

// Encode returns the published form: indented JSON with a trailing newline.
func (s CatalogSnapshot) Encode() ([]byte, error) {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
