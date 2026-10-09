-- Modify "import_runs" table
ALTER TABLE "import_runs" ADD COLUMN "release_version" timestamptz NULL;
-- Modify "releases" table
ALTER TABLE "releases" ADD COLUMN "overlay_ref" text NOT NULL DEFAULT '';
