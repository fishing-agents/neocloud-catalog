-- Modify "cost_estimates" table
ALTER TABLE "cost_estimates" ADD COLUMN "cheapest" boolean NOT NULL DEFAULT false;
