-- +goose Up

-- The outbox contains routing information and a lifecycle fact (ADR-0031). It outlives hard deletion.
CREATE TABLE resource_state_outbox (
    revision     BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    routing_key  TEXT        NOT NULL,
    project_id   UUID        NOT NULL,
    status       SMALLINT    NOT NULL CHECK (status IN (1, 2, 3)), -- active, inactive, purged
    captured_at  TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    trace_parent TEXT        NOT NULL DEFAULT '' CHECK (length(trace_parent) <= 55)
);

ALTER TABLE resources ADD COLUMN state_enrolled BOOLEAN NOT NULL DEFAULT false;

CREATE TABLE resource_state_backfill (
    singleton    BOOLEAN     PRIMARY KEY DEFAULT true CHECK (singleton),
    after_id     UUID        NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
    last_id      UUID,
    completed_at TIMESTAMPTZ
);
-- The ALTER above holds the write lock. All later INSERTs run the capture trigger regardless of ID.
INSERT INTO resource_state_backfill (last_id) VALUES ((SELECT id FROM resources ORDER BY id DESC LIMIT 1));

-- +goose StatementBegin
CREATE FUNCTION capture_resource_state() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, public AS $$
DECLARE
    org_live boolean;
    project_live boolean;
    effective_status smallint;
    event_trace text := coalesce(current_setting('app.trace_parent', true), '');
BEGIN
    IF TG_OP = 'DELETE' THEN
        INSERT INTO resource_state_outbox (routing_key, project_id, status, trace_parent)
        VALUES (OLD.organization_id::text || '/' || OLD.id::text, OLD.project_id, 3, event_trace);
        RETURN OLD;
    END IF;

    IF TG_OP = 'UPDATE' AND OLD.state_enrolled AND NEW.deleted_at IS NOT DISTINCT FROM OLD.deleted_at THEN
        RETURN NEW;
    END IF;

    -- Read the current organization after acquiring its share lock. A writer from M02 can reach
    -- this trigger without a parent lock. The organization trigger never locks resource rows.
    SELECT o.deleted_at IS NULL INTO org_live
    FROM organizations o WHERE o.id = NEW.organization_id FOR SHARE;

    SELECT p.deleted_at IS NULL INTO project_live
    FROM projects p WHERE p.organization_id = NEW.organization_id AND p.id = NEW.project_id;

    effective_status := CASE WHEN org_live AND project_live AND NEW.deleted_at IS NULL THEN 1 ELSE 2 END;
    INSERT INTO resource_state_outbox (routing_key, project_id, status, trace_parent)
    VALUES (NEW.organization_id::text || '/' || NEW.id::text, NEW.project_id, effective_status, event_trace);
    NEW.state_enrolled := true;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER resources_capture_state
BEFORE INSERT OR UPDATE OF deleted_at, state_enrolled OR DELETE ON resources
FOR EACH ROW EXECUTE FUNCTION capture_resource_state();

-- +goose StatementBegin
CREATE FUNCTION capture_organization_resource_state() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, public AS $$
BEGIN
    INSERT INTO resource_state_outbox (routing_key, project_id, status, trace_parent)
    SELECT NEW.id::text || '/' || r.id::text, r.project_id,
           CASE WHEN NEW.deleted_at IS NULL THEN 1 ELSE 2 END,
           coalesce(current_setting('app.trace_parent', true), '')
    FROM resources r JOIN projects p ON p.organization_id = r.organization_id AND p.id = r.project_id
    WHERE r.organization_id = NEW.id AND r.deleted_at IS NULL AND p.deleted_at IS NULL;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER organizations_capture_resource_state
AFTER UPDATE OF deleted_at ON organizations
FOR EACH ROW WHEN ((OLD.deleted_at IS NULL) <> (NEW.deleted_at IS NULL))
EXECUTE FUNCTION capture_organization_resource_state();

-- +goose StatementBegin
CREATE FUNCTION lock_expired_organization(org uuid) RETURNS boolean
LANGUAGE sql VOLATILE SECURITY DEFINER SET search_path = pg_catalog AS $$
    SELECT EXISTS (
        SELECT 1 FROM public.organizations o
        WHERE o.id = org AND o.deleted_at < clock_timestamp() - interval '30 days'
        FOR SHARE
    );
$$;
-- +goose StatementEnd
REVOKE EXECUTE ON FUNCTION lock_expired_organization(uuid) FROM PUBLIC;

-- +goose StatementBegin
DO $$
DECLARE
    svc text := regexp_replace(current_user, '_migrator$', '');
BEGIN
    IF svc = current_user THEN
        RAISE EXCEPTION 'migrations must run as <service>_migrator, not %', current_user;
    END IF;
    EXECUTE format('GRANT INSERT (routing_key, project_id, status, trace_parent) ON resource_state_outbox TO %I, %I',
                   svc || '_app', svc || '_batch');
    EXECUTE format('GRANT EXECUTE ON FUNCTION lock_expired_organization(uuid) TO %I', svc || '_batch');
END
$$;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION lock_expired_organization(uuid);
DROP TRIGGER organizations_capture_resource_state ON organizations;
DROP FUNCTION capture_organization_resource_state();
DROP TRIGGER resources_capture_state ON resources;
DROP FUNCTION capture_resource_state();
ALTER TABLE resources DROP COLUMN state_enrolled;
DROP TABLE resource_state_backfill;
DROP TABLE resource_state_outbox;
