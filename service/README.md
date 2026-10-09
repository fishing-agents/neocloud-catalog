# catalog-service

The neocloud catalog service: a Postgres-backed store of the catalog's model
templates, hardware facts and node types, the importers that fill it, the
generator that derives fits, estimates, guidance and cost estimates from it,
and the releases that publish the signed schema 4 export. neocloud-api,
GPUStack, AI Runway and air-gapped installs keep consuming that export; none
of them reads this database.

It is a nested Go module (`github.com/fishman/neocloud-catalog/service`) next
to the stdlib-only core, which it shares the types, `Generate`, `Render`,
`Merge`, `Validate` and the export encoding with.

Until the cutover the committed `data/*.json` stays the bootstrap input and CI
keeps releasing from those files (see the core README's Release section).

## Architecture

```
importers (bootstrap-from-json, epoch, huggingface, azure-prices, kaito, upstream)
        |                                   /v1 curation API (operators)
        v                                          |
   Postgres  <------------------------------------+
   (layers: upstream | local | overlay; provenance per value)
        |
   generator (fits, estimates, guidance, cost estimates) -> derived tables
        |
   release: signed schema 4 export (OCI artifact) + overlay + adapter exports
```

- `cmd/catalog-service` - the binary: `serve` plus one-shot commands.
- `internal/model` - GORM models, the schema authority; `migrations/` -
  versioned Atlas migrations generated from them.
- `internal/store` - layered reads and writes, the effective catalog.
- `internal/importers/*` - one package per importer; `internal/jobs` - River
  jobs on the `catalog` queue for import and generator runs.
- `internal/generator`, `internal/costs` - generator runs and cost estimates.
- `internal/release` - releases: create, publish (OCI push, cosign signing),
  adapter exports.
- `internal/httpapi` (generated from `api/openapi.yaml`), `internal/auth`,
  `internal/server` - the HTTP API, `/healthz`, `/readyz`, `/metrics`.
- `charts/catalog-service` - the Helm chart; `Dockerfile` - its image.

## Layers and provenance

Every curated or imported row carries its `layer`, `source` (an importer name
or `curation`), `source_url`, `as_of` and the import run that wrote it.
Tables mixing sources (models, quantizations, families, node types, draft
models) also carry `field_provenance`: per field, where its value came from.
`GET /v1/catalog?provenance=true` returns both.

- `upstream` - what the importers write. In our deployment the bootstrap,
  Epoch and Hugging Face importers; in an operator deployment only our
  release (read-only there).
- `local` - operator curation through the API. A local object replaces the
  upstream object with the same key as a whole row; deleting it restores the
  upstream one. A local object takes the position of the upstream object it
  overrides; local-only objects follow all upstream objects, ordered by their
  own position and key, so upstream reorders never move them.
- `overlay` - extension payloads only, never core fields (core `Merge` rules).

An importer never overwrites a field whose provenance is curation; its run
summary lists each curated value kept over a differing imported one under
`conflicts` (curated and imported value), the whole-layer imports
(`bootstrap-from-json`, `upstream`) included. The bootstrap run records its
rows as `bootstrap-from-json`, not as curation, so later importers may correct
them; curation enters only through curation writes (the local layer and the
curated `families.overlay.json`).

## Importers

Each run is a job on the `catalog` queue and a row in `import_runs` with its
status (`succeeded` may still list per-source errors), summary and changes.
Importers are idempotent per input version: a second run over unchanged input
changes nothing.

| Importer | Deployment | Writes |
|---|---|---|
| `bootstrap-from-json` | ours | the upstream layer from `bootstrap.data_dir` (`data/*.json`) |
| `epoch` | ours | family hardware facts: Epoch AI's ML hardware data merged with `families.overlay.json` |
| `huggingface` | ours | file manifests, weight sizes, parameter counts, architecture and context limits read at each source's pinned revision |
| `azure-prices` | both | Azure Retail Prices (Linux on-demand, spot, 1-year reserved) of the `azure/` node types in `azure_prices.regions`; internal |
| `kaito` | both | nothing: lists disagreements with KAITO's model catalog at its pinned release |
| `upstream` | operator | our signed release into the upstream layer and, with `upstream.overlay_ref`, our signed overlay pinned to it into the overlay layer |

- Hugging Face: gated repositories need a token in `huggingface.token_file`
  (a mounted Secret, read at startup). Without one, gated sources are
  reported as `gated: token missing`. The recorded config fixtures of the
  gated Llama 3.1 8B and 70B and Gemma 2 9B repositories hold the Hub's 401;
  with a token whose account accepted those licences, replace each
  `testdata/hf/<repo>/<revision>/config.401.txt` by the `config.200.json`
  the Hub returns.
- Azure prices: a node type priced in none of the configured regions is an
  error of the run and loses its upstream price rows; a price of 0 counts as
  unpriced. The run fails only when Azure node types exist and none could be
  priced; a catalog without `azure/` node types succeeds.
- Upstream: verified as neocloud-api verifies a release (cosign bundle as an
  OCI referrer, keyless identity or public key), and refused when older than
  a release already imported.

Run one from the CLI (`catalog-service import <importer>`) or the API
(`POST /v1/imports {"importer": "<name>"}`).

## Generator and cost estimates

A generator run (`catalog-service generate`, `POST /v1/generator-runs`) runs
the core generation over the effective catalog and writes fits, estimates
and guidance with the input import runs and an input digest. Cost estimates
are computed in the same run per fit and guidance setup, cloud node type,
region and billing type (instances needed, USD per hour, USD per month =
hourly x 730), each with its price's source and `as_of`.

## Prices and cost estimates are internal

Prices and cost estimates are stored and served by this service only
(`/v1/local/prices`, `/v1/cost-estimates`). No release, overlay or adapter
export carries them: provider price terms do not clearly allow
redistribution. They stay outside a generator run's input digest, so a price
change never changes a release.

## Releases

- `release create <generator-run-id>` (`POST /v1/releases`) freezes a
  succeeded generator run. Its version is the run's generation time
  (`catalog.VersionLayout`), so it is always newer than any upstream release
  it imported.
- `release publish` (`POST /v1/releases/{version}/publish`) pushes the public
  export (schema 4: no prices, cost estimates or extensions) to
  `release.ref` as the signed catalog artifact, and with
  `release.overlay_ref` the overlay re-pinned to the release digest. A
  release is published once; publishing it again with identical content
  changes nothing.
- Releases are immutable: a database trigger refuses changes to a published
  release's content and refs. Publishing is monotonic: a release older than
  a published one is refused, so a tag never moves back.
- Signing: the API signs with `release.key` (a cosign key pair; password in
  `COSIGN_PASSWORD`), within `release.publish_timeout`. The CLI signs with
  `-key <cosign.key>` or `-keyless` (cosign's keyless flow, for CI) and with
  `-exports <dir>` also writes the adapter exports and a signed `SHA256SUMS`.

## API

`api/openapi.yaml` is the contract (`make generate`). Every `/v1` call needs a
bearer token: an API key or, with `auth.issuer_url` set, an OIDC JWT from ICP
whose `aud` contains `auth.audience`. Reads take any authenticated principal;
writes take an API key or a JWT whose roles claim contains `admin`. When the
auth backend fails the API answers 503 `auth_unavailable`, not 401.

API keys are minted with the CLI, printed once and stored as their SHA-256:

```sh
catalog-service auth issue -name ops -prefix ops [-expires 720h]
# in a cluster:
kubectl exec deploy/catalog-service -- /catalog-service auth issue -name ops
```

Endpoints: `GET /v1/catalog` (effective catalog), `/v1/local/models/{slug}`
and its `presets/{name}`, `/v1/local/node-types/{id}`, `/v1/local/prices`,
`/v1/imports` and `/v1/imports/{id}`, `/v1/generator-runs` and
`/v1/generator-runs/{id}`, `/v1/releases`, `/v1/releases/{version}/publish`,
`/v1/cost-estimates`.

## Configuration

Embedded defaults (`internal/config/defaults.toml`, which documents every
key), then `$NEOCLOUD_CATALOG_CONFIG_DIR/*.toml` in lexical order, then
`NEOCLOUD_CATALOG_<KEY>` env (`NEOCLOUD_CATALOG_DATABASE_DSN` ->
`database.dsn`). Sections: `server`, `database`, `telemetry`, `bootstrap`,
`huggingface`, `azure_prices`, `auth`, `jobs`, `release`, `upstream`.

`jobs.job_timeout` cancels a run on the catalog queue (runs are never
retried); a run still running after `jobs.rescue_after` was left by a worker
that exited and is failed. On SIGTERM `serve` gives running jobs
`server.write_timeout` to finish, then cancels them and records them as
failed.

`import` and `generate` share the queue with `serve`'s worker: they work it
until their own job finished, then until any job of another process they
took finished (at most `jobs.job_timeout`), and never cancel such a job. A
signal cancels only their own job; a second signal exits at once.

## Schema

The GORM models are the schema authority. `make migrate-diff NAME=<name>`
writes the next versioned migration from them (Atlas, against a docker
postgres:16 dev database). Production applies the versioned migrations with
`atlas migrate apply` (`make migrate-apply`, or the chart's migrate Job);
`make migrate` (GORM AutoMigrate plus the releases trigger) is for dev and
tests only. `serve` migrates River's own tables at startup.

## Run locally

```sh
make migrate                       # dev schema in database.dsn
go run ./cmd/catalog-service import bootstrap-from-json
go run ./cmd/catalog-service generate
make serve
```

## Deploy

Build the image from the repository root (the module replaces `../` and
`../adapters`): `make image`, or `docker build -f service/Dockerfile .`. It
carries the binary, the curated `data/` and the migrations.

The chart (`charts/catalog-service`, Kubernetes 1.36+) runs `serve` (the API
and the catalog queue's workers) and a pre-install and pre-upgrade hook Job
that applies the migrations with the pinned `arigaio/atlas` image, mounting
the service image as an image volume, so the migrations always match the
binary. `config` in the values is a TOML overlay with the keys of
`defaults.toml`; the chart sets the listen addresses, `bootstrap.data_dir`
and the paths of the files it mounts.

Secrets are referenced, never values:

| Values | Secret | Used for |
|---|---|---|
| `database.dsnSecretName`, `dsnSecretKey` | the Postgres DSN | service and migrate Job (env only) |
| `signing.secretName` | `cosign.key`, `cosign.password` | `release.key`; required with `config.release.ref` |
| `huggingface.tokenSecretName`, `tokenSecretKey` | Hugging Face token | `huggingface.token_file` (gated repositories); not mounted in an operator deployment |
| `registryAuth.secretName` | `kubernetes.io/dockerconfigjson` | pushing releases, pulling a private upstream |
| `imagePullSecrets` | `kubernetes.io/dockerconfigjson` | pulling the service image (Deployment and migrate Job); may name the `registryAuth` Secret |

```sh
kubectl create secret generic catalog-service-db --from-literal=dsn='postgres://...'
cosign generate-key-pair k8s://<namespace>/catalog-service-cosign
helm install catalog-service charts/catalog-service \
  --set image.repository=<registry>/catalog-service --set image.tag=<tag> \
  --set signing.secretName=catalog-service-cosign \
  --set config.release.ref=<registry>/neocloud-catalog:v4
```

`serviceMonitor.enabled` scrapes `/metrics` on the metrics port.

### Operator deployment

An operator runs the same chart with `config.upstream.ref` set. The deployment
then registers only the `upstream`, `azure-prices` and `kaito` importers; the
upstream layer is our release, replaced whole by each upstream import. The
operator curates `local`, and publishes with its own key: its release (newer
than ours by construction) and, with `config.release.overlay_ref`, our
overlay re-pinned to its release digest, both signed with
`signing.secretName`.

```yaml
config:
  upstream:
    ref: ghcr.io/fishing-agents/neocloud-catalog:v4
    identity_issuer: https://token.actions.githubusercontent.com
    identity: '^https://github\.com/fishing-agents/neocloud-catalog/\.github/workflows/release\.yml@refs/tags/v.*$'
    overlay_ref: <our overlay repository>:v4
    overlay_identity_issuer: <our overlay identity issuer>
    overlay_identity: <our overlay identity>
  release:
    ref: registry.example.com/operator/neocloud-catalog:v4
    overlay_ref: registry.example.com/operator/neocloud-catalog-overlay:v4
signing:
  secretName: operator-cosign
registryAuth:
  secretName: operator-registry
```

Air-gapped or private registries: `upstreamTrust.publicKey` and
`overlayPublicKey` (PEM) trust a key instead of a keyless identity,
`upstreamTrust.trustedRoot` (a Sigstore `trusted_root.json`) verifies keyless
signatures without the public-good TUF repository, and
`config.upstream.plain_http` talks HTTP to a registry mirror. The chart
refuses `upstreamTrust` without `config.upstream.ref`.

The operator's neocloud-api then trusts the operator's key:
`catalog.ref` at the operator's release, `catalog.public_key` and
`catalog.overlay_public_key` at its `cosign.pub`.

## Test

```sh
make test-unit                     # no database: DB tests skip
make test-integration              # Postgres at TEST_DSN; per-package databases, dropped after
make chart-test                    # helm template and helm lint; skipped without helm
NEOCLOUD_CATALOG_LIVE=1 go test ./internal/importers/... -run TestLive
```

The Epoch, Hugging Face, Azure prices and KAITO importers are tested against
recordings under `testdata/`; each has one opt-in live test that checks the
recordings still match the live source.
