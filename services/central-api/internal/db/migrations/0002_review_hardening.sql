-- Schema version 2: review hardening (security review 2026-11-09).
--
-- 1. Four-eyes rule across versions. 0001 only compared verified_by with
--    the edited_by copied from the reviewed draft, so author A's content
--    could be self-verified once editor B saved any follow-up draft on top
--    of it (A -> B -> A). A reviewer may not verify a scheme if they edited
--    ANY draft since the last VERIFIED version.
-- 2. Withdrawal. A reviewer can mark the live VERIFIED version OUTDATED by
--    appending an OUTDATED copy of it (POST /v1/admin/schemes/{id}/withdraw).
--    withdrawn_by records who did it; edited_by stays the content author,
--    exactly as for VERIFIED copies.

ALTER TABLE scheme_versions ADD COLUMN withdrawn_by uuid REFERENCES users (id);
ALTER TABLE scheme_versions ADD CONSTRAINT scheme_versions_withdrawn_by_outdated
    CHECK ((status = 'OUTDATED') = (withdrawn_by IS NOT NULL));

-- Rejects a VERIFIED row whose reviewer edited any NEEDS_REVIEW/UNVERIFIED
-- version of the same scheme after the previous VERIFIED version.
-- (OUTDATED rows are copies of already-verified content and do not count.)
-- The equal-editor case is left to the CHECK (verified_by <> edited_by),
-- which reports it as a check-constraint violation.
CREATE FUNCTION js_scheme_four_eyes() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    last_verified integer;
BEGIN
    IF NEW.status <> 'VERIFIED' OR NEW.verified_by IS NULL OR NEW.verified_by = NEW.edited_by THEN
        RETURN NEW;
    END IF;
    SELECT max(version) INTO last_verified FROM scheme_versions
     WHERE scheme_id = NEW.scheme_id AND status = 'VERIFIED' AND version < NEW.version;
    IF EXISTS (SELECT 1 FROM scheme_versions
                WHERE scheme_id = NEW.scheme_id
                  AND version > COALESCE(last_verified, 0) AND version < NEW.version
                  AND status IN ('NEEDS_REVIEW', 'UNVERIFIED')
                  AND edited_by = NEW.verified_by) THEN
        RAISE EXCEPTION 'four-eyes rule: reviewer edited scheme % since its last verified version', NEW.scheme_id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER scheme_versions_four_eyes BEFORE INSERT ON scheme_versions
    FOR EACH ROW EXECUTE FUNCTION js_scheme_four_eyes();
