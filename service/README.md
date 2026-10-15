# catalog-service

Runs the catalog as a service for us and for operators. It imports from Hugging
Face, Epoch AI and Azure prices with provenance per value, cross-checks against
KAITO, generates fits and guidance, and publishes signed releases. An operator
layers local changes over our release and re-publishes it under their own key.

Consumers (neocloud-api, GPUStack, AI Runway) read the export, never this
database. neocloud-api also calls it as the model-source broker: search and
inspect source repositories, import one, tenant-private models and release
webhooks ([contract](docs/contracts/broker.md)). Nested module `github.com/fishman/neocloud-catalog/service`;
it reuses the core's `Generate`, `Render`, `Merge` and `Validate`. Until the
cutover, `data/*.json` stays the bootstrap input and CI releases from it.

## Layout

- `cmd/catalog-service` - `serve` plus one-shot commands.
- `internal/model` - GORM models (schema authority); `migrations/` - Atlas migrations.
- `internal/store` - layered reads/writes, the effective catalog.
- `internal/importers/*`, `internal/jobs` - importers and model imports as River jobs on the
  `catalog` queue; webhooks on the `webhooks` queue.
- `internal/broker` - source search and inspection, per-repository model imports.
- `internal/generator`, `internal/costs`, `internal/release`.
- `internal/httpapi` (generated from `api/openapi.yaml`), `internal/auth`, `internal/server`.
- `charts/catalog-service` - Helm chart; `Dockerfile` - image.

## Layers

- `upstream` - importer output (ours) or our signed release (operator).
- `local` - operator curation; replaces the upstream object with the same key
  as a whole row, deleting it restores upstream.
- `overlay` - extension payloads only.
- tenant (`tenant_models`) - models of tenants' private repositories, per
  tenant, generated at import; never in the catalog, a generator run or a release.

Rows carry `layer`, `source`, `source_url`, `as_of` and import run; mixed-source
tables add per-field `field_provenance` (`GET /v1/catalog?provenance=true`).
Importers never overwrite curated values; kept curated values are listed under
`conflicts` in the run summary.

## Importers

Run with `catalog-service import <name>` or `POST /v1/imports`. Idempotent per
input version.

| Importer | Deployment | Writes |
|---|---|---|
| `bootstrap-from-json` | ours | upstream layer from `bootstrap.data_dir` |
| `epoch` | ours | family hardware facts (Epoch AI + `families.overlay.json`) |
| `huggingface` | ours | manifests, sizes, parameters, architecture, context at pinned revisions |
| `azure-prices` | both | Azure prices of `azure/` node types in `azure_prices.regions` (internal) |
| `kaito` | both | nothing; reports disagreements with KAITO's catalog |
| `upstream` | operator | our signed release (and `upstream.overlay_ref` overlay); refuses older releases |

`POST /v1/model-imports` imports one Hugging Face repository (`model-import`):
the operator's import is queued into `local` (operator mode too) as the import
run of its id; a tenant's request waits as `requested` (a repeat answers it)
until `.../{id}/approve` or `/reject`; a private one runs within the request with the
tenant's token (never stored or logged) into the tenant layer. The model is the
repository's weights at the pinned commit, slug `<owner>--<name>`, one preset
per backend running them, validated and generated before it is written.

Gated Hugging Face repos need `huggingface.token_file`. The gated fixtures under
`testdata/hf/<repo>/<revision>/` hold the Hub's 401 (`config.401.txt`); replace
with `config.200.json` given a token that accepted the licences.

## Generator, releases, prices

- `generate` (`POST /v1/generator-runs`) derives fits, estimates, guidance and
  cost estimates (USD/month = hourly x 730).
- `release create <generator-run-id>` (`POST /v1/releases`) freezes a run; its
  version is the run's generation time, so it is always newer than any imported
  upstream release.
- `release publish -ref <repo:tag> (-key <cosign.key> | -keyless) [-overlay-ref <repo:tag>] [-exports <dir>] <version>`
  (`POST /v1/releases/{version}/publish`, signs with `release.key`) pushes the
  signed export, optionally the re-pinned overlay and adapter exports with a
  signed `SHA256SUMS`, then queues the `release.published` webhooks. Key password
  in `COSIGN_PASSWORD`.
- Published releases are immutable (DB trigger) and monotonic (older than a
  published one is refused).
- Prices and cost estimates are internal: no release, overlay or export carries
  them (provider terms), and they are outside the generator input digest.

## API

Contract: [`api/openapi.yaml`](api/openapi.yaml) (`make generate`). `/v1` needs a
bearer token: an API key, or an OIDC JWT (`auth.issuer_url`, `aud` contains
`auth.audience`). Writes need an API key or the `admin` role. Auth backend
failure is 503 `auth_unavailable`.

```sh
catalog-service auth issue -name ops -prefix ops [-expires 720h]   # printed once, stored hashed
```

| Path | Methods |
|---|---|
| `/v1/catalog` | GET |
| `/v1/local/models/{slug}`, `.../presets/{name}` | GET PUT DELETE |
| `/v1/local/node-types/{id}`, `/v1/local/prices` | GET PUT DELETE |
| `/v1/imports` | GET POST |
| `/v1/imports/{id}`, `/v1/generator-runs/{id}` | GET |
| `/v1/generator-runs` | POST |
| `/v1/releases` | GET POST |
| `/v1/releases/{version}/publish` | POST |
| `/v1/cost-estimates` | GET |
| `/v1/model-imports` | GET POST |
| `/v1/model-imports/{id}` | GET |
| `/v1/model-imports/{id}/approve`, `.../reject` | POST |
| `/v1/tenants/{tenant_id}/models` | GET |
| `/v1/sources/search`, `/v1/sources/models/{source}/{owner}/{name}` | GET |

Publishing a release posts a signed `release.published` webhook to
`webhooks.subscribers`, retried with backoff (contract section 5).

Also `/healthz`, `/readyz`, and `/metrics` on `telemetry.metrics_addr`.

## Configuration

Embedded `internal/config/defaults.toml` (documents every key), then
`$NEOCLOUD_CATALOG_CONFIG_DIR/*.toml` in lexical order, then
`NEOCLOUD_CATALOG_<KEY>` env (`NEOCLOUD_CATALOG_DATABASE_DSN` -> `database.dsn`).

| Section | Keys |
|---|---|
| `server` | `addr`, `read_timeout`, `write_timeout` (also SIGTERM grace for running jobs) |
| `database` | `dsn`, `max_conns` |
| `telemetry` | `metrics_addr` |
| `bootstrap` | `data_dir` |
| `huggingface` | `token_file` |
| `azure_prices` | `regions` |
| `auth` | `issuer_url`, `audience` |
| `jobs` | `job_timeout` (runs never retried), `rescue_after` (orphaned runs failed) |
| `release` | `ref`, `key`, `overlay_ref`, `publish_timeout` |
| `upstream` | `ref`, `identity_issuer`, `identity`, `public_key`, `overlay_ref`, `overlay_identity_issuer`, `overlay_identity`, `overlay_public_key`, `trusted_root`, `plain_http` |
| `webhooks` | `subscribers` (`[[webhooks.subscribers]]` with `url`, `secret`) |

## Make targets

```sh
make migrate                 # dev only: GORM AutoMigrate + releases trigger
make migrate-diff NAME=<n>   # next Atlas migration from the models (docker postgres:16)
make migrate-apply           # production migrations (the chart's migrate Job does this)
make serve
make image                   # from repo root context: docker build -f service/Dockerfile .
make test-unit               # DB tests skip
make test-integration        # Postgres at TEST_DSN
make chart-test              # helm template + lint; skipped without helm
make compose-test            # deploy/: upstream publishes, operator imports (docker)
NEOCLOUD_CATALOG_LIVE=1 go test ./internal/importers/... -run TestLive
```

Local run: `make migrate`, `go run ./cmd/catalog-service import bootstrap-from-json`,
`go run ./cmd/catalog-service generate`, `make serve`.

Compose test: `deploy/test.sh` builds the image and starts postgres, a registry,
an upstream and an operator instance, then publishes a signed release upstream
and checks that the operator imports it. `deploy/test.sh down` removes the stack
and its volumes.

## Deploy

Chart `charts/catalog-service` (Kubernetes 1.36+) runs `serve` plus a
pre-install/upgrade Job applying migrations from the service image. `config` in
[values](charts/catalog-service/values.yaml) is a TOML overlay of
`defaults.toml`. Secrets are referenced by name: `database.dsnSecretName`,
`signing.secretName` (`cosign.key`, `cosign.password`), `huggingface.tokenSecretName`
(operator mode too: model imports), `registryAuth.secretName`, `imagePullSecrets`,
`webhooks.secretName` (a `webhooks.toml` with the subscribers, loaded after the config).

```sh
kubectl create secret generic catalog-service-db --from-literal=dsn='postgres://...'
cosign generate-key-pair k8s://<namespace>/catalog-service-cosign
helm install catalog-service charts/catalog-service \
  --set image.repository=<registry>/catalog-service --set image.tag=<tag> \
  --set signing.secretName=catalog-service-cosign \
  --set config.release.ref=<registry>/neocloud-catalog:v4
```

### Operator mode

Set `config.upstream.ref`: only `upstream`, `azure-prices` and `kaito` run, the
upstream layer is our release, the operator curates `local` (model imports
included) and publishes with its own key.

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

Air-gapped: `upstreamTrust.publicKey`/`overlayPublicKey` (PEM),
`upstreamTrust.trustedRoot`, `config.upstream.plain_http`. The operator's
neocloud-api sets `catalog.ref`, `catalog.public_key` and
`catalog.overlay_public_key` to the operator's release and `cosign.pub`.
