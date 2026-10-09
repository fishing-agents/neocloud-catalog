-- Modify "releases" table
ALTER TABLE "releases" ADD COLUMN "overlay" jsonb NOT NULL;
