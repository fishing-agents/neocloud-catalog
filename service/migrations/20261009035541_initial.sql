-- Create "import_runs" table
CREATE TABLE "import_runs" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "created_at" timestamptz NULL,
  "updated_at" timestamptz NULL,
  "importer" text NOT NULL,
  "input_version" text NOT NULL,
  "layer" text NOT NULL,
  "status" text NOT NULL,
  "started_at" timestamptz NOT NULL,
  "finished_at" timestamptz NULL,
  "summary" jsonb NULL,
  "error" text NOT NULL,
  PRIMARY KEY ("id"),
  CONSTRAINT "chk_import_runs_layer" CHECK (layer = ANY (ARRAY['upstream'::text, 'local'::text, 'overlay'::text])),
  CONSTRAINT "chk_import_runs_status" CHECK (status = ANY (ARRAY['running'::text, 'succeeded'::text, 'failed'::text]))
);
-- Create index "idx_import_runs_input" to table: "import_runs"
CREATE INDEX "idx_import_runs_input" ON "import_runs" ("importer", "input_version");
-- Create "backend_versions" table
CREATE TABLE "backend_versions" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "created_at" timestamptz NULL,
  "updated_at" timestamptz NULL,
  "layer" text NOT NULL,
  "source" text NOT NULL,
  "source_url" text NOT NULL,
  "as_of" timestamptz NOT NULL,
  "import_run_id" uuid NULL,
  "backend" text NOT NULL,
  "version" text NOT NULL,
  "position" bigint NOT NULL,
  "images" jsonb NOT NULL,
  "command" jsonb NOT NULL,
  "health_path" text NOT NULL,
  "min_compute_capability" text NOT NULL,
  "quantizations" jsonb NOT NULL,
  "defaults" jsonb NOT NULL,
  PRIMARY KEY ("id"),
  CONSTRAINT "fk_backend_versions_import_run" FOREIGN KEY ("import_run_id") REFERENCES "import_runs" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION,
  CONSTRAINT "chk_backend_versions_layer" CHECK (layer = ANY (ARRAY['upstream'::text, 'local'::text, 'overlay'::text]))
);
-- Create index "idx_backend_versions_import_run_id" to table: "backend_versions"
CREATE INDEX "idx_backend_versions_import_run_id" ON "backend_versions" ("import_run_id");
-- Create index "idx_backend_versions_natural" to table: "backend_versions"
CREATE UNIQUE INDEX "idx_backend_versions_natural" ON "backend_versions" ("layer", "backend", "version");
-- Create "backends" table
CREATE TABLE "backends" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "created_at" timestamptz NULL,
  "updated_at" timestamptz NULL,
  "layer" text NOT NULL,
  "source" text NOT NULL,
  "source_url" text NOT NULL,
  "as_of" timestamptz NOT NULL,
  "import_run_id" uuid NULL,
  "name" text NOT NULL,
  PRIMARY KEY ("id"),
  CONSTRAINT "fk_backends_import_run" FOREIGN KEY ("import_run_id") REFERENCES "import_runs" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION,
  CONSTRAINT "chk_backends_layer" CHECK (layer = ANY (ARRAY['upstream'::text, 'local'::text, 'overlay'::text]))
);
-- Create index "idx_backends_import_run_id" to table: "backends"
CREATE INDEX "idx_backends_import_run_id" ON "backends" ("import_run_id");
-- Create index "idx_backends_natural" to table: "backends"
CREATE UNIQUE INDEX "idx_backends_natural" ON "backends" ("layer", "name");
-- Create "generator_runs" table
CREATE TABLE "generator_runs" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "created_at" timestamptz NULL,
  "updated_at" timestamptz NULL,
  "status" text NOT NULL,
  "started_at" timestamptz NOT NULL,
  "finished_at" timestamptz NULL,
  "input_digest" text NOT NULL,
  "schema_version" text NOT NULL,
  "report" jsonb NOT NULL,
  "error" text NOT NULL,
  PRIMARY KEY ("id"),
  CONSTRAINT "chk_generator_runs_status" CHECK (status = ANY (ARRAY['running'::text, 'succeeded'::text, 'failed'::text]))
);
-- Create "cost_estimates" table
CREATE TABLE "cost_estimates" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "created_at" timestamptz NULL,
  "updated_at" timestamptz NULL,
  "generator_run_id" uuid NOT NULL,
  "model_slug" text NOT NULL,
  "setup" text NOT NULL,
  "fit_key" text NOT NULL,
  "node_type_id" text NOT NULL,
  "region" text NOT NULL,
  "billing" text NOT NULL,
  "instances" bigint NOT NULL,
  "usd_per_hour" numeric NOT NULL,
  "usd_per_month" numeric NOT NULL,
  "price_source" text NOT NULL,
  "price_source_url" text NOT NULL,
  "price_as_of" timestamptz NOT NULL,
  PRIMARY KEY ("id"),
  CONSTRAINT "fk_cost_estimates_generator_run" FOREIGN KEY ("generator_run_id") REFERENCES "generator_runs" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
  CONSTRAINT "chk_cost_estimates_billing" CHECK (billing = ANY (ARRAY['on_demand'::text, 'spot'::text, 'reserved_1y'::text])),
  CONSTRAINT "chk_cost_estimates_setup" CHECK (setup = ANY (ARRAY['fit'::text, 'minimum'::text, 'optimal'::text]))
);
-- Create index "idx_cost_estimates_key" to table: "cost_estimates"
CREATE UNIQUE INDEX "idx_cost_estimates_key" ON "cost_estimates" ("generator_run_id", "model_slug", "setup", "fit_key", "node_type_id", "region", "billing");
-- Create "draft_models" table
CREATE TABLE "draft_models" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "created_at" timestamptz NULL,
  "updated_at" timestamptz NULL,
  "layer" text NOT NULL,
  "source" text NOT NULL,
  "source_url" text NOT NULL,
  "as_of" timestamptz NOT NULL,
  "import_run_id" uuid NULL,
  "slug" text NOT NULL,
  "method" text NOT NULL,
  "targets" jsonb NOT NULL,
  "weights_gb" numeric NOT NULL,
  "backends" jsonb NOT NULL,
  PRIMARY KEY ("id"),
  CONSTRAINT "fk_draft_models_import_run" FOREIGN KEY ("import_run_id") REFERENCES "import_runs" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION,
  CONSTRAINT "chk_draft_models_layer" CHECK (layer = ANY (ARRAY['upstream'::text, 'local'::text, 'overlay'::text]))
);
-- Create index "idx_draft_models_import_run_id" to table: "draft_models"
CREATE INDEX "idx_draft_models_import_run_id" ON "draft_models" ("import_run_id");
-- Create index "idx_draft_models_natural" to table: "draft_models"
CREATE UNIQUE INDEX "idx_draft_models_natural" ON "draft_models" ("layer", "slug");
-- Create "extension_payloads" table
CREATE TABLE "extension_payloads" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "created_at" timestamptz NULL,
  "updated_at" timestamptz NULL,
  "layer" text NOT NULL,
  "source" text NOT NULL,
  "source_url" text NOT NULL,
  "as_of" timestamptz NOT NULL,
  "import_run_id" uuid NULL,
  "object_kind" text NOT NULL,
  "object_key" text NOT NULL,
  "namespace" text NOT NULL,
  "payload" jsonb NOT NULL,
  PRIMARY KEY ("id"),
  CONSTRAINT "fk_extension_payloads_import_run" FOREIGN KEY ("import_run_id") REFERENCES "import_runs" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION,
  CONSTRAINT "chk_extension_payloads_layer" CHECK (layer = ANY (ARRAY['upstream'::text, 'local'::text, 'overlay'::text])),
  CONSTRAINT "chk_extension_payloads_object_kind" CHECK (object_kind = ANY (ARRAY['families'::text, 'models'::text, 'presets'::text, 'fits'::text, 'backends'::text])),
  CONSTRAINT "chk_extension_payloads_overlay" CHECK (layer = 'overlay'::text)
);
-- Create index "idx_extension_payloads_import_run_id" to table: "extension_payloads"
CREATE INDEX "idx_extension_payloads_import_run_id" ON "extension_payloads" ("import_run_id");
-- Create index "idx_extension_payloads_natural" to table: "extension_payloads"
CREATE UNIQUE INDEX "idx_extension_payloads_natural" ON "extension_payloads" ("layer", "object_kind", "object_key", "namespace");
-- Create "families" table
CREATE TABLE "families" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "created_at" timestamptz NULL,
  "updated_at" timestamptz NULL,
  "layer" text NOT NULL,
  "source" text NOT NULL,
  "source_url" text NOT NULL,
  "as_of" timestamptz NOT NULL,
  "import_run_id" uuid NULL,
  "name" text NOT NULL,
  "vendor" text NOT NULL,
  "architecture" text NOT NULL,
  "compute_capability" text NOT NULL,
  "dtypes" jsonb NOT NULL,
  "memory_gb" numeric NOT NULL,
  "memory_bandwidth_gbs" numeric NOT NULL,
  "tflops" jsonb NOT NULL,
  "interconnect" text NOT NULL,
  "interconnect_gbs" numeric NOT NULL,
  "sources" jsonb NOT NULL,
  PRIMARY KEY ("id"),
  CONSTRAINT "fk_families_import_run" FOREIGN KEY ("import_run_id") REFERENCES "import_runs" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION,
  CONSTRAINT "chk_families_layer" CHECK (layer = ANY (ARRAY['upstream'::text, 'local'::text, 'overlay'::text]))
);
-- Create index "idx_families_import_run_id" to table: "families"
CREATE INDEX "idx_families_import_run_id" ON "families" ("import_run_id");
-- Create index "idx_families_natural" to table: "families"
CREATE UNIQUE INDEX "idx_families_natural" ON "families" ("layer", "name");
-- Create "fits" table
CREATE TABLE "fits" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "created_at" timestamptz NULL,
  "updated_at" timestamptz NULL,
  "generator_run_id" uuid NOT NULL,
  "fit_key" text NOT NULL,
  "model_slug" text NOT NULL,
  "device_family" text NOT NULL,
  "profile" text NULL,
  "quantization" text NOT NULL,
  "kv_cache_dtype" text NOT NULL,
  "mode" text NOT NULL,
  "gpu_count" bigint NOT NULL,
  "nodes" bigint NOT NULL,
  "max_context" bigint NOT NULL,
  "vram_required_gb" numeric NOT NULL,
  PRIMARY KEY ("id"),
  CONSTRAINT "fk_fits_generator_run" FOREIGN KEY ("generator_run_id") REFERENCES "generator_runs" ("id") ON UPDATE NO ACTION ON DELETE CASCADE
);
-- Create index "idx_fits_key" to table: "fits"
CREATE UNIQUE INDEX "idx_fits_key" ON "fits" ("generator_run_id", "fit_key");
-- Create index "idx_fits_model_slug" to table: "fits"
CREATE INDEX "idx_fits_model_slug" ON "fits" ("model_slug");
-- Create "fit_estimates" table
CREATE TABLE "fit_estimates" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "created_at" timestamptz NULL,
  "updated_at" timestamptz NULL,
  "generator_run_id" uuid NOT NULL,
  "fit_id" uuid NOT NULL,
  "source" text NOT NULL,
  "decode_tokens_per_sec" numeric NOT NULL,
  "concurrent_sequences" bigint NOT NULL,
  "aggregate_tokens_per_sec" numeric NOT NULL,
  "context" bigint NOT NULL,
  PRIMARY KEY ("id"),
  CONSTRAINT "fk_fit_estimates_fit" FOREIGN KEY ("fit_id") REFERENCES "fits" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
  CONSTRAINT "fk_fit_estimates_generator_run" FOREIGN KEY ("generator_run_id") REFERENCES "generator_runs" ("id") ON UPDATE NO ACTION ON DELETE CASCADE
);
-- Create index "idx_fit_estimates_fit_id" to table: "fit_estimates"
CREATE INDEX "idx_fit_estimates_fit_id" ON "fit_estimates" ("fit_id");
-- Create index "idx_fit_estimates_key" to table: "fit_estimates"
CREATE UNIQUE INDEX "idx_fit_estimates_key" ON "fit_estimates" ("generator_run_id", "fit_id", "source");
-- Create "flag_tables" table
CREATE TABLE "flag_tables" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "created_at" timestamptz NULL,
  "updated_at" timestamptz NULL,
  "layer" text NOT NULL,
  "source" text NOT NULL,
  "source_url" text NOT NULL,
  "as_of" timestamptz NOT NULL,
  "import_run_id" uuid NULL,
  "backend" text NOT NULL,
  "backend_version" text NOT NULL,
  "flags" jsonb NOT NULL,
  PRIMARY KEY ("id"),
  CONSTRAINT "fk_flag_tables_import_run" FOREIGN KEY ("import_run_id") REFERENCES "import_runs" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION,
  CONSTRAINT "chk_flag_tables_layer" CHECK (layer = ANY (ARRAY['upstream'::text, 'local'::text, 'overlay'::text]))
);
-- Create index "idx_flag_tables_import_run_id" to table: "flag_tables"
CREATE INDEX "idx_flag_tables_import_run_id" ON "flag_tables" ("import_run_id");
-- Create index "idx_flag_tables_natural" to table: "flag_tables"
CREATE UNIQUE INDEX "idx_flag_tables_natural" ON "flag_tables" ("layer", "backend", "backend_version");
-- Create "guidance" table
CREATE TABLE "guidance" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "created_at" timestamptz NULL,
  "updated_at" timestamptz NULL,
  "generator_run_id" uuid NOT NULL,
  "model_slug" text NOT NULL,
  "kind" text NOT NULL,
  "family" text NOT NULL,
  "gpus" bigint NOT NULL,
  "nodes" bigint NOT NULL,
  "quantization" text NOT NULL,
  "preset" text NOT NULL,
  "context" bigint NOT NULL,
  "node_types" jsonb NOT NULL,
  "decode_tokens_per_sec" numeric NOT NULL,
  "aggregate_tokens_per_sec" numeric NOT NULL,
  "source" text NOT NULL,
  "confidence" text NOT NULL,
  "fit_keys" jsonb NOT NULL,
  PRIMARY KEY ("id"),
  CONSTRAINT "fk_guidance_generator_run" FOREIGN KEY ("generator_run_id") REFERENCES "generator_runs" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
  CONSTRAINT "chk_guidance_kind" CHECK (kind = ANY (ARRAY['minimum'::text, 'optimal'::text]))
);
-- Create index "idx_guidance_key" to table: "guidance"
CREATE UNIQUE INDEX "idx_guidance_key" ON "guidance" ("generator_run_id", "model_slug", "kind");
-- Create "model_sources" table
CREATE TABLE "model_sources" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "created_at" timestamptz NULL,
  "updated_at" timestamptz NULL,
  "layer" text NOT NULL,
  "source" text NOT NULL,
  "source_url" text NOT NULL,
  "as_of" timestamptz NOT NULL,
  "import_run_id" uuid NULL,
  "model_slug" text NOT NULL,
  "quantization" text NOT NULL,
  "draft_slug" text NOT NULL,
  "type" text NOT NULL,
  "repo" text NOT NULL,
  "revision" text NOT NULL,
  "preference" bigint NOT NULL,
  PRIMARY KEY ("id"),
  CONSTRAINT "fk_model_sources_import_run" FOREIGN KEY ("import_run_id") REFERENCES "import_runs" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION,
  CONSTRAINT "chk_model_sources_layer" CHECK (layer = ANY (ARRAY['upstream'::text, 'local'::text, 'overlay'::text])),
  CONSTRAINT "chk_model_sources_owner" CHECK (((draft_slug = ''::text) AND (model_slug <> ''::text) AND (quantization <> ''::text)) OR ((draft_slug <> ''::text) AND (model_slug = ''::text) AND (quantization = ''::text)))
);
-- Create index "idx_model_sources_import_run_id" to table: "model_sources"
CREATE INDEX "idx_model_sources_import_run_id" ON "model_sources" ("import_run_id");
-- Create index "idx_model_sources_natural" to table: "model_sources"
CREATE UNIQUE INDEX "idx_model_sources_natural" ON "model_sources" ("layer", "model_slug", "quantization", "draft_slug", "type", "repo");
-- Create "model_files" table
CREATE TABLE "model_files" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "created_at" timestamptz NULL,
  "updated_at" timestamptz NULL,
  "layer" text NOT NULL,
  "source" text NOT NULL,
  "source_url" text NOT NULL,
  "as_of" timestamptz NOT NULL,
  "import_run_id" uuid NULL,
  "model_source_id" uuid NOT NULL,
  "path" text NOT NULL,
  "position" bigint NOT NULL,
  "size_bytes" bigint NOT NULL,
  "digest" text NOT NULL,
  PRIMARY KEY ("id"),
  CONSTRAINT "fk_model_files_import_run" FOREIGN KEY ("import_run_id") REFERENCES "import_runs" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION,
  CONSTRAINT "fk_model_files_model_source" FOREIGN KEY ("model_source_id") REFERENCES "model_sources" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
  CONSTRAINT "chk_model_files_layer" CHECK (layer = ANY (ARRAY['upstream'::text, 'local'::text, 'overlay'::text]))
);
-- Create index "idx_model_files_import_run_id" to table: "model_files"
CREATE INDEX "idx_model_files_import_run_id" ON "model_files" ("import_run_id");
-- Create index "idx_model_files_model_source_id" to table: "model_files"
CREATE INDEX "idx_model_files_model_source_id" ON "model_files" ("model_source_id");
-- Create index "idx_model_files_natural" to table: "model_files"
CREATE UNIQUE INDEX "idx_model_files_natural" ON "model_files" ("layer", "model_source_id", "path");
-- Create "model_quantizations" table
CREATE TABLE "model_quantizations" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "created_at" timestamptz NULL,
  "updated_at" timestamptz NULL,
  "layer" text NOT NULL,
  "source" text NOT NULL,
  "source_url" text NOT NULL,
  "as_of" timestamptz NOT NULL,
  "import_run_id" uuid NULL,
  "model_slug" text NOT NULL,
  "name" text NOT NULL,
  "position" bigint NOT NULL,
  "kind" text NOT NULL,
  "requires_dtype" text NOT NULL,
  "weights_gb" numeric NOT NULL,
  PRIMARY KEY ("id"),
  CONSTRAINT "fk_model_quantizations_import_run" FOREIGN KEY ("import_run_id") REFERENCES "import_runs" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION,
  CONSTRAINT "chk_model_quantizations_layer" CHECK (layer = ANY (ARRAY['upstream'::text, 'local'::text, 'overlay'::text]))
);
-- Create index "idx_model_quantizations_import_run_id" to table: "model_quantizations"
CREATE INDEX "idx_model_quantizations_import_run_id" ON "model_quantizations" ("import_run_id");
-- Create index "idx_model_quantizations_natural" to table: "model_quantizations"
CREATE UNIQUE INDEX "idx_model_quantizations_natural" ON "model_quantizations" ("layer", "model_slug", "name");
-- Create "models" table
CREATE TABLE "models" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "created_at" timestamptz NULL,
  "updated_at" timestamptz NULL,
  "layer" text NOT NULL,
  "source" text NOT NULL,
  "source_url" text NOT NULL,
  "as_of" timestamptz NOT NULL,
  "import_run_id" uuid NULL,
  "slug" text NOT NULL,
  "name" text NOT NULL,
  "family" text NOT NULL,
  "task" text NOT NULL,
  "capabilities" jsonb NOT NULL,
  "description" text NOT NULL,
  "params_b" numeric NOT NULL,
  "params_active_b" numeric NOT NULL,
  "release_date" text NOT NULL,
  "home" text NOT NULL,
  "icon" text NOT NULL,
  "aliases" jsonb NOT NULL,
  "license" text NOT NULL,
  "gated" boolean NOT NULL,
  "architecture" jsonb NOT NULL,
  "kv_bytes_per_token" numeric NOT NULL,
  "default_context" bigint NOT NULL,
  "context_max" bigint NOT NULL,
  PRIMARY KEY ("id"),
  CONSTRAINT "fk_models_import_run" FOREIGN KEY ("import_run_id") REFERENCES "import_runs" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION,
  CONSTRAINT "chk_models_layer" CHECK (layer = ANY (ARRAY['upstream'::text, 'local'::text, 'overlay'::text]))
);
-- Create index "idx_models_import_run_id" to table: "models"
CREATE INDEX "idx_models_import_run_id" ON "models" ("import_run_id");
-- Create index "idx_models_natural" to table: "models"
CREATE UNIQUE INDEX "idx_models_natural" ON "models" ("layer", "slug");
-- Create "node_type_prices" table
CREATE TABLE "node_type_prices" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "created_at" timestamptz NULL,
  "updated_at" timestamptz NULL,
  "layer" text NOT NULL,
  "source" text NOT NULL,
  "source_url" text NOT NULL,
  "as_of" timestamptz NOT NULL,
  "import_run_id" uuid NULL,
  "node_type_id" text NOT NULL,
  "region" text NOT NULL,
  "billing" text NOT NULL,
  "usd_per_hour" numeric NOT NULL,
  PRIMARY KEY ("id"),
  CONSTRAINT "fk_node_type_prices_import_run" FOREIGN KEY ("import_run_id") REFERENCES "import_runs" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION,
  CONSTRAINT "chk_node_type_prices_billing" CHECK (billing = ANY (ARRAY['on_demand'::text, 'spot'::text, 'reserved_1y'::text])),
  CONSTRAINT "chk_node_type_prices_layer" CHECK (layer = ANY (ARRAY['upstream'::text, 'local'::text, 'overlay'::text]))
);
-- Create index "idx_node_type_prices_import_run_id" to table: "node_type_prices"
CREATE INDEX "idx_node_type_prices_import_run_id" ON "node_type_prices" ("import_run_id");
-- Create index "idx_node_type_prices_natural" to table: "node_type_prices"
CREATE UNIQUE INDEX "idx_node_type_prices_natural" ON "node_type_prices" ("layer", "node_type_id", "region", "billing");
-- Create "node_types" table
CREATE TABLE "node_types" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "created_at" timestamptz NULL,
  "updated_at" timestamptz NULL,
  "layer" text NOT NULL,
  "source" text NOT NULL,
  "source_url" text NOT NULL,
  "as_of" timestamptz NOT NULL,
  "import_run_id" uuid NULL,
  "node_type_id" text NOT NULL,
  "family" text NOT NULL,
  "gpus" bigint NOT NULL,
  PRIMARY KEY ("id"),
  CONSTRAINT "fk_node_types_import_run" FOREIGN KEY ("import_run_id") REFERENCES "import_runs" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION,
  CONSTRAINT "chk_node_types_layer" CHECK (layer = ANY (ARRAY['upstream'::text, 'local'::text, 'overlay'::text]))
);
-- Create index "idx_node_types_family" to table: "node_types"
CREATE INDEX "idx_node_types_family" ON "node_types" ("family");
-- Create index "idx_node_types_import_run_id" to table: "node_types"
CREATE INDEX "idx_node_types_import_run_id" ON "node_types" ("import_run_id");
-- Create index "idx_node_types_natural" to table: "node_types"
CREATE UNIQUE INDEX "idx_node_types_natural" ON "node_types" ("layer", "node_type_id");
-- Create "presets" table
CREATE TABLE "presets" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "created_at" timestamptz NULL,
  "updated_at" timestamptz NULL,
  "layer" text NOT NULL,
  "source" text NOT NULL,
  "source_url" text NOT NULL,
  "as_of" timestamptz NOT NULL,
  "import_run_id" uuid NULL,
  "model_slug" text NOT NULL,
  "name" text NOT NULL,
  "position" bigint NOT NULL,
  "quantization" text NOT NULL,
  "backend" text NOT NULL,
  "backend_version" text NOT NULL,
  "min_backend_version" text NOT NULL,
  "purpose" text NOT NULL,
  "context" bigint NOT NULL,
  "tensor_parallel" bigint NOT NULL,
  "kv_cache_dtype" text NOT NULL,
  "prefix_caching" boolean NULL,
  "tool_parser" text NOT NULL,
  "reasoning_parser" text NOT NULL,
  "chat_template" text NOT NULL,
  "speculative" jsonb NOT NULL,
  "topology" jsonb NOT NULL,
  "env" jsonb NOT NULL,
  "extra_args" jsonb NOT NULL,
  "vendors" jsonb NOT NULL,
  PRIMARY KEY ("id"),
  CONSTRAINT "fk_presets_import_run" FOREIGN KEY ("import_run_id") REFERENCES "import_runs" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION,
  CONSTRAINT "chk_presets_layer" CHECK (layer = ANY (ARRAY['upstream'::text, 'local'::text, 'overlay'::text]))
);
-- Create index "idx_presets_import_run_id" to table: "presets"
CREATE INDEX "idx_presets_import_run_id" ON "presets" ("import_run_id");
-- Create index "idx_presets_natural" to table: "presets"
CREATE UNIQUE INDEX "idx_presets_natural" ON "presets" ("layer", "model_slug", "name");
-- Create "releases" table
CREATE TABLE "releases" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "created_at" timestamptz NULL,
  "updated_at" timestamptz NULL,
  "version" text NOT NULL,
  "digest" text NOT NULL,
  "schema_version" text NOT NULL,
  "artifact_ref" text NOT NULL,
  "generator_run_id" uuid NOT NULL,
  "published_at" timestamptz NULL,
  PRIMARY KEY ("id"),
  CONSTRAINT "fk_releases_generator_run" FOREIGN KEY ("generator_run_id") REFERENCES "generator_runs" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION
);
-- Create index "idx_releases_generator_run_id" to table: "releases"
CREATE INDEX "idx_releases_generator_run_id" ON "releases" ("generator_run_id");
-- Create index "idx_releases_version" to table: "releases"
CREATE UNIQUE INDEX "idx_releases_version" ON "releases" ("version");
