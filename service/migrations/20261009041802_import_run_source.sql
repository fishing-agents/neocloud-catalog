-- Modify "import_runs" table
ALTER TABLE "import_runs" ADD COLUMN "source" text NOT NULL, ADD COLUMN "source_url" text NOT NULL;
