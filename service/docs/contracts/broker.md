# Catalog broker contract

Status: version 1, 2026-10-15. Normative. The key words MUST, MUST NOT, SHOULD, SHOULD NOT and MAY are to be read as in RFC 2119.

The catalog service is the model-source broker: it owns every connection to model sources (Hugging Face; ModelScope later) and their credentials, imports repositories into the catalog, and publishes signed releases. neocloud-api is its client: it owns tenants, policy and what tenants may deploy, and is the only caller frontends reach. This contract is what neocloud-api relies on, so that another catalog implementation can stand in. `api/openapi.yaml` is the schema of every body named here; this document adds the semantics.

## 1. Connection and authentication

- The client calls the broker over HTTPS at its base URL (neocloud-api's `catalog.broker_url`) with `Authorization: Bearer <key>`, an operator API key (`neocatalog_<prefix>_<secret>`). Every `/v1` operation needs it.
- GET MAY be served to any authenticated principal; every other method MUST need an operator key (or an OIDC token with the `admin` role).
- No or an invalid token is `401 unauthorized`; a read-only principal writing is `403 forbidden`; an authentication backend failure is `503 auth_unavailable` with `Retry-After`.
- Errors are `{"code": "...", "message": "..."}` with a stable `code`: `invalid_request` (400), `unauthorized` (401), `forbidden` (403), `not_found` (404), `conflict` (409), `unsupported_source` (422), `rate_limited` (429, with `Retry-After` seconds), `upstream_unavailable` (502), `auth_unavailable` (503), `internal_error` (500). The message is for humans.

## 2. Model imports

### 2.1 Requesting

`POST /v1/model-imports` with `{source, repo, revision?, tenant_id?, private?, token?}`:

| Field | Rule |
|---|---|
| `source` | `huggingface`. `modelscope` is recognised and MUST be answered `422 unsupported_source` until supported. |
| `repo` | `owner/name`; letters, digits and `_.-`, no `--` or `..` (the Hub refuses them). Else 400. |
| `revision` | branch, tag or commit; absent = the default branch. The import pins the commit it resolves. |
| `tenant_id` | the requesting tenant (1 to 128 of letters, digits and `_.:-`); absent for the operator's own import. |
| `private` | import the tenant's private repository into the tenant's layer; needs `tenant_id` and `token`. |
| `token` | the tenant's source token, for a private import only (else 400). |

The broker MUST answer:

| Request | State | Answer |
|---|---|---|
| no `tenant_id` (the operator's import) | `queued`, then run into the local layer | `202` |
| `tenant_id`, not `private` (a tenant's request of a public repository) | `requested` until an operator decides | `202` |
| `private` | `queued`, run within the request into the tenant's layer | `201` with the final state (`succeeded` or `failed`) |

Every answer carries the import (`ModelImport`) and `Location: /v1/model-imports/{id}`.

### 2.2 States

```
requested --approve--> queued --> running --> succeeded
    |                                   \----> failed
    +------reject-----> rejected
```

- `POST /v1/model-imports/{id}/approve` moves a `requested` import to `queued`; `POST /v1/model-imports/{id}/reject` with an optional `{"reason": "..."}` moves it to `rejected`. Any other state MUST be `409 conflict`; an unknown id `404`.
- `succeeded`, `failed` and `rejected` are final. An import left `running` by a broker that stopped MUST end `failed` (the reference fails it after `jobs.rescue_after`).
- `GET /v1/model-imports` lists imports newest first, filtered by `tenant_id`, `state` and `limit` (1 to 1000, default 100); `GET /v1/model-imports/{id}` returns one. Clients poll the import until a final state.
- A succeeded import names its model (`model`, the slug) and the commit imported (`commit`); a failed one says why (`error`). `report` lists what generating the model reported (presets fitting no family, no deployment minimum); it does not fail the import.

### 2.3 What an import writes

- The model's slug is `<owner>--<name>` of the repository id, lower-cased; the same repository always maps to the same slug, and re-importing it replaces that model (a newer commit, say).
- The model is one quantization of the repository's weights (float by dtype, or fp8) with a manifest of every file at the commit (path, size, `sha256:` or `git-oid:` digest), a standard preset per backend whose newest version runs that quantization, the architecture and context from `config.json`, the parameter count, licence and gating from the Hub, and the task from the pipeline tag (chat, completion, vision-chat, embedding, rerank). It MUST validate as a catalog model and is generated (fits, deployment guidance) against the catalog's device families and backends before it is written.
- The broker MUST refuse (state `failed`) a repository it cannot serve and say why: gated or private without a token that may read it, GGUF repositories (they hold every quantization; operators curate one), quantization methods no backend runs, unknown parameter counts, pipeline tags without a catalog task.
- An import without `private` writes the operator's local layer (also in an operator deployment), with provenance `source huggingface`, the Hub URL and the import run, recorded as the import run of the same id (`GET /v1/imports/{id}`, importer `model-import`). Curated values the operator set on the model are kept over imported ones, as with any import. It MUST be refused when another catalog model already holds the repository, or when a catalog model holds the slug without the repository. The model lands in the next release the operator generates and publishes; imports never publish.
- A private import writes the tenant's layer only (section 3).

### 2.4 Tokens

Public and gated repositories are read with the operator's source token (the broker's own credential). A private import's `token` MUST be used for that one import only: the broker MUST NOT store it (database, queue, files), log it or send it anywhere but the source, and drops it when the import ends. The reference runs a private import within its request (bounded by two minutes) for this reason; clients MUST allow for that latency.

## 3. Tenant layer

`GET /v1/tenants/{tenant_id}/models` returns the models of the tenant's private imports, by slug: each `{tenant_id, model_import, source, source_url, as_of, model, fits}`, where `model` is the generated catalog model (deployment guidance and `kv_bytes_per_token` included) and `fits` its fit matrix, both as of the import. An unknown tenant has none (`[]`).

- Tenant models MUST NOT appear in the catalog (`GET /v1/catalog`), a generator run or a release.
- The client MUST show a tenant model to its tenant only, next to the release catalog, and MAY deploy it like a catalog model. Weights are fetched where the GPUs are, with the tenant's credentials, not by the broker.

## 4. Sources

- `GET /v1/sources/search?source&q&limit` searches the source's public repositories (`limit` 1 to 100, default 20), most downloaded first: `[{repo, pipeline_tag, library_name, downloads, likes}]`. The broker searches anonymously, so no private repository of its account shows. Answers MAY come from a cache (the reference keeps them for 10 minutes).
- `GET /v1/sources/models/{source}/{owner}/{name}?revision` inspects one repository at a revision with the operator's token: commit, licence, gating, pipeline tag and the catalog task it maps to (empty if none), tags, the model card's metadata (`card`), every file with its size, total and weights bytes, parameter count, architecture and maximum context (when `config.json` is readable) and the detected quantizations (`[{name, kind, bytes, files}]`; kinds `float`, `fp8` and `gguf` are servable). A private, missing or unreadable repository is `404`.
- Upstream calls are rate limited: a spent budget is `429 rate_limited` with `Retry-After`; clients SHOULD retry after it. A failing source is `502 upstream_unavailable`.

## 5. Release webhook

When a release is published (`POST /v1/releases/{version}/publish`, or the `release publish` command), the broker MUST notify every configured subscriber (`webhooks.subscribers`: `url` and `secret`).

Request: `POST <url>`, `Content-Type: application/json`, body

```json
{"event": "release.published", "ref": "registry.example/operator/neocloud-catalog:v4", "digest": "sha256:...", "version": "2026-10-15T12:00:00Z"}
```

`ref` is the published repository and tag clients pull, `digest` the manifest digest it was published as, `version` the release version. Headers:

| Header | Value |
|---|---|
| `X-Neocloud-Event` | `release.published` |
| `X-Neocloud-Delivery` | a UUID, the same for every attempt of one delivery |
| `X-Neocloud-Signature` | `sha256=` and the lower-case hex HMAC-SHA256 of the raw body, keyed with the subscriber's secret |

- A subscriber MUST verify the signature over the raw body with a constant-time comparison and answer `401` on a mismatch; it MUST answer `2xx` once it accepted the event.
- Anything but `2xx`, or no answer within 30 seconds, MUST be retried with backoff (the reference: 30 s doubling to at most an hour, 12 attempts). Delivery is at least once: republishing a release notifies again and retries repeat; subscribers MUST be idempotent (deduplicate by `X-Neocloud-Delivery` or compare `digest` with what they hold).
- The webhook is a hint, not a source of trust: the subscriber MUST pull `ref` and verify its signature as it does on its interval poll, which stays as the fallback. A subscriber no longer configured is not retried.

## 6. Conformance checklist

A broker conforms when:

1. Every `/v1` operation needs an operator key or token; writes need write rights; errors use the codes of section 1.
2. `POST /v1/model-imports` starts the operator's import `queued`, a tenant's public request `requested`, and runs a private import into the tenant's layer, answering 202, 202 and 201; `modelscope` is 422.
3. Approve and reject act on `requested` imports only (409 otherwise); imports reach exactly one final state; an import left running ends failed.
4. An import pins the commit, writes one validated, generated model at the slug `<owner>--<name>` (lower-cased), refuses what it cannot serve with a reason, and never publishes.
5. A private import's token is never stored, logged or queued.
6. Tenant models are served per tenant by `GET /v1/tenants/{tenant_id}/models` and appear in no catalog, generator run or release.
7. Searches are anonymous; inspections hide private repositories; spent budgets are 429 with `Retry-After`.
8. Every publish sends the signed `release.published` event to every subscriber, at least once, retried with backoff, with one delivery id per delivery.
