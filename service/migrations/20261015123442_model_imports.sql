-- Create "model_imports" table
CREATE TABLE "model_imports" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "created_at" timestamptz NULL,
  "updated_at" timestamptz NULL,
  "source" text NOT NULL,
  "repo" text NOT NULL,
  "revision" text NOT NULL,
  "commit" text NOT NULL,
  "tenant_id" text NOT NULL,
  "private" boolean NOT NULL,
  "state" text NOT NULL,
  "model_slug" text NOT NULL,
  "reason" text NOT NULL,
  "error" text NOT NULL,
  "report" jsonb NOT NULL,
  "decided_at" timestamptz NULL,
  "started_at" timestamptz NULL,
  "finished_at" timestamptz NULL,
  PRIMARY KEY ("id"),
  CONSTRAINT "chk_model_imports_private" CHECK ((NOT private) OR (tenant_id <> ''::text)),
  CONSTRAINT "chk_model_imports_source" CHECK (source = ANY (ARRAY['huggingface'::text, 'modelscope'::text])),
  CONSTRAINT "chk_model_imports_state" CHECK (state = ANY (ARRAY['requested'::text, 'queued'::text, 'running'::text, 'succeeded'::text, 'failed'::text, 'rejected'::text]))
);
-- Create index "idx_model_imports_requested" to table: "model_imports"
CREATE UNIQUE INDEX "idx_model_imports_requested" ON "model_imports" ("tenant_id", "repo", "revision") WHERE (state = 'requested'::text);
-- Create index "idx_model_imports_state" to table: "model_imports"
CREATE INDEX "idx_model_imports_state" ON "model_imports" ("state");
-- Create index "idx_model_imports_tenant_id" to table: "model_imports"
CREATE INDEX "idx_model_imports_tenant_id" ON "model_imports" ("tenant_id");
-- Create "tenant_models" table
CREATE TABLE "tenant_models" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "created_at" timestamptz NULL,
  "updated_at" timestamptz NULL,
  "tenant_id" text NOT NULL,
  "slug" text NOT NULL,
  "source" text NOT NULL,
  "source_url" text NOT NULL,
  "as_of" timestamptz NOT NULL,
  "model_import_id" uuid NOT NULL,
  "model" jsonb NOT NULL,
  "fits" jsonb NOT NULL,
  PRIMARY KEY ("id"),
  CONSTRAINT "fk_tenant_models_model_import" FOREIGN KEY ("model_import_id") REFERENCES "model_imports" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION
);
-- Create index "idx_tenant_models_model_import_id" to table: "tenant_models"
CREATE INDEX "idx_tenant_models_model_import_id" ON "tenant_models" ("model_import_id");
-- Create index "idx_tenant_models_natural" to table: "tenant_models"
CREATE UNIQUE INDEX "idx_tenant_models_natural" ON "tenant_models" ("tenant_id", "slug");
