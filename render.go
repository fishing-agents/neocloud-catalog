package catalog

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Render is RenderRole for a single server: no disaggregation role, node 0.
func Render(m ModelTemplate, p Preset, bv BackendVersion, fit ModelFit, draft *DraftModel, vendor string) ([]string, error) {
	return RenderRole(m, p, bv, fit, draft, vendor, "", 0)
}

// RenderRole renders a preset into the arguments that follow its backend
// version's command, in a stable order: context, parallelism, KV cache dtype,
// prefix caching, parsers, chat template, speculative decoding, topology, extra
// args. Tensor parallel is the role's GPU count on a disaggregated side, else
// the preset's, else the fit's GPU count. vendor is the GPU vendor (or cpu) of
// the image the arguments are for. role is "prefill" or "decode" for one side
// of a disaggregated preset and empty otherwise; nodeRank is the node of a
// multi-node preset, whose arguments carry the placeholders {{master_addr}}
// and {{master_port}} for the caller to fill.
// A field the flag table cannot express is an error naming the backend version.
func RenderRole(m ModelTemplate, p Preset, bv BackendVersion, fit ModelFit, draft *DraftModel, vendor, role string, nodeRank int) ([]string, error) {
	args, err := render(p, bv.Flags, fit, draft, vendor, role, nodeRank)
	if err != nil {
		return nil, fmt.Errorf("%s/%s on %s %s: %w", m.Slug, p.Name, p.Backend, bv.Version, err)
	}
	return args, nil
}

func render(p Preset, f FlagTable, fit ModelFit, draft *DraftModel, vendor, role string, nodeRank int) ([]string, error) {
	a := argv{separate: f.SeparateValues}
	if p.Context > 0 {
		if f.Context == "" {
			return nil, errors.New("cannot express context")
		}
		a.set(f.Context, strconv.Itoa(p.Context))
	}

	pp := p.Topology.Pipeline
	if pp > 1 && p.TensorParallel == 0 {
		// The world size is tensor times pipeline parallel, so the fit's GPU
		// count does not say how to split it.
		return nil, errors.New("pipeline parallel needs an explicit tensor_parallel")
	}
	tp := cmp.Or(p.TensorParallel, fit.GPUCount)
	switch role {
	case "prefill":
		tp = p.Topology.Prefill
	case "decode":
		tp = p.Topology.Decode
	}
	// Without a tensor-parallel flag (llama.cpp) the engine splits layers over
	// the GPUs it is given, so only an explicit preset value is refused.
	switch {
	case tp <= 1:
	case f.TensorParallel != "":
		a.set(f.TensorParallel, strconv.Itoa(tp))
	case p.TensorParallel > 0:
		return nil, errors.New("cannot express tensor parallel")
	}
	if pp > 1 {
		if f.PipelineParallel == "" {
			return nil, errors.New("cannot express pipeline parallel")
		}
		a.set(f.PipelineParallel, strconv.Itoa(pp))
	}

	if d := p.KVCacheDType; d != "" && d != "auto" {
		v, ok := f.KVCacheDTypes[d]
		if !ok || f.KVCacheDType == "" {
			return nil, fmt.Errorf("has no kv cache dtype %q", d)
		}
		a.set(f.KVCacheDType, v)
	}

	if pc := p.PrefixCaching; pc != nil {
		flag, opposite := f.PrefixCachingOn, f.PrefixCachingOff
		if !*pc {
			flag, opposite = opposite, flag
		}
		// With only the opposite switch, the requested state is the default.
		switch {
		case flag != "":
			a.args = append(a.args, flag)
		case opposite == "":
			return nil, errors.New("cannot express prefix caching")
		}
	}

	if p.ToolParser != "" {
		name, ok := f.ToolParsers[p.ToolParser]
		if !ok {
			return nil, fmt.Errorf("has no tool parser %q", p.ToolParser)
		}
		a.args = append(a.args, f.ToolParserNeeds...)
		if f.ToolParser != "" {
			a.set(f.ToolParser, name)
		}
	}
	if p.ReasoningParser != "" {
		name, ok := f.ReasoningParsers[p.ReasoningParser]
		if !ok || f.ReasoningParser == "" {
			return nil, fmt.Errorf("has no reasoning parser %q", p.ReasoningParser)
		}
		a.set(f.ReasoningParser, name)
	}
	if p.ChatTemplate != "" {
		paths, ok := f.ChatTemplates[p.ChatTemplate]
		if !ok || f.ChatTemplate == "" {
			return nil, fmt.Errorf("has no chat template %q", p.ChatTemplate)
		}
		path, ok := paths[vendor]
		if !ok {
			return nil, fmt.Errorf("has no chat template %q for vendor %q", p.ChatTemplate, vendor)
		}
		a.set(f.ChatTemplate, path)
	}

	if s := p.Speculative; s != nil {
		if err := a.speculative(f, *s, draft); err != nil {
			return nil, err
		}
	}
	if err := a.topology(f, p.Topology, role, nodeRank); err != nil {
		return nil, err
	}
	return append(a.args, p.ExtraArgs...), nil
}

type argv struct {
	separate bool
	args     []string
}

func (a *argv) set(flag, value string) {
	if a.separate {
		a.args = append(a.args, flag, value)
	} else {
		a.args = append(a.args, flag+"="+value)
	}
}

// setJSON relies on encoding/json sorting map keys for a stable argv.
func (a *argv) setJSON(flag string, v map[string]any) {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // strings and ints always marshal
	}
	a.set(flag, string(b))
}

func (a *argv) speculative(f FlagTable, s Speculative, draft *DraftModel) error {
	if s.Tokens < 1 {
		return errors.New("speculative tokens must be positive")
	}
	flag, vllm := strings.CutPrefix(f.Speculative, "json:")
	if !vllm && f.Speculative != "sglang" {
		return errors.New("cannot express speculative decoding")
	}
	method, ok := f.SpeculativeMethods[s.Method]
	if !ok {
		return fmt.Errorf("has no speculative method %q", s.Method)
	}
	// Every mapped method loads draft weights; vLLM refuses to start without
	// them (config/speculative.py).
	if s.DraftModel == "" {
		return fmt.Errorf("speculative method %q needs a draft model", s.Method)
	}
	if draft == nil || draft.Slug != s.DraftModel {
		return fmt.Errorf("draft model %q not given", s.DraftModel)
	}
	src := draft.Source
	if vllm {
		cfg := map[string]any{"method": method, "model": src.Repo, "num_speculative_tokens": s.Tokens}
		if src.Revision != "" {
			cfg["revision"] = src.Revision
		}
		a.setJSON(flag, cfg)
		return nil
	}
	a.set("--speculative-algorithm", method)
	a.set("--speculative-draft-model-path", src.Repo)
	if src.Revision != "" {
		a.set("--speculative-draft-model-revision", src.Revision)
	}
	// A top-1 chain verifies its steps plus one token; SGLang forces that
	// draft-token count whenever topk is 1.
	a.set("--speculative-num-steps", strconv.Itoa(s.Tokens))
	a.set("--speculative-eagle-topk", "1")
	a.set("--speculative-num-draft-tokens", strconv.Itoa(s.Tokens+1))
	return nil
}

func (a *argv) topology(f FlagTable, t Topology, role string, nodeRank int) error {
	if role != "" && t.Kind != "disaggregated" {
		return fmt.Errorf("role %q needs a disaggregated preset", role)
	}
	if nodeRank != 0 && t.Kind != "multi-node" {
		return fmt.Errorf("node rank %d needs a multi-node preset", nodeRank)
	}
	switch t.Kind {
	case "", "single":
	case "disaggregated":
		if role != "prefill" && role != "decode" {
			return fmt.Errorf("disaggregated preset needs role prefill or decode, got %q", role)
		}
		transfer, ok := f.KVTransfers[t.Transfer]
		flag, vllm := strings.CutPrefix(f.Disaggregation, "json:")
		switch {
		case !vllm && f.Disaggregation != "sglang":
			return errors.New("cannot express disaggregation")
		case !ok:
			return fmt.Errorf("has no kv transfer %q", t.Transfer)
		case vllm:
			kvRole := "kv_producer"
			if role == "decode" {
				kvRole = "kv_consumer"
			}
			a.setJSON(flag, map[string]any{"kv_connector": transfer, "kv_role": kvRole})
		default:
			a.set("--disaggregation-mode", role)
			a.set("--disaggregation-transfer-backend", transfer)
		}
	case "multi-node":
		if nodeRank < 0 || nodeRank >= t.Nodes {
			return fmt.Errorf("node rank %d outside %d nodes", nodeRank, t.Nodes)
		}
		switch f.MultiNode {
		case "vllm":
			a.set("--nnodes", strconv.Itoa(t.Nodes))
			a.set("--node-rank", strconv.Itoa(nodeRank))
			a.set("--master-addr", "{{master_addr}}")
			a.set("--master-port", "{{master_port}}")
			if nodeRank > 0 {
				a.args = append(a.args, "--headless")
			}
		case "sglang":
			a.set("--nnodes", strconv.Itoa(t.Nodes))
			a.set("--node-rank", strconv.Itoa(nodeRank))
			a.set("--dist-init-addr", "{{master_addr}}:{{master_port}}")
		default:
			return errors.New("cannot express multi-node")
		}
	default:
		return fmt.Errorf("unknown topology %q", t.Kind)
	}
	return nil
}
