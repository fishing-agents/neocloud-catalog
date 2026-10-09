-- Releases are immutable: a row only ever changes to record its publication,
-- once, and a published release is never deleted. Atlas does not model
-- triggers from GORM, so this migration is written by hand.
CREATE FUNCTION "releases_immutable"() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    IF OLD.published_at IS NOT NULL THEN
      RAISE EXCEPTION 'release % is published and cannot be deleted', OLD.version;
    END IF;
    RETURN OLD;
  END IF;
  IF OLD.published_at IS NOT NULL
    OR (NEW.id, NEW.created_at, NEW.version, NEW.digest, NEW.schema_version, NEW.generator_run_id, NEW.overlay)
      IS DISTINCT FROM (OLD.id, OLD.created_at, OLD.version, OLD.digest, OLD.schema_version, OLD.generator_run_id, OLD.overlay) THEN
    RAISE EXCEPTION 'release % is immutable', OLD.version;
  END IF;
  RETURN NEW;
END
$$;
CREATE TRIGGER "releases_immutable" BEFORE UPDATE OR DELETE ON "releases"
  FOR EACH ROW EXECUTE FUNCTION "releases_immutable"();
