package catalog

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
)

const llama31ToolTemplate = "/vllm-workspace/examples/tool_chat_template_llama3.1_json.jinja"

var (
	vllmEagle3 = &DraftModel{Slug: "eagle3-llama-3.1-8b-redhat", Method: "eagle3",
		Source: ModelSource{Type: SourceHuggingFace, Repo: "RedHatAI/Llama-3.1-8B-Instruct-speculator.eagle3", Revision: "f4fa34a8f803a0ba75d048d6b3dbc1ad5149e9ac"}}
	sglangEagle3 = &DraftModel{Slug: "eagle3-llama-3.1-8b-sglang", Method: "eagle3",
		Source: ModelSource{Type: SourceHuggingFace, Repo: "lmsys/sglang-EAGLE3-LLaMA3.1-Instruct-8B", Revision: "28a53ce8911434c031d7c78392abb26d898ec293"}}
	localEagle3 = &DraftModel{Slug: "eagle3-local", Method: "eagle3", Source: ModelSource{Type: SourceLocal, Repo: "/models/eagle3"}}
)

// curatedBackends returns the pinned version of each backend from
// data/backends.json, so the tests pin the curated flag tables rather than
// fixtures.
func curatedBackends(t *testing.T) map[string]BackendVersion {
	t.Helper()
	b, err := os.ReadFile("data/backends.json")
	if err != nil {
		t.Fatal(err)
	}
	var backends []Backend
	if err := json.Unmarshal(b, &backends); err != nil {
		t.Fatal(err)
	}
	pinned := map[string]string{"vllm": "v0.31.0", "sglang": "v0.5.21", "llamacpp": "v0.6.0"}
	out := map[string]BackendVersion{}
	for _, be := range backends {
		for _, v := range be.Versions {
			if v.Version == pinned[be.Name] {
				out[be.Name] = v
			}
		}
	}
	for name, version := range pinned {
		if _, ok := out[name]; !ok {
			t.Fatalf("data/backends.json has no %s %s", name, version)
		}
	}
	return out
}

func TestRender(t *testing.T) {
	bvs := curatedBackends(t)
	on, off := true, false
	pd := Topology{Kind: "disaggregated", Prefill: 2, Decode: 4, Transfer: "nixl"}
	twoNodes := Topology{Kind: "multi-node", Nodes: 2}
	cases := []struct {
		name    string
		backend string
		preset  Preset
		gpus    int
		draft   *DraftModel
		role    string
		rank    int
		want    []string
	}{
		{name: "vllm llama3 tools", backend: "vllm", gpus: 1,
			preset: Preset{Context: 8192, ToolParser: "llama3", ChatTemplate: "llama3.1-tools"},
			want:   []string{"--max-model-len=8192", "--enable-auto-tool-choice", "--tool-call-parser=llama3_json", "--chat-template=" + llama31ToolTemplate}},
		{name: "sglang llama3 tools", backend: "sglang", gpus: 1,
			preset: Preset{Context: 8192, ToolParser: "llama3"},
			want:   []string{"--context-length=8192", "--tool-call-parser=llama3"}},
		{name: "vllm qwen25 tools", backend: "vllm", gpus: 1,
			preset: Preset{ToolParser: "qwen25"},
			want:   []string{"--enable-auto-tool-choice", "--tool-call-parser=hermes"}},
		{name: "sglang tp from fit", backend: "sglang", gpus: 2,
			preset: Preset{Context: 8192},
			want:   []string{"--context-length=8192", "--tp-size=2"}},
		{name: "preset tp overrides fit", backend: "vllm", gpus: 2,
			preset: Preset{TensorParallel: 4},
			want:   []string{"--tensor-parallel-size=4"}},
		{name: "vllm kv fp8", backend: "vllm", gpus: 1, preset: Preset{KVCacheDType: "fp8"}, want: []string{"--kv-cache-dtype=fp8"}},
		{name: "sglang kv fp8", backend: "sglang", gpus: 1, preset: Preset{KVCacheDType: "fp8"}, want: []string{"--kv-cache-dtype=fp8_e4m3"}},
		{name: "kv auto is the default", backend: "sglang", gpus: 1, preset: Preset{KVCacheDType: "auto"}},
		{name: "prefix caching nil", backend: "vllm", gpus: 1, preset: Preset{}},
		{name: "vllm prefix caching on", backend: "vllm", gpus: 1, preset: Preset{PrefixCaching: &on}, want: []string{"--enable-prefix-caching"}},
		{name: "vllm prefix caching off", backend: "vllm", gpus: 1, preset: Preset{PrefixCaching: &off}, want: []string{"--no-enable-prefix-caching"}},
		{name: "sglang prefix caching on is the default", backend: "sglang", gpus: 1, preset: Preset{PrefixCaching: &on}},
		{name: "sglang prefix caching off", backend: "sglang", gpus: 1, preset: Preset{PrefixCaching: &off}, want: []string{"--disable-radix-cache"}},
		{name: "vllm eagle3", backend: "vllm", gpus: 1, draft: vllmEagle3,
			preset: Preset{Speculative: &Speculative{Method: "eagle3", DraftModel: vllmEagle3.Slug, Tokens: 3}},
			want: []string{`--speculative-config={"method":"eagle3","model":"RedHatAI/Llama-3.1-8B-Instruct-speculator.eagle3",` +
				`"num_speculative_tokens":3,"revision":"f4fa34a8f803a0ba75d048d6b3dbc1ad5149e9ac"}`}},
		{name: "unpinned draft has no revision", backend: "vllm", gpus: 1, draft: localEagle3,
			preset: Preset{Speculative: &Speculative{Method: "eagle3", DraftModel: localEagle3.Slug, Tokens: 3}},
			want:   []string{`--speculative-config={"method":"eagle3","model":"/models/eagle3","num_speculative_tokens":3}`}},
		{name: "sglang eagle3", backend: "sglang", gpus: 1, draft: sglangEagle3,
			preset: Preset{Speculative: &Speculative{Method: "eagle3", DraftModel: sglangEagle3.Slug, Tokens: 3}},
			want: []string{"--speculative-algorithm=EAGLE3", "--speculative-draft-model-path=lmsys/sglang-EAGLE3-LLaMA3.1-Instruct-8B",
				"--speculative-draft-model-revision=28a53ce8911434c031d7c78392abb26d898ec293",
				"--speculative-num-steps=3", "--speculative-eagle-topk=1", "--speculative-num-draft-tokens=4"}},
		{name: "sglang disaggregated prefill", backend: "sglang", gpus: 2, role: "prefill",
			preset: Preset{Context: 8192, Topology: pd},
			want:   []string{"--context-length=8192", "--tp-size=2", "--disaggregation-mode=prefill", "--disaggregation-transfer-backend=nixl"}},
		{name: "vllm prefill runs on the prefill gpus", backend: "vllm", gpus: 1, role: "prefill", preset: Preset{TensorParallel: 8, Topology: pd},
			want: []string{"--tensor-parallel-size=2", `--kv-transfer-config={"kv_connector":"NixlConnector","kv_role":"kv_producer"}`}},
		{name: "vllm decode runs on the decode gpus", backend: "vllm", gpus: 1, role: "decode", preset: Preset{TensorParallel: 8, Topology: pd},
			want: []string{"--tensor-parallel-size=4", `--kv-transfer-config={"kv_connector":"NixlConnector","kv_role":"kv_consumer"}`}},
		{name: "vllm multi-node head", backend: "vllm", gpus: 16, preset: Preset{Topology: twoNodes},
			want: []string{"--tensor-parallel-size=16", "--nnodes=2", "--node-rank=0", "--master-addr={{master_addr}}", "--master-port={{master_port}}"}},
		{name: "vllm multi-node worker", backend: "vllm", gpus: 16, rank: 1, preset: Preset{Topology: twoNodes},
			want: []string{"--tensor-parallel-size=16", "--nnodes=2", "--node-rank=1", "--master-addr={{master_addr}}", "--master-port={{master_port}}", "--headless"}},
		{name: "vllm pipeline", backend: "vllm", gpus: 16, preset: Preset{TensorParallel: 8, Topology: Topology{Kind: "multi-node", Nodes: 2, Pipeline: 2}},
			want: []string{"--tensor-parallel-size=8", "--pipeline-parallel-size=2", "--nnodes=2", "--node-rank=0", "--master-addr={{master_addr}}", "--master-port={{master_port}}"}},
		{name: "sglang multi-node worker", backend: "sglang", gpus: 16, rank: 1, preset: Preset{Topology: twoNodes},
			want: []string{"--tp-size=16", "--nnodes=2", "--node-rank=1", "--dist-init-addr={{master_addr}}:{{master_port}}"}},
		{name: "llamacpp separate values", backend: "llamacpp", gpus: 1,
			preset: Preset{Context: 8192, ToolParser: "qwen25"},
			want:   []string{"-c", "8192", "--jinja"}},
		{name: "llamacpp spreads layers over the fit's gpus", backend: "llamacpp", gpus: 2, preset: Preset{Context: 8192},
			want: []string{"-c", "8192"}},
		{name: "full order", backend: "vllm", gpus: 2, draft: vllmEagle3,
			preset: Preset{Context: 8192, KVCacheDType: "fp8", PrefixCaching: &off, ToolParser: "llama3", ChatTemplate: "llama3.1-tools",
				Speculative: &Speculative{Method: "eagle3", DraftModel: vllmEagle3.Slug, Tokens: 3}, ExtraArgs: []string{"--max-num-seqs", "64"}},
			want: []string{"--max-model-len=8192", "--tensor-parallel-size=2", "--kv-cache-dtype=fp8", "--no-enable-prefix-caching",
				"--enable-auto-tool-choice", "--tool-call-parser=llama3_json", "--chat-template=" + llama31ToolTemplate,
				`--speculative-config={"method":"eagle3","model":"RedHatAI/Llama-3.1-8B-Instruct-speculator.eagle3",` +
					`"num_speculative_tokens":3,"revision":"f4fa34a8f803a0ba75d048d6b3dbc1ad5149e9ac"}`,
				"--max-num-seqs", "64"}},
	}
	for _, c := range cases {
		bv, ok := bvs[c.backend]
		if !ok {
			t.Fatalf("no curated backend %s", c.backend)
		}
		c.preset.Name, c.preset.Backend, c.preset.BackendVersion = "p", c.backend, bv.Version
		m, fit := ModelTemplate{Slug: "m"}, ModelFit{GPUCount: c.gpus}
		var got []string
		var err error
		if c.role == "" && c.rank == 0 {
			got, err = Render(m, c.preset, bv, fit, c.draft)
		} else {
			got, err = RenderRole(m, c.preset, bv, fit, c.draft, c.role, c.rank)
		}
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("%s: got %q, %v\nwant %q", c.name, got, err, c.want)
		}
	}
}

// Every refusal names the preset and the backend version, so a generation
// error points at the flag table that cannot express the preset.
func TestRenderRefusesWhatTheBackendCannotExpress(t *testing.T) {
	bvs := curatedBackends(t)
	on := true
	pd := Topology{Kind: "disaggregated", Prefill: 1, Decode: 1, Transfer: "nixl"}
	cases := []struct {
		backend string
		preset  Preset
		draft   *DraftModel
		role    string
		rank    int
		want    string
	}{
		{backend: "vllm", preset: Preset{ToolParser: "gemma2"}, want: `has no tool parser "gemma2"`},
		{backend: "sglang", preset: Preset{ReasoningParser: "qwen3"}, want: `has no reasoning parser "qwen3"`},
		{backend: "sglang", preset: Preset{ChatTemplate: "llama3.1-tools"}, want: `has no chat template "llama3.1-tools"`},
		{backend: "llamacpp", preset: Preset{KVCacheDType: "fp8"}, want: `has no kv cache dtype "fp8"`},
		{backend: "llamacpp", preset: Preset{PrefixCaching: &on}, want: "cannot express prefix caching"},
		{backend: "llamacpp", preset: Preset{TensorParallel: 2}, want: "cannot express tensor parallel"},
		{backend: "vllm", preset: Preset{Topology: Topology{Kind: "multi-node", Nodes: 2, Pipeline: 2}}, want: "pipeline parallel needs an explicit tensor_parallel"},
		{backend: "llamacpp", preset: Preset{Speculative: &Speculative{Method: "eagle3", Tokens: 3}}, want: "cannot express speculative decoding"},
		{backend: "vllm", preset: Preset{Speculative: &Speculative{Method: "ngram", Tokens: 3}}, want: `has no speculative method "ngram"`},
		{backend: "vllm", draft: vllmEagle3, preset: Preset{Speculative: &Speculative{Method: "eagle3", Tokens: 3}},
			want: `speculative method "eagle3" needs a draft model`},
		{backend: "vllm", preset: Preset{Speculative: &Speculative{Method: "eagle3", DraftModel: vllmEagle3.Slug, Tokens: 3}},
			want: `draft model "eagle3-llama-3.1-8b-redhat" not given`},
		{backend: "sglang", draft: vllmEagle3, preset: Preset{Speculative: &Speculative{Method: "eagle3", DraftModel: sglangEagle3.Slug, Tokens: 3}},
			want: `draft model "eagle3-llama-3.1-8b-sglang" not given`},
		{backend: "sglang", draft: sglangEagle3, preset: Preset{Speculative: &Speculative{Method: "eagle3", DraftModel: sglangEagle3.Slug}},
			want: "speculative tokens must be positive"},
		{backend: "sglang", preset: Preset{Topology: pd}, want: `needs role prefill or decode, got ""`},
		{backend: "sglang", role: "prefill", preset: Preset{}, want: `role "prefill" needs a disaggregated preset`},
		{backend: "vllm", role: "decode", preset: Preset{Topology: Topology{Kind: "disaggregated", Prefill: 1, Decode: 1, Transfer: "mooncake"}},
			want: `has no kv transfer "mooncake"`},
		{backend: "llamacpp", role: "decode", preset: Preset{Topology: pd}, want: "cannot express disaggregation"},
		{backend: "vllm", rank: 2, preset: Preset{Topology: Topology{Kind: "multi-node", Nodes: 2}}, want: "node rank 2 outside 2 nodes"},
		{backend: "sglang", rank: 1, preset: Preset{}, want: "node rank 1 needs a multi-node preset"},
		{backend: "llamacpp", preset: Preset{Topology: Topology{Kind: "multi-node", Nodes: 2}}, want: "cannot express multi-node"},
		{backend: "vllm", preset: Preset{Topology: Topology{Kind: "ring"}}, want: `unknown topology "ring"`},
	}
	for _, c := range cases {
		bv := bvs[c.backend]
		c.preset.Name, c.preset.Backend, c.preset.BackendVersion = "bad-preset", c.backend, bv.Version
		got, err := RenderRole(ModelTemplate{Slug: "llama"}, c.preset, bv, ModelFit{GPUCount: 1}, c.draft, c.role, c.rank)
		version := c.backend + " " + bv.Version
		if err == nil || !strings.Contains(err.Error(), "llama/bad-preset") || !strings.Contains(err.Error(), version) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("got %q, %v; want an error naming llama/bad-preset and %s: %s", got, err, version, c.want)
		}
	}
}
