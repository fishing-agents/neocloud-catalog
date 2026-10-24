-- Modify "generator_runs" table
ALTER TABLE "generator_runs" ALTER COLUMN "snapshot" TYPE jsonb;
-- Drop "templates" table
DROP TABLE "templates";
