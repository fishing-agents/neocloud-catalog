// Package runrecord is the run-record contract: one serving configuration observed
// over one window, with its outcome. Records carry no tenant, host or request data:
// unknown fields are refused and launch args are limited to an allowlist.
package runrecord

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"
)

// SchemaID is Record.Schema for this contract version.
const SchemaID = "run-record/v1"

// JSONSchema is the JSON Schema (draft 2020-12) of a Record.
//
//go:embed schema/run-record.v1.json
var JSONSchema []byte

// Record is one configuration observed over one window.
type Record struct {
	Schema          string    `json:"schema"`
	RecordID        string    `json:"record_id"`
	InstallID       string    `json:"install_id"`
	Platform        string    `json:"platform"`
	PlatformVersion string    `json:"platform_version,omitempty"`
	ObservedFrom    time.Time `json:"observed_from"`
	ObservedTo      time.Time `json:"observed_to"`
	Model           Model     `json:"model"`
	Config          Config    `json:"config"`
	Outcome         Outcome   `json:"outcome"`
	Workload        Workload  `json:"workload"`
}

// Model identifies the weights; Catalog is set only when the deployment matched a preset.
type Model struct {
	Catalog      *CatalogRef `json:"catalog,omitempty"`
	Source       string      `json:"source"`
	Repo         string      `json:"repo"`
	Revision     string      `json:"revision,omitempty"`
	Quantization string      `json:"quantization"`
	ParamsB      float64     `json:"params_b,omitempty"`
	WeightsGB    float64     `json:"weights_gb,omitempty"`
}

// CatalogRef names the catalog model and preset a deployment matched.
type CatalogRef struct {
	Slug    string `json:"slug"`
	Preset  string `json:"preset,omitempty"`
	Version string `json:"version"`
}

// Config is how the model was served and on what hardware.
type Config struct {
	Backend          string   `json:"backend"`
	BackendVersion   string   `json:"backend_version,omitempty"`
	ImageDigest      string   `json:"image_digest,omitempty"`
	Args             []string `json:"args,omitempty"`
	TensorParallel   int      `json:"tensor_parallel"`
	PipelineParallel int      `json:"pipeline_parallel,omitempty"`
	MaxContext       int      `json:"max_context"`
	GPUModel         string   `json:"gpu_model"`
	GPUCount         int      `json:"gpu_count"`
	GPUMemoryGB      float64  `json:"gpu_memory_gb"`
	Interconnect     string   `json:"interconnect,omitempty"`
	Nodes            int      `json:"nodes"`
}

// Outcome is what happened. Measurements are nil when the platform does not expose them.
type Outcome struct {
	Status             string   `json:"status"`
	Failure            string   `json:"failure,omitempty"`
	Reason             string   `json:"reason,omitempty"`
	VRAMUsedGB         *float64 `json:"vram_used_gb,omitempty"`
	OutputTokensPerSec *float64 `json:"output_tokens_per_sec,omitempty"`
	TotalTokensPerSec  *float64 `json:"total_tokens_per_sec,omitempty"`
	TTFTP50Ms          *float64 `json:"ttft_p50_ms,omitempty"`
	TTFTP95Ms          *float64 `json:"ttft_p95_ms,omitempty"`
	ITLP50Ms           *float64 `json:"itl_p50_ms,omitempty"`
	ITLP95Ms           *float64 `json:"itl_p95_ms,omitempty"`
	ConcurrencyMean    *float64 `json:"concurrency_mean,omitempty"`
	ConcurrencyMax     *float64 `json:"concurrency_max,omitempty"`
	RequestsOK         *int64   `json:"requests_ok,omitempty"`
	RequestsFailed     *int64   `json:"requests_failed,omitempty"`
}

// Workload is a benchmark level, or a live traffic window (mean token lengths).
type Workload struct {
	Source       string `json:"source"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
	Concurrency  int    `json:"concurrency,omitempty"`
	Requests     int64  `json:"requests,omitempty"`
}

const (
	StatusRan    = "ran"
	StatusFailed = "failed"

	SourceBenchmarkStandard = "benchmark-standard"
	SourceBenchmarkOperator = "benchmark-operator"
	SourceLive              = "live"
)

var (
	modelSources = []string{"huggingface", "modelscope"}
	backends     = []string{"vllm", "sglang"}
	failures     = []string{"oom", "crash", "unschedulable", "timeout", "other"}
	sources      = []string{SourceBenchmarkStandard, SourceBenchmarkOperator, SourceLive}
)

// ValueArgs and SwitchArgs are the launch flags a record may carry; anything else
// (paths, served names, API keys, hosts) is dropped or refused. Switches never take
// a value, so a following positional token is not mistaken for one.
var (
	ValueArgs = []string{
		"--tensor-parallel-size", "--tp-size", "--pipeline-parallel-size", "--pp-size",
		"--data-parallel-size", "--dp-size", "--max-model-len", "--context-length",
		"--max-num-seqs", "--max-num-batched-tokens", "--max-running-requests", "--chunked-prefill-size",
		"--gpu-memory-utilization", "--mem-fraction-static", "--dtype", "--quantization", "--kv-cache-dtype",
		"--speculative-algorithm", "--speculative-num-steps", "--num-speculative-tokens",
	}
	SwitchArgs = []string{
		"--enable-expert-parallel", "--enable-ep-moe", "--enable-chunked-prefill",
		"--enable-prefix-caching", "--no-enable-prefix-caching", "--disable-radix-cache",
		"--enforce-eager", "--disable-cuda-graph",
	}
)

var (
	uuidRE  = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	nameRE  = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	repoRE  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)
	valueRE = regexp.MustCompile(`^[A-Za-z0-9._:+-]{1,64}$`)
)

const maxReason = 200

// FilterArgs keeps allowlisted flags, normalizing values to "--flag=value". A value
// that is not plain (a path, JSON) is dropped with its flag.
func FilterArgs(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		flag, value, joined := strings.Cut(args[i], "=")
		switch {
		case slices.Contains(SwitchArgs, flag):
			if !joined {
				out = append(out, flag)
			}
		case slices.Contains(ValueArgs, flag):
			if !joined && i+1 < len(args) {
				value, joined = args[i+1], true
				i++
			}
			if joined && valueRE.MatchString(value) {
				out = append(out, flag+"="+value)
			}
		}
	}
	return out
}

// Decode reads and validates one record, refusing unknown fields.
func Decode(r io.Reader) (Record, error) {
	var rec Record
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rec); err != nil {
		return rec, fmt.Errorf("run record: %w", err)
	}
	return rec, rec.Validate()
}

// Validate checks the record against the contract.
func (r Record) Validate() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	if r.Schema != SchemaID {
		bad("schema %q, want %q", r.Schema, SchemaID)
	}
	if !uuidRE.MatchString(r.RecordID) {
		bad("record_id %q is not a lowercase UUID", r.RecordID)
	}
	if !uuidRE.MatchString(r.InstallID) {
		bad("install_id %q is not a lowercase UUID", r.InstallID)
	}
	if !nameRE.MatchString(r.Platform) {
		bad("platform %q is not a lowercase adapter name", r.Platform)
	}
	if r.ObservedFrom.IsZero() || r.ObservedTo.Before(r.ObservedFrom) {
		bad("observed window %s..%s is empty or reversed", r.ObservedFrom, r.ObservedTo)
	}

	m := r.Model
	if !slices.Contains(modelSources, m.Source) {
		bad("model.source %q, want one of %v", m.Source, modelSources)
	}
	if !repoRE.MatchString(m.Repo) {
		bad("model.repo %q is not an org/name repository id", m.Repo)
	}
	if m.Quantization == "" {
		bad("model.quantization is required")
	}
	if c := m.Catalog; c != nil && (c.Slug == "" || c.Version == "") {
		bad("model.catalog needs slug and version")
	}

	c := r.Config
	if !slices.Contains(backends, c.Backend) {
		bad("config.backend %q, want one of %v", c.Backend, backends)
	}
	for _, a := range c.Args {
		flag, value, joined := strings.Cut(a, "=")
		ok := slices.Contains(SwitchArgs, flag) && !joined || slices.Contains(ValueArgs, flag) && joined && valueRE.MatchString(value)
		if !ok {
			bad("config.args %q is not an allowlisted flag with a plain value", a)
		}
	}
	if c.TensorParallel < 1 || c.MaxContext < 1 || c.GPUCount < 1 || c.GPUMemoryGB <= 0 || c.Nodes < 1 || c.GPUModel == "" {
		bad("config needs tensor_parallel, max_context, gpu_model, gpu_count, gpu_memory_gb and nodes")
	}

	o := r.Outcome
	switch o.Status {
	case StatusRan:
		if o.Failure != "" {
			bad("outcome.failure set on a run that ran")
		}
	case StatusFailed:
		if !slices.Contains(failures, o.Failure) {
			bad("outcome.failure %q, want one of %v", o.Failure, failures)
		}
	default:
		bad("outcome.status %q, want ran or failed", o.Status)
	}
	if len(o.Reason) > maxReason {
		bad("outcome.reason is %d bytes, at most %d", len(o.Reason), maxReason)
	}

	w := r.Workload
	if !slices.Contains(sources, w.Source) {
		bad("workload.source %q, want one of %v", w.Source, sources)
	}
	if w.Source != SourceLive && (w.InputTokens < 1 || w.OutputTokens < 1 || w.Concurrency < 1) {
		bad("a benchmark workload needs input_tokens, output_tokens and concurrency")
	}
	return errors.Join(errs...)
}
