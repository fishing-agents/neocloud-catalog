-- A published release records its overlay's publication once, at publish or
-- later; nothing else of it changes (see 20261009052754_release_immutable).
CREATE OR REPLACE FUNCTION "releases_immutable"() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    IF OLD.published_at IS NOT NULL THEN
      RAISE EXCEPTION 'release % is published and cannot be deleted', OLD.version;
    END IF;
    RETURN OLD;
  END IF;
  IF (NEW.id, NEW.created_at, NEW.version, NEW.digest, NEW.schema_version, NEW.generator_run_id, NEW.overlay)
      IS DISTINCT FROM (OLD.id, OLD.created_at, OLD.version, OLD.digest, OLD.schema_version, OLD.generator_run_id, OLD.overlay)
    OR OLD.published_at IS NOT NULL AND (
      (NEW.artifact_ref, NEW.published_at) IS DISTINCT FROM (OLD.artifact_ref, OLD.published_at)
      OR OLD.overlay_ref <> '' AND NEW.overlay_ref IS DISTINCT FROM OLD.overlay_ref) THEN
    RAISE EXCEPTION 'release % is immutable', OLD.version;
  END IF;
  RETURN NEW;
END
$$;
