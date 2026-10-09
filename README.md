# neocloud-catalog

A public, CNCF-donation-ready model-template catalog: curated model templates,
the backends that serve them, structured deployable presets, the hardware facts
of GPU device families, a closed vocabulary of node types, and a fit matrix of
which families run each model, at which quantization and KV cache dtype, on how
many GPUs and nodes, with what context, VRAM and estimated throughput, plus
per-model deployment guidance. The catalog is a headless data artifact, not a
service - it ships as a pure Go module whose whole value is the versioned,
digest-pinned snapshot it embeds (schema 4). neocloud ingests that snapshot; it
does not curate models itself.

## What is here

- `catalog.go` - the pure catalog core. Stdlib only: no GORM, tenancy, or HTTP.
  Types: `Family` (vendor, architecture, compute capability or gfx target,
  native dtypes, memory, bandwidth, interconnect, source URLs), `NodeType`,
  `ModelTemplate` (metadata, weights and sources per quantization with its kind
  and required dtype, architecture, derived KV bytes/token, context limits,
  license, gated, presets, deployment guidance), `ModelSource` (huggingface,
  modelscope, oci or local; pinned revision; optional `files`), `Preset`,
  `DraftModel`, `Backend` and `BackendVersion` (image per vendor, command
  template, health path, quantization kinds, flag table), `ModelFit` (model x
  family x quantization x KV cache dtype -> native or emulated, GPU count,
  nodes, max context, VRAM, estimate), `CatalogSnapshot`, `VRAMRequired`
  (weights + KV for any context), `Fit`, `Digest` (sha256 of the snapshot
  content, the pin clients use).
- `artifact.go` - the published OCI artifact contract (`ArtifactType`,
  `OverlayArtifactType`, `VersionAnnotation`) and `ParseVersion`, the order
  clients refresh by.
- `extension.go` - the extension API (`Extension`, `RegisterExtension`,
  `RunExtensions`) and the overlay `Merge`; see Extensions and overlays.
- `nodetype.go` - `NodeTypeVocabulary` and `FallbackNodeType`; see Node types.
- `render.go` - `Render`/`RenderRole` turn a structured preset into the engine
  arguments through its backend version's flag table (multi-node arguments carry
  `{{master_addr}}` and `{{master_port}}` for the caller to fill).
- `data/` - the curated sources. `models.json` holds each model's metadata
  (description, task, capabilities, active parameters, release date, model
  card, aliases), published architecture (from its config), checkpoint kind,
  size and sources per quantization, license, gated flag and presets;
  `draft_models.json` the speculative-decoding drafts with their targets and
  the backends that load them; `backends.json` the inference servers (vLLM,
  SGLang, llama.cpp) with a pinned image per vendor, command templates
  (`{{model_path}}`, `{{model}}`, `{{port}}`), the quantization kinds they run
  and their flag tables; `families.json` the per-family hardware facts
  (generated, see Regenerate) and `families.overlay.json` their curated part;
  `node_types.json` the cloud instances of the node-type vocabulary.
- `generate.go` - the outsourced generation. `Generate` validates the input
  (`CatalogSnapshot.Validate`: every source has a known type and repo, and
  huggingface, modelscope and oci sources a pinned revision; drafts are pinned
  huggingface or modelscope repos; a model's `task` is one of `chat`,
  `completion`, `embedding`, `rerank`, `vision-chat` and its `capabilities`
  among `tools`, `reasoning`, `vision`, `long-context`, `json-schema`; every
  preset has a `purpose` (see Presets) and names a quantization of its model
  whose kind its backend version runs - natively in the quantization's
  `requires_dtype`, when set - an existing backend version and a draft for its
  backend, model and method that fits next to the model, stays within the
  model's context, spans at least two nodes when multi-node, and renders on its
  backend version; preset names are unique per model), derives the fit matrix
  and deployment guidance, then runs the registered extensions.
  KV bytes/token = 2 x layers x KV heads x head dim x 2 bytes (half for an fp8
  KV cache). A fit exists for a model, quantization, KV cache dtype and family
  only where some preset with them runs there: its backend version has an image
  for the family's vendor, the family meets its minimum compute capability
  (NVIDIA only), and the quantization kind runs `native` (its dtype is native to
  the family) or `emulated` (NVIDIA from a compute capability). The fit is the
  smallest layout such a preset needs - tensor-parallel GPU counts (1/2/4/8 per
  node, dividing the attention heads) across a multi-node preset's nodes, each
  side of a disaggregated one - whose budget (90% of GPU memory, a conservative
  budget below vLLM's 0.92 default) holds the weights plus the preset's
  context; max context is what the rest holds, capped at the model's ceiling. A
  preset whose quantization fits families its backend runs on none of, or an
  unparsable NVIDIA compute capability, fails generation. Each model's
  `deployment` names, among its float and fp8 fits serving one sequence at 20
  tokens/s, the minimum (least GPU memory) and optimal (highest aggregate per
  GPU at 20 tokens/s per sequence) with example node types; models without a
  minimum and presets whose quantization fits no family are reported.
- `estimate.go` - `Estimate`, the per-fit performance estimate: a
  memory-bandwidth roofline where each decode step reads the active weights
  once and every concurrent sequence's KV cache; concurrency is the most
  sequences memory holds that each still decode 20 tokens/s. Its constants are
  documented in one place: `BandwidthEfficiency` 0.8 of peak bandwidth,
  tensor-parallel efficiency 0.85 (2 GPUs) and 0.75 (4 or more),
  `EstimateMemoryUtilization` 0.92 of memory for weights and KV, and
  `DecentDecodeTokensPerSec` 20.
- `snapshots/v4/catalog.json` - the committed, generated snapshot the module
  embeds (schema 4.0.0). `SnapshotV4()` returns it.
- `cmd/catalog-generate` - the pipeline CLI: reads `data/` and writes the
  snapshot.
- `cmd/families-import` - writes `data/families.json` from Epoch AI's ML
  hardware data (vendor, memory, bandwidth, dense tensor TFLOPS, NVLink-class
  bandwidth) and `data/families.overlay.json` (the SKU each family name stands
  for, its Epoch row, architecture, compute capability or gfx target, native
  dtypes, interconnect, and Epoch gaps or corrections), every value with its
  source URL.
- `runrecord/` - the run-record contract: one serving configuration observed
  over one window with its outcome (Go types, `schema/run-record.v1.json`,
  strict `Decode`, `Validate`, the launch-flag allowlist `FilterArgs`).
- `adapters/` - a nested module (own `go.mod`, so the core stays stdlib only):
  the adapter contract and registry, one adapter per serving platform
  (`gpustack/`, `airunway/`), and `cmd/catalog-export`.
- `.github/workflows/release.yml` - publishes and signs the snapshot and the
  platform exports on a tag.

## Snapshot contract

The snapshot is the sellable/embeddable artifact. `SchemaVersion` is the wire
format (bump on a breaking shape change); `Version` changes when the content
changes; `Digest` is the content hash clients pin and the ETag for conditional
gets. The `/v1/model-catalog` endpoint in neocloud serves the same shape and
semantics.

Upstream refresh: the snapshot is published as a single-layer OCI artifact
(`application/vnd.neocloud.catalog.v4+json`, version in the
`org.opencontainers.image.version` manifest annotation), signed with cosign. A
client resolves the reference to a manifest digest, verifies the Sigstore bundle
attached as an OCI referrer against that digest and the publisher's identity,
refuses a version not newer than the one in use, pulls the layer, and accepts
the snapshot only if its version matches the annotation and its schema major is
4. Anything else keeps the last good snapshot.

Schema 4 replaced schema 3 in one cutover; schema 3 is not published any more.
The moving `v3` tag keeps serving the last schema 3 release, which a schema 4
client refuses while it keeps its last good catalog: repoint `catalog.ref` to
`:v4`.

### Presets

A preset states intent in engine-neutral fields: `purpose` (`standard`,
`throughput`, `latency`, `long-context`), `context`, `tensor_parallel` (0 = from
the fit), `kv_cache_dtype` (`auto`, `fp8`), `prefix_caching`, `tool_parser`,
`reasoning_parser`, `chat_template`, `speculative`, `topology`, `env` (no
secrets), `min_backend_version`, `extra_args` (verbatim escape hatch) and
`vendors`. Each backend version's flag table maps the fields and the neutral
parser, template, KV dtype, speculative method and KV transfer names to its own
flags; a chat template maps to a path per image vendor, since each vendor's
image lays out its files differently. `Render` produces the argv for one
vendor's image, so adapters do not translate by hand. Validate renders every
preset once per vendor it runs on.

- Speculative decoding: `speculative` names the method, the token count and,
  when the method needs one, a draft from `draft_models`. Only `eagle` and
  `eagle3` are mapped today, the methods verified against the pinned engine
  sources; other methods (`mtp`, `ngram`, `draft-model`) fail generation until
  a flag table maps them (vLLM's `ngram` needs prompt lookup bounds a preset
  cannot state). Validate checks that the draft's weights fit next to the
  model's on the GPUs of every fit the preset deploys on, but a fit's
  `vram_required_gb` does not include them.
- Topology: `single` (default), `multi-node` (`nodes`, `pipeline` stages per
  node) or `disaggregated` (`prefill_gpus`, `decode_gpus`, KV `transfer`
  `nixl` or `mooncake`). Fits carry the node count.
- llama.cpp: the `llamacpp` backend runs `gguf` quantizations (e.g. `Q4_K_M`)
  whose source names the file in `files`, from pinned llama-server images for
  nvidia, amd and cpu.

### Hardware facts

`families` carry vendor, architecture, compute capability (NVIDIA) or gfx
target (AMD), native dtypes, memory, memory bandwidth and interconnect. Memory,
bandwidth and tensor throughput come from Epoch AI's ML hardware data (CC BY
4.0, see `NOTICE`); compute capability from developer.nvidia.com/cuda/gpus;
gfx target and native dtypes from rocm.docs.amd.com. Every value carries its
source URL. A fit is `native` when the quantization's dtype is native to the
family and `emulated` when the backend version runs it weight-only from a
compute capability (FP8 on A100 and A10G).

### Node types

`node_types` is a closed vocabulary of the systems models run on: cloud
instances by `<provider>/<instance type>` (e.g. `aws/p5.48xlarge`) with only
their GPU family and count, plus generated fallbacks `other/<family>x<gpus>`
for 1, 2, 4 and 8 GPUs of every family. A system that is no listed instance is
reported as its fallback, never as free text. Deployment guidance names
example node types.

### Deployment guidance

Each model's `deployment` holds a `minimum` (the least GPU memory serving one
sequence at `DecentDecodeTokensPerSec`, 20 tokens/s, at the preset's context)
and an `optimal` setup (the highest aggregate output tokens/s per GPU while each
sequence keeps 20 tokens/s): family, GPUs, nodes, quantization, preset,
context, example node types and the fits it derives from. Only float and fp8
quantizations drive guidance; every fit carries its own `performance` estimate.
Recommendations are estimates (`source` and `confidence` `estimate`) until
observed aggregates exist.

### Extensions and overlays

`Family`, `ModelTemplate`, `Preset`, `ModelFit` and `BackendVersion` carry
`extensions`: payloads keyed by a namespace (e.g. `neocloud.io/interruption`)
that the core preserves and the digest covers but never interprets. The public
snapshot has none. Private features register an `Extension`; generation fails
when one writes outside its namespace. Their payloads ship as an `Overlay` (an
OCI artifact of type `application/vnd.neocloud.catalog-overlay.v4+json`, signed
by its own identity) pinned to the public snapshot digest. `Merge` adds the
payloads and refuses another digest, an unknown id or a namespace already
present. `Validate` checks only registered namespaces and ignores the rest. A
merged snapshot is identified by the public digest plus the overlay digest,
never by a merged digest.

## Platform adapters

An adapter fits the catalog to one serving platform using settings the
platform already has. It declares what the platform can express
(`Capabilities`), exports the snapshot in the platform's formats, and lists
every preset it cannot express with a documented reason: `adapters.Coverage`
fails an export where a preset is neither exported nor skipped. vLLM and SGLang
presets are equally first-class. Adding a platform is adding a package that
calls `adapters.Register` from `init`.

```sh
cd adapters && go run ./cmd/catalog-export -out ../dist   # every adapter and format
go run ./cmd/catalog-export -adapter gpustack -out ../dist
```

Adapters render every preset through `Render` (its flag table) and add only
what the platform owns. Platforms without a fit matrix get the GPU count of
the `-reference-family` fit (default `H100`), else of the best fit on a vendor
the preset has an image for: native before emulated, then the fewest GPUs.
Export extensions registered next to a catalog extension
(`adapters.RegisterExportExtension`) render its payloads into their adapter's
formats after the adapter; they may change the files but not which presets are
exported. Public builds register none.

| Format | Use it with | Notes |
|---|---|---|
| `gpustack` (`model-catalog.yaml`) | GPUStack `--model-catalog-file <url>` or an admin catalog source | One spec per preset and GPU vendor (each vendor has its own image), `mode` = preset name (GPUStack shows one spec per mode and picks the first whose `gpu_filters` match). NVIDIA specs carry `gpu_filters.compute_capability`: the backend version's `emulated_from` where it emulates the quantization (FP8: `>=7.5` vLLM, `>=8.0` SGLang), else the lowest family with its native dtype (native FP8 `>=8.9`, bf16 `>=8.0`). The pinned image goes in `image_name`, not `backend_version` (GPUStack refuses both, and its bundled runners predate our versions). No GPU count: GPUStack's scheduler sizes it. llama.cpp runs as `backend: Custom` (GPUStack has no llama.cpp backend): `run_command` holds llama-server's arguments, the GGUF file `huggingface_filename`. vLLM specs come last per repo, plain ones after speculative ones (GPUStack takes a repo's defaults from its last spec). Skipped: `topology GPUStack catalog cannot express` (multi-node, which GPUStack's scheduler decides; disaggregated, as GPUStack's prefill/decode roles are not mapped yet), `backend GPUStack cannot run`, `no Hugging Face or ModelScope source` (also a source of several files). Lost: source revision (no field), fit matrix, the CPU llama.cpp image (GPUStack matches specs by GPU vendor). |
| `airunway-recipes` | AI Runway `VLLM_RECIPES_BASE_URL=<base>` | vLLM presets only. One recipe per Hugging Face repo; further vLLM presets are derived recipes (`meta.derived_from`). The NVIDIA image and families only (see deployments). Skipped: `recipes resolver is vLLM only` (SGLang, llama.cpp), `recipes resolve to one Direct vLLM server` (multi-node, disaggregated), `one recipe per Hugging Face repo`, `recipes need a Hugging Face org/model repo`, `no whole-GPU fit to size the GPU count`. |
| `airunway-deployments` | `kubectl apply -f deployments/...` or `POST /api/deployments` with `api/...` | vLLM and SGLang presets on NVIDIA GPUs (AI Runway's API requests `nvidia.com/gpu`). SGLang runs only on AI Runway's Dynamo provider, on Dynamo's sglang-runtime image (the pinned SGLang image is not used); manifests carry `neocloud.io/required-provider`. A disaggregated SGLang preset uses `serving.mode: disaggregated` with `scaling.prefill`/`decode` GPU counts; Dynamo adds each worker's role. Gated models use AI Runway's `hf-token-secret`. Skipped: `KAITO runs llama.cpp only as an AIKit runner` (its template passes no context or engine flags to the pinned llama-server), `AI Runway has no multi-node deployments`, `topology needs Dynamo disaggregated mode` (vLLM, or roles needing different engine args), `backend AI Runway cannot run`, `ModelDeployment source is huggingface or custom only`, `no whole-GPU fit to size the GPU count`. Lost: source revision, ModelScope-only models, AMD. |

Model metadata: a GPUStack model set carries `name`, `description`, `home`
(the model's, else its first source's page), `size` (`params_b`),
`activated_size` (`params_active_b` of a mixture of experts), `categories`
(`task`: chat, completion and vision-chat `llm`, embedding `embedding`, rerank
`reranker`), `capabilities` (`context/<context_max>K`, which stands for
`long-context`, then `tools`, `reasoning`, `vision`), `licenses` and
`release_date`. Lost there: `json-schema` (GPUStack names no such capability),
`icon` (a neocloud asset path GPUStack cannot serve), `family`, `aliases`,
`gated`. AI Runway reads only a recipe's `hf_id`, `title` (the model name) and
`provider` (the repo's org), and no metadata from a ModelDeployment; recipes
also carry `model.parameter_count` and `model.context_length` (the
recipes.vllm.ai layout), unread. Every other metadata field is lost in both AI
Runway formats.

A release publishes the exports with a cosign-signed `SHA256SUMS`
(`SHA256SUMS.sigstore.json`) as release assets and on GitHub Pages, which serves
the recipes tree. Neither platform verifies signatures itself; check by hand:

```sh
cosign verify-blob SHA256SUMS --bundle SHA256SUMS.sigstore.json \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github\.com/fishing-agents/neocloud-catalog/\.github/workflows/release\.yml@refs/tags/v.*$'
sha256sum -c SHA256SUMS
```

GitHub Pages must be enabled once in the repository settings (source: GitHub
Actions) before the first release.

## Curate

Sources pin the exact content: `revision` is the commit sha, never a branch.

```sh
curl -s https://huggingface.co/api/models/<org>/<repo> | jq -r .sha
git ls-remote https://www.modelscope.cn/<org>/<repo>.git HEAD
```

Backend images are pinned release tags per vendor (`vllm/vllm-openai:vX.Y.Z`,
`lmsysorg/sglang:vX.Y.Z`, `ghcr.io/ggml-org/llama.cpp:server[-<variant>]-bNNNN`);
pull a tag successfully before committing it. Every hardware value and node type
carries its source URL. `gated` is true when a source needs an authenticated,
licence-accepted account.

## Regenerate

Hardware facts come from the overlay plus Epoch's CSV (downloaded by default;
`-csv` reads a local copy). After editing `data/families.overlay.json`:

```sh
go run ./cmd/families-import
```

After editing `data/`, from the module root:

```sh
go run ./cmd/catalog-generate
```

This validates `data/`, rewrites `snapshots/v4/catalog.json`, lists the models
without a minimum setup and prints the digest. Unchanged content keeps the
committed version and digest. Commit the snapshot; the module's embedded
snapshot is picked up at build.

## Release

Push a `v*` tag on a commit whose committed snapshot is current. The release
workflow checks that regenerating reproduces the snapshot, pushes it to
`ghcr.io/fishing-agents/neocloud-catalog` under the git tag and the moving `v4`
tag, and signs the manifest with cosign keyless. There is no signing key: the
signature binds the digest to the workflow identity
`https://github.com/fishing-agents/neocloud-catalog/.github/workflows/release.yml@refs/tags/<tag>`,
recorded in the public Rekor log.

neocloud trusts it with:

```toml
[catalog]
ref = "ghcr.io/fishing-agents/neocloud-catalog:v4"
identity_issuer = "https://token.actions.githubusercontent.com"
identity = '^https://github\.com/fishing-agents/neocloud-catalog/\.github/workflows/release\.yml@refs/tags/v.*$'
```

Pin the identity to this exact workflow file: an org-wide pattern would trust
any workflow in any repository of the org. Check a release by hand:

```sh
cosign verify ghcr.io/fishing-agents/neocloud-catalog:v4 \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github\.com/fishing-agents/neocloud-catalog/\.github/workflows/release\.yml@refs/tags/v.*$'
```

## Test

```sh
go test ./... && (cd adapters && go test ./...)
```

Pure unit tests; no database. One test regenerates `data/` and fails when the
committed snapshot is stale. Adapter exports are compared with golden files under
`adapters/<platform>/testdata/`; after a deliberate change, regenerate them with
`cd adapters && go test ./gpustack ./airunway -update` and review the diff.

## License

Apache-2.0. Isolated and dependency-free so it can lift out for donation.

`data/families.json` contains hardware figures from Epoch AI, "Data on Machine
Learning Hardware" (https://epoch.ai/data/machine-learning-hardware), licensed
under CC BY 4.0; see `NOTICE`.
