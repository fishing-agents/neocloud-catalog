# neocloud-catalog

For operators and serving platforms: one curated, validated model catalog -
models, quantizations, serving presets per backend and GPU vendor, a fit matrix
of what runs on which GPU family and node count, and sources pinned to a
revision with file manifests. It ships as a signed, versioned snapshot and
exports directly to GPUStack and AI Runway, so choosing hardware and flags for a
model needs no guesswork.

Delivered as a stdlib-only Go module and a cosign-signed OCI artifact (schema
4). The catalog service in [`service/`](service/README.md) curates and imports
it; neocloud only ingests the snapshot.

## Layout

- `catalog.go` - core types (`Family`, `NodeType`, `ModelTemplate`,
  `ModelSource`, `Preset`, `Backend`, `ModelFit`, `Template`,
  `CatalogSnapshot`), `Fit`, `VRAMRequired`, `Digest`.
- `artifact.go` - OCI artifact contract and `ParseVersion`.
- `extension.go` - `Extension`, `RegisterExtension`, overlay `Merge`.
- `nodetype.go` - `NodeTypeVocabulary`, `FallbackNodeType`.
- `render.go` - `Render`/`RenderRole`: preset -> engine argv via the backend
  version's flag table.
- `generate.go` - `Generate`: validate `data/`, derive fits and guidance, run
  extensions.
- `estimate.go` - `Estimate`: memory-bandwidth roofline; tuning constants live here.
- `data/` - curated sources: `models.json`, `draft_models.json`, `backends.json`,
  `families.json` + `families.overlay.json`, `node_types.json`, and
  `templates/` (one template per file, `<kind>-<slug>-<version>.json`).
- `snapshots/v4/catalog.json` - generated, embedded snapshot (`SnapshotV4()`).
- `cmd/catalog-generate` - `data/` -> snapshot.
- `runrecord/` - run-record contract (`schema/run-record.v1.json`).
- `adapters/` - nested module: platform adapters and `cmd/catalog-export`.
- `service/` - nested module: catalog service and Helm chart.
- `.github/workflows/release.yml` - publishes and signs on tag.

## Snapshot contract

- `SchemaVersion` is the wire format; clients gate on the major (4).
- `Version` changes with content; `Digest` is the content hash clients pin
  (and the ETag of neocloud's `/v1/model-catalog`).
- Published as a single-layer OCI artifact
  `application/vnd.neocloud.catalog.v4+json`, version in the
  `org.opencontainers.image.version` annotation, cosign-signed. Clients verify
  the Sigstore bundle (OCI referrer) against the manifest digest and publisher
  identity, refuse non-newer versions and keep the last good snapshot on any
  failure.
- Schema 3 is no longer published; the `v3` tag serves its last release. Point
  `catalog.ref` at `:v4`.

Field semantics: see the types in `catalog.go`. Key rules:

- Presets are engine-neutral (`purpose`, `context`, `kv_cache_dtype`,
  `speculative`, `topology`, ...); each backend version's flag table maps them.
  Speculative methods mapped today: `eagle`, `eagle3`. Topologies: `single`,
  `multi-node` (fit keys end in `/<n>-node`), `disaggregated` (`nixl`,
  `mooncake`).
- A fit is `native` when the quantization dtype is native to the family,
  `emulated` when the backend runs it weight-only from a compute capability.
  VRAM budget is 90% of GPU memory.
- `deployment` per model: `minimum` (least GPU memory at 20 tokens/s) and
  `optimal` (highest per-GPU throughput at 20 tokens/s per sequence), from
  single-node float/fp8 fits. Estimates only.
- Node types: `<provider>/<instance>` plus fallbacks `other/<family>x<gpus>`.
- Extensions: namespaced payloads the core preserves but never interprets; the
  public snapshot has none. Private payloads ship as an overlay
  (`application/vnd.neocloud.catalog-overlay.v4+json`) pinned to the public
  digest.
- Templates (schema 4.2, optional): curated token factory, app and platform
  templates neocloud operators add in one click (see below).

## Templates

Each entry of `templates` is a complete template as the neocloud API stores it
(`neocloud-api/docs/contracts/apps.md`, `token-factories.md`), keyed by `kind`
(`token_factory`, `app`, `platform`), `slug` and `version`, plus:

- `status`: `tested` (verified end to end on a cluster) or `preview` (listed,
  shown as preview).
- `upstream_chart_ref`: `oci://<host>/<repository>:<version>`, `version` its
  tag; `chart_digest`: the `sha256:` manifest digest there. Operators mirror the
  chart digest-preserving (`neocloud charts mirror`); the API rewrites the
  reference to the mirror on add. Both null for a Kubeflow workspace.
- An app's API `kind` is `app_kind` (`chart`, `kubeflow_workspace`).
- `pending_publish: true`: the chart is the catalog's own copy (below) and not
  published yet, so `chart_digest` is null and the template cannot be added.

Generation refuses an unknown kind or status, a malformed reference or digest,
a version other than the reference's tag, a repeated kind, slug and version, a
missing field the kind requires, and unknown fields in `data/templates`. Pin
the digest the registry reports for the tag:

```sh
helm pull oci://<host>/<repository> --version <version>   # prints Digest: sha256:...
```

No token factories ship yet: the gateway charts considered (LiteLLM, Kong, vLLM
router) take their upstream models in their own shape, not the API's
`[{name, model, url}]` list.

### Chart copies

Charts without an OCI upstream (MLflow from community-charts, bedag's `raw`
for kagent agents) are pulled verbatim from their Helm repository, checked
against the tarball sha256 in the release workflow's `charts` matrix, pushed
once to `ghcr.io/fishing-agents/charts` and signed with the release identity.
`helm push` stamps the push time into the manifest, so the workflow never
pushes a published version again. A new copy: add it to the matrix and its
template with `pending_publish: true`; after the release, copy the digest from
the job summary into `chart_digest`, drop `pending_publish`, regenerate and
release again. Make each new GHCR package public once.

## Platform adapters

Each adapter exports presets in a platform's formats and must list every preset
it skips with a reason (`adapters.Coverage`). Add a platform by a package that
calls `adapters.Register` from `init`.

```sh
cd adapters && go run ./cmd/catalog-export -out ../dist   # all adapters
go run ./cmd/catalog-export -adapter gpustack -out ../dist  # flags: -format -snapshot -reference-family (default H100)
```

| Format | Use with |
|---|---|
| `gpustack` (`model-catalog.yaml`) | GPUStack `--model-catalog-file <url>` |
| `airunway-recipes` | AI Runway `VLLM_RECIPES_BASE_URL=<base>` (vLLM only) |
| `airunway-deployments` | `kubectl apply -f deployments/...` or `POST /api/deployments` (NVIDIA only) |

Releases attach the exports with a cosign-signed `SHA256SUMS` and serve the
recipes tree on GitHub Pages (enable Pages with source GitHub Actions once).

```sh
cosign verify-blob SHA256SUMS --bundle SHA256SUMS.sigstore.json \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github\.com/fishing-agents/neocloud-catalog/\.github/workflows/release\.yml@refs/tags/v.*$'
sha256sum -c SHA256SUMS
```

## Curate and regenerate

Pin `revision` to a commit sha, never a branch; pin backend images to release
tags you have pulled.

```sh
curl -s https://huggingface.co/api/models/<org>/<repo> | jq -r .sha
git ls-remote https://www.modelscope.cn/<org>/<repo>.git HEAD
```

Hardware facts: edit `data/families.json` with `data/families.overlay.json`;
from `service/`, `go run ./cmd/catalog-service import epoch` imports them and
`NEOCLOUD_CATALOG_LIVE=1 go test ./internal/importers/epoch -run TestLive`
checks them against Epoch.

After editing `data/`, from the repo root:

```sh
go run ./cmd/catalog-generate   # validates, rewrites snapshots/v4/catalog.json, prints digest
```

Commit the snapshot; unchanged content keeps version and digest.

## Release

Push a `v*` tag on a commit with a current snapshot. The workflow checks the
snapshot reproduces, pushes it to `ghcr.io/fishing-agents/neocloud-catalog`
under the tag and `v4`, and signs keyless (identity: the release workflow at
the tag, logged in Rekor). neocloud trusts it with:

```toml
[catalog]
ref = "ghcr.io/fishing-agents/neocloud-catalog:v4"
identity_issuer = "https://token.actions.githubusercontent.com"
identity = '^https://github\.com/fishing-agents/neocloud-catalog/\.github/workflows/release\.yml@refs/tags/v.*$'
```

Pin the identity to this workflow file; an org-wide pattern trusts any workflow
in the org.

```sh
cosign verify ghcr.io/fishing-agents/neocloud-catalog:v4 \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github\.com/fishing-agents/neocloud-catalog/\.github/workflows/release\.yml@refs/tags/v.*$'
```

## Test

```sh
go test ./... && (cd adapters && go test ./...)
cd adapters && go test ./gpustack ./airunway -update   # regenerate goldens, review diff
```

A test fails when the committed snapshot is stale.

## License

Apache-2.0. `data/families.json` contains Epoch AI hardware data
(https://epoch.ai/data/machine-learning-hardware), CC BY 4.0; see `NOTICE`.
