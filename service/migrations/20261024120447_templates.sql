-- Modify "generator_runs" table
ALTER TABLE "generator_runs" ALTER COLUMN "snapshot" TYPE json;
-- Create "templates" table
CREATE TABLE "templates" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "created_at" timestamptz NULL,
  "updated_at" timestamptz NULL,
  "kind" text NOT NULL,
  "slug" text NOT NULL,
  "version" text NOT NULL,
  "position" bigint NOT NULL,
  "body" json NOT NULL,
  PRIMARY KEY ("id")
);
-- Create index "idx_templates_natural" to table: "templates"
CREATE UNIQUE INDEX "idx_templates_natural" ON "templates" ("kind", "slug", "version");
