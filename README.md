# neocloud-catalog

A public, CNCF-donation-ready model-template catalog: curated model templates,
the backends that serve them, deployable presets, and a fit matrix of which GPU
device families run them, at which quantization, on how many GPUs, with what
context and VRAM. The catalog is a headless data artifact, not a service - it
ships as a pure Go module whose whole value is the versioned, digest-pinned
snapshot it embeds. neocloud ingests that snapshot; it does not curate models
itself.

## What is here

- `catalog.go` - the pure catalog core. Stdlib only: no GORM, tenancy, or HTTP.
  Types: `ModelTemplate` (weights and sources per quantization, architecture,
  derived KV bytes/token, context limits, license, gated, presets),
  `ModelSource` (huggingface, modelscope, oci or local; pinned revision),
  `Preset` (quantization + backend version + arguments), `Backend` and
  `BackendVersion` (image, command template, health path, vendors), `ModelFit`
  (model x family x quantization -> GPU count, max context, VRAM),
  `CatalogSnapshot`, `VRAMRequired` (weights + KV for any context), `Fit`,
  `Digest` (sha256 of the snapshot content, the pin clients use).
- `artifact.go` - the published OCI artifact contract (`ArtifactType`,
  `VersionAnnotation`) and `ParseVersion`, the order clients refresh by.
- `data/` - the curated sources. `models.json` holds each model's published
  architecture (from its config), checkpoint size and sources per quantization,
  license, gated flag and presets; `backends.json` the inference servers (vLLM,
  SGLang) with pinned release images and command templates (`{{model_path}}`,
  `{{model}}`, `{{port}}`); `families.json` the per-family GPU memory table.
- `generate.go` - the outsourced generation. `Generate` validates the input
  (`CatalogSnapshot.Validate`: every source has a known type and repo, and
  huggingface, modelscope and oci sources a pinned revision; every preset names
  a quantization of its model and an existing backend version; preset names are
  unique per model) and derives the fit matrix. KV bytes/token = 2 x layers x KV
  heads x head dim x 2 bytes. For each model, quantization, and family it picks
  the smallest tensor-parallel GPU count (1/2/4/8, dividing the attention heads)
  whose budget (90% of GPU memory, vLLM's default) holds the weights plus the
  default context; max context is what the rest holds, capped at the model's
  ceiling. No throughput figures: the catalog states only what it can derive.
- `snapshots/v3/catalog.json` - the committed, generated snapshot the module
  embeds (schema 3.0.0). `SnapshotV3()` returns it.
- `cmd/catalog-generate` - the pipeline CLI: reads `data/` and writes the
  snapshot.
- `.github/workflows/release.yml` - publishes and signs the snapshot on a tag.

## Snapshot contract

The snapshot is the sellable/embeddable artifact. `SchemaVersion` is the wire
format (bump on a breaking shape change); `Version` changes when the content
changes; `Digest` is the content hash clients pin and the ETag for conditional
gets. The `/v1/model-catalog` endpoint in neocloud serves the same shape and
semantics.

Upstream refresh: the snapshot is published as a single-layer OCI artifact
(`application/vnd.neocloud.catalog.v3+json`, version in the
`org.opencontainers.image.version` manifest annotation), signed with cosign. A
client resolves the reference to a manifest digest, verifies the Sigstore bundle
attached as an OCI referrer against that digest and the publisher's identity,
refuses a version not newer than the one in use, pulls the layer, and accepts
the snapshot only if its version matches the annotation and its schema major is
3. Anything else keeps the last good snapshot.

## Curate

Sources pin the exact content: `revision` is the commit sha, never a branch.

```sh
curl -s https://huggingface.co/api/models/<org>/<repo> | jq -r .sha
git ls-remote https://www.modelscope.cn/<org>/<repo>.git HEAD
```

Backend images are pinned release tags (`vllm/vllm-openai:vX.Y.Z`,
`lmsysorg/sglang:vX.Y.Z`). `gated` is true when a source needs an
authenticated, licence-accepted account.

## Regenerate

After editing `data/`, from the module root:

```sh
go run ./cmd/catalog-generate
```

This validates `data/`, rewrites `snapshots/v3/catalog.json` and prints the
digest. Unchanged content keeps the committed version and digest. Commit the
snapshot; the module's embedded snapshot is picked up at build.

## Release

Push a `v*` tag on a commit whose committed snapshot is current. The release
workflow checks that regenerating reproduces the snapshot, pushes it to
`ghcr.io/fishing-agents/neocloud-catalog` under the git tag and the moving `v3`
tag, and signs the manifest with cosign keyless. There is no signing key: the
signature binds the digest to the workflow identity
`https://github.com/fishing-agents/neocloud-catalog/.github/workflows/release.yml@refs/tags/<tag>`,
recorded in the public Rekor log.

neocloud trusts it with:

```toml
[catalog]
ref = "ghcr.io/fishing-agents/neocloud-catalog:v3"
identity_issuer = "https://token.actions.githubusercontent.com"
identity = '^https://github\.com/fishing-agents/neocloud-catalog/\.github/workflows/release\.yml@refs/tags/v.*$'
```

Pin the identity to this exact workflow file: an org-wide pattern would trust
any workflow in any repository of the org. Check a release by hand:

```sh
cosign verify ghcr.io/fishing-agents/neocloud-catalog:v3 \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github\.com/fishing-agents/neocloud-catalog/\.github/workflows/release\.yml@refs/tags/v.*$'
```

## Test

```sh
go test ./...
```

Pure unit tests; no database. One test regenerates `data/` and fails when the
committed snapshot is stale.

## License

Apache-2.0. Isolated and dependency-free so it can lift out for donation.
