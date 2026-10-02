# neocloud-catalog

A public, CNCF-donation-ready model-template catalog: curated model templates and
a fit matrix of which GPU device families run them, at which quantization, on how
many GPUs, with what context and VRAM. The catalog is a headless data artifact, not a service - it ships as a
pure Go module whose whole value is the versioned, digest-pinned snapshot it
embeds. neocloud ingests that snapshot; it does not curate models itself.

## What is here

- `catalog.go` - the pure catalog core. Stdlib only: no GORM, tenancy, or HTTP.
  Types: `ModelTemplate` (weights per quantization, architecture, derived KV
  bytes/token, context limits), `Architecture` (layers, attention heads, KV heads,
  head dim), `ModelFit` (model x family x quantization -> GPU count, max context,
  VRAM), `CatalogSnapshot`, `VRAMRequired` (weights + KV for any context), `Fit`,
  `Digest` (sha256 of the snapshot content, the pin clients use).
- `data/` - the curated sources. `models.json` holds each model's published
  architecture (from its config) and checkpoint size per quantization;
  `families.json` is the per-family GPU memory table.
- `generate.go` - the outsourced generation. KV bytes/token = 2 x layers x KV
  heads x head dim x 2 bytes. For each model, quantization, and family,
  `Generate` picks the smallest tensor-parallel GPU count (1/2/4/8, dividing the
  attention heads) whose budget (90% of GPU memory, vLLM's default) holds the
  weights plus the default context; max context is what the rest holds, capped
  at the model's ceiling. No throughput figures: the catalog states only what it
  can derive.
- `snapshots/v2/catalog.json` - the committed, generated snapshot the module
  embeds (schema 2.0.0). `SnapshotV2()` returns it.
- `cmd/catalog-generate` - the pipeline CLI: reads `data/`, writes the snapshot.

## Snapshot contract

The snapshot is the sellable/embeddable artifact. `SchemaVersion` is the wire
format (bump on a breaking shape change); `Version` changes on any generation;
`Digest` is the content hash clients pin and the ETag for conditional gets. The
`/v1/model-catalog` endpoint in neocloud serves the same shape and semantics.

## Regenerate

After editing `data/`, from the module root:

```sh
go run ./cmd/catalog-generate
```

This rewrites `snapshots/v2/catalog.json` with a fresh version and prints the
digest. Commit the snapshot. The module's embedded snapshot is picked up at build.

## Test

```sh
go test ./...
```

Pure unit tests; no database.

## License

Apache-2.0. Isolated and dependency-free so it can lift out for donation.
