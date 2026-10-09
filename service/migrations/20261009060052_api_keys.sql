-- Create "api_keys" table
CREATE TABLE "api_keys" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "created_at" timestamptz NULL,
  "updated_at" timestamptz NULL,
  "name" text NOT NULL,
  "prefix" text NOT NULL,
  "key_hash" text NOT NULL,
  "expires_at" timestamptz NULL,
  PRIMARY KEY ("id")
);
-- Create index "idx_api_keys_key_hash" to table: "api_keys"
CREATE UNIQUE INDEX "idx_api_keys_key_hash" ON "api_keys" ("key_hash");
