// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

package huggingface

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	catalog "github.com/fishman/neocloud-catalog"
	"github.com/fishman/neocloud-catalog/service/internal/importers/huggingface/hubtest"
)

const qwenCommit = "cf98f3b3bbb457ad9e2bb7baf9a0125b6b88caa8"

func seedBackends(t *testing.T) []catalog.Backend {
	t.Helper()
	return committed(t).Backends
}

// One repository read at its default branch becomes a catalog model of its weights at the commit,
// with a standard preset per backend running their kind; Generate accepts it.
func TestRepositoryTemplate(t *testing.T) {
	h := hubtest.New(t, nil)
	r, err := testImporter(h.URL, "").Read(ctx, "Qwen/Qwen2.5-14B-Instruct", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if r.Commit != qwenCommit || r.Gated || r.License != "apache-2.0" || r.Task() != "chat" {
		t.Errorf("repository %s at %s gated %v license %q task %q", r.Repo, r.Commit, r.Gated, r.License, r.Task())
	}
	m, err := r.Template(seedBackends(t))
	if err != nil {
		t.Fatal(err)
	}
	q := m.Quantizations[0]
	presets := []string{}
	for _, p := range m.Presets {
		presets = append(presets, p.Name+" "+p.Backend+" "+p.BackendVersion)
	}
	if m.Slug != "qwen--qwen2.5-14b-instruct" || m.Name != "Qwen2.5-14B-Instruct" || m.Family != "qwen2" || m.Task != "chat" ||
		m.ParamsB != 14.8 || m.ParamsActiveB != 14.8 || m.ContextMax != 32768 || m.DefaultContext != 8192 ||
		m.Architecture != (catalog.Architecture{Layers: 48, AttentionHeads: 40, KVHeads: 8, HeadDim: 128}) || m.ReleaseDate != "2024-09-16" ||
		len(m.Quantizations) != 1 || q.Name != "bf16" || q.Kind != "float" || q.RequiresDType != "bf16" || q.WeightsGB != 29.5 ||
		len(q.Sources) != 1 || q.Sources[0].Revision != qwenCommit || len(q.Sources[0].Files) != 18 ||
		!slices.Equal(presets, []string{"vllm-bf16 vllm v0.31.0", "sglang-bf16 sglang v0.5.21"}) {
		b, _ := json.MarshalIndent(m, "", " ")
		t.Errorf("template %s\npresets %v", b, presets)
	}
	in := committed(t)
	if _, _, err := catalog.Generate(in.Families, in.NodeTypes, []catalog.ModelTemplate{m}, nil, in.Backends); err != nil {
		t.Errorf("generate: %v", err)
	}
}

// A repository the catalog cannot serve is refused saying why; detection still lists what it holds.
func TestRepositoryTemplateRefusals(t *testing.T) {
	h := hubtest.New(t, map[string]hubtest.Private{"acme/qwen": {Recorded: "Qwen/Qwen2.5-14B-Instruct", Token: "tenant-token"}})
	imp := testImporter(h.URL, "")
	for _, c := range []struct{ repo, rev, want string }{
		{"google/gemma-2-9b-it", "11c9b309abf73637e4b6f9a3fa1e92e615547819", "gated: token missing"},
		{"bartowski/Qwen2.5-14B-Instruct-GGUF", "", "GGUF repository"},
	} {
		r, err := imp.Read(ctx, c.repo, c.rev, "")
		if err != nil {
			t.Fatalf("%s: %v", c.repo, err)
		}
		if _, err := r.Template(seedBackends(t)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: template error %v, want %q", c.repo, err, c.want)
		}
	}
	r, err := imp.Read(ctx, "bartowski/Qwen2.5-14B-Instruct-GGUF", "", "")
	if err != nil {
		t.Fatal(err)
	}
	var sum int64
	qs := r.Quantizations()
	for _, q := range qs {
		if q.Kind != "gguf" || len(q.Files) != 1 {
			t.Errorf("GGUF quantization %+v", q)
		}
		sum += q.Bytes
	}
	if !slices.ContainsFunc(qs, func(q Quantization) bool { return q.Name == "Q4_K_M" }) || sum != r.WeightsBytes() {
		t.Errorf("GGUF quantizations %+v of %d bytes, want Q4_K_M among them and %d bytes", qs, sum, r.WeightsBytes())
	}
	for _, token := range []string{"", "wrong"} {
		if _, err := imp.Read(ctx, "acme/qwen", "", token); !errors.Is(err, ErrNotFound) {
			t.Errorf("private repository with token %q: %v, want not found", token, err)
		}
	}
	if r, err := imp.Read(ctx, "acme/qwen", "", "tenant-token"); err != nil || !r.Private || r.Commit != qwenCommit {
		t.Errorf("private repository with its token: %+v %v", r, err)
	}
	if _, err := imp.Read(ctx, "nobody/nothing", "", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing repository: %v, want not found", err)
	}
}

// The weights' quantization comes from config.json: float by dtype, fp8 natively or as
// compressed-tensors float8 groups, else the method, which no backend runs.
func TestWeightsQuantization(t *testing.T) {
	tree := []entry{{Type: "file", Path: "model.safetensors", Size: 10}, {Type: "file", Path: "pytorch_model.bin", Size: 10}}
	for _, c := range []struct{ config, name, kind string }{
		{`{"torch_dtype": "bfloat16"}`, "bf16", "float"},
		{`{"dtype": "float16"}`, "fp16", "float"},
		{`{"quantization_config": {"quant_method": "fp8"}}`, "fp8", "fp8"},
		{`{"quantization_config": {"quant_method": "compressed-tensors", "config_groups": {"group_0": {"weights": {"type": "float", "num_bits": 8}}}}}`, "fp8", "fp8"},
		{`{"quantization_config": {"quant_method": "compressed-tensors", "config_groups": {"group_0": {"weights": {"type": "int", "num_bits": 4}}}}}`, "compressed-tensors", "compressed-tensors"},
		{`{"quantization_config": {"quant_method": "gptq"}}`, "gptq", "gptq"},
	} {
		r := &Repository{tree: tree, weights: 10, config: &config{}}
		if err := json.Unmarshal([]byte(c.config), &r.extra); err != nil {
			t.Fatal(err)
		}
		q, ok := r.weightsQuantization()
		if !ok || q.Name != c.name || q.Kind != c.kind || !slices.Equal(q.Files, []string{"model.safetensors"}) {
			t.Errorf("%s: %+v, want %s %s of model.safetensors", c.config, q, c.name, c.kind)
		}
	}
}
