-- Least-privilege grants. Applied after schema.sql by apply_schema, as the table owner.
-- Idempotent. Each role's grants are skipped if the role does not exist, so a local
-- single-user database (no initdb roles) still works.

-- Ground truth: nobody but the fault injector touches it.
REVOKE ALL ON incidents_truth FROM PUBLIC;

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'copilot_app') THEN
    REVOKE ALL ON incidents_truth FROM copilot_app;
    GRANT SELECT, INSERT, UPDATE, DELETE ON logs, doc_chunks TO copilot_app;
    GRANT USAGE, SELECT ON SEQUENCE logs_id_seq, doc_chunks_id_seq TO copilot_app;
  END IF;

  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'copilot_ro') THEN
    REVOKE ALL ON incidents_truth FROM copilot_ro;
    GRANT SELECT ON logs, doc_chunks TO copilot_ro;
  END IF;

  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'fault_injector') THEN
    -- SELECT is needed for INSERT ... ON CONFLICT, and for the injector's own
    -- UPDATE of a run's row. The copilot roles get no access at all.
    GRANT SELECT, INSERT, UPDATE ON incidents_truth TO fault_injector;
  END IF;

  -- incidents_seen and actions arrive with their tickets; grant only if they exist.
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'copilot_app') THEN
    IF to_regclass('incidents_seen') IS NOT NULL THEN
      GRANT SELECT, INSERT, UPDATE, DELETE ON incidents_seen TO copilot_app;
    END IF;
    IF to_regclass('actions') IS NOT NULL THEN
      -- No DELETE: the app must not be able to erase its own audit trail (AC-3).
      REVOKE ALL ON actions FROM copilot_app;
      GRANT SELECT, INSERT, UPDATE ON actions TO copilot_app;
    END IF;
  END IF;
END
$$;
