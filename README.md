# neocloud-catalog

A public, CNCF-donation-ready model-template catalog: curated model templates and
a fit matrix of which GPU device families run them, at what context, VRAM, and
throughput. The catalog is a headless data artifact, not a service - it ships as a
pure Go module whose whole value is the versioned, digest-pinned snapshot it
embeds. neocloud ingests that snapshot; it does not curate models itself.

## What is here

- `catalog.go` - the pure catalog core. Stdlib only: no GORM, tenancy, or HTTP.
  Types: `ModelTemplate` (weights per quantization, KV bytes/token, context
  limits), `ModelFit` (which family runs which model), `CatalogSnapshot`,
  `VRAMRequired` (weights + kv*tokens for any context), `Fit`, `Digest` (sha256 of
  the snapshot content, the pin clients use).
- `data/` - the curated sources. `models.json` is the model catalog; `families.json`
  is the per-family GPU memory table the fit derivation uses.
- `generate.go` - the outsourced generation. `Generate` derives the fit matrix
  from the curated sources (a family fits a model when its memory exceeds the fp16
  weights; max context is what the remaining memory holds at KV bytes/token).
- `snapshots/v1/catalog.json` - the committed, generated snapshot the module
  embeds. `SnapshotV1()` returns it.
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

This rewrites `snapshots/v1/catalog.json` with a fresh version and prints the
digest. Commit the snapshot. The module's embedded snapshot is picked up at build.

## Test

```sh
go test ./...
```

Pure unit tests; no database.

## License

Apache-2.0. Isolated and dependency-free so it can lift out for donation.
