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
- `index.go` - `Index`, the signed pointer to a snapshot (Ed25519 over the
  digest): `SignIndex`, `VerifyIndex`.
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
- `cmd/catalog-generate` - the pipeline CLI: reads `data/`, writes the snapshot
  and, with `--sign-key`, the signed index.

## Snapshot contract

The snapshot is the sellable/embeddable artifact. `SchemaVersion` is the wire
format (bump on a breaking shape change); `Version` changes when the content
changes; `Digest` is the content hash clients pin and the ETag for conditional
gets. The `/v1/model-catalog` endpoint in neocloud serves the same shape and
semantics.

Upstream refresh: clients fetch `index.json`, verify its signature with the
publisher's public key, fetch `snapshot_url` (relative to the index), and accept
the snapshot only if its digest matches the index and its schema major is 3.
Anything else keeps the last good snapshot.

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

## Release signing

The release job signs the index; the private key never enters the repository.

1. Once, create the key (a 32-byte Ed25519 seed, hex) and store it in the
   release job's secret store: `openssl rand -hex 32 > catalog-sign.key`.
2. On a release, check out the tagged commit, write the secret to a file and
   run `go run ./cmd/catalog-generate --sign-key <file>`. The snapshot stays
   byte-identical to the committed one; `snapshots/v3/index.json` (gitignored)
   is written with the digest signature, and the public key is printed (hex).
3. Publish `index.json` and `catalog.json` side by side, for example as
   release assets. neocloud's `[catalog] url` is the index URL and
   `[catalog] public_key` the printed public key.

## Test

```sh
go test ./...
```

Pure unit tests; no database. One test regenerates `data/` and fails when the
committed snapshot is stale.

## License

Apache-2.0. Isolated and dependency-free so it can lift out for donation.
