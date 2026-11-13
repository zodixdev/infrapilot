-- control-plane schema v1 (ADR-0018, ADR-0022, ADR-0023, ADR-0024, ADR-0027).
--
-- Runs as <svc>_migrator. Grants go to <svc>_app (runtime, RLS enforced) and <svc>_batch (purge job, BYPASSRLS);
-- the names are derived from the migrator's name so the same file works in compose, tests and production.
-- Tenant tables: organization_id NOT NULL, RLS, policy on app.org_id, unique indexes and foreign keys include
-- organization_id. Non-tenant tables: users (global identities), api_key_index (pre-tenant lookup) and
-- org_create_keys (idempotency of organization creates, which have no tenant yet).

-- +goose Up

-- +goose StatementBegin
DO $$
BEGIN
  IF current_user !~ '_migrator$' THEN
    RAISE EXCEPTION 'migrations must run as <service>_migrator, not %', current_user;
  END IF;
END
$$;
-- +goose StatementEnd

-- Global user identities, created on first authenticated request (OIDC subject). Not tenant data.
CREATE TABLE users (
    id           UUID        PRIMARY KEY,
    subject      TEXT        NOT NULL CHECK (char_length(subject) BETWEEN 1 AND 255),
    email        TEXT        NOT NULL CHECK (char_length(email) <= 320),
    display_name TEXT        NOT NULL DEFAULT '' CHECK (char_length(display_name) <= 200),
    -- As asserted by the identity provider; only verified addresses are matched when adding members.
    email_verified BOOLEAN   NOT NULL DEFAULT false,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX users_subject_key ON users (subject);
CREATE INDEX users_email_idx ON users (lower(email)) WHERE email_verified;
CREATE INDEX users_last_seen_idx ON users (last_seen_at);

-- Organizations are tenant rows keyed on their own id (ADR-0018); organization_id mirrors id so the uniform
-- tenant policy and schema checks apply.
CREATE TABLE organizations (
    id              UUID        PRIMARY KEY,
    organization_id UUID        GENERATED ALWAYS AS (id) STORED NOT NULL,
    name            TEXT        NOT NULL CHECK (char_length(name) BETWEEN 1 AND 200),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at      TIMESTAMPTZ
);
CREATE INDEX organizations_deleted_idx ON organizations (deleted_at) WHERE deleted_at IS NOT NULL;

CREATE TABLE memberships (
    organization_id UUID        NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    user_id         UUID        NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    role            TEXT        NOT NULL CHECK (role IN ('owner', 'admin', 'operator', 'viewer')),
    created_by      UUID,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (organization_id, user_id)
);
-- "My organizations" (ADR-0023) and the user purge's "has memberships" check.
CREATE INDEX memberships_user_idx ON memberships (user_id, organization_id);

CREATE TABLE projects (
    id              UUID        PRIMARY KEY,
    organization_id UUID        NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    name            TEXT        NOT NULL CHECK (char_length(name) BETWEEN 1 AND 200),
    slug            TEXT        NOT NULL CHECK (slug ~ '^[a-z][a-z0-9-]{1,61}[a-z0-9]$'),
    description     TEXT        NOT NULL DEFAULT '' CHECK (char_length(description) <= 2000),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at      TIMESTAMPTZ,
    UNIQUE (organization_id, id)
);
CREATE UNIQUE INDEX projects_slug_key ON projects (organization_id, slug) WHERE deleted_at IS NULL;
CREATE INDEX projects_deleted_idx ON projects (deleted_at) WHERE deleted_at IS NOT NULL;

CREATE TABLE resources (
    id              UUID        PRIMARY KEY,
    organization_id UUID        NOT NULL,
    project_id      UUID        NOT NULL,
    kind            TEXT        NOT NULL CHECK (kind IN ('host', 'service', 'endpoint', 'database', 'queue', 'container', 'other')),
    name            TEXT        NOT NULL CHECK (char_length(name) BETWEEN 1 AND 200),
    labels          JSONB       NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(labels) = 'object'),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at      TIMESTAMPTZ,
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX resources_name_key ON resources (organization_id, project_id, name) WHERE deleted_at IS NULL;
-- Keyset pagination per project, optionally filtered by kind.
CREATE INDEX resources_list_idx ON resources (organization_id, project_id, id) WHERE deleted_at IS NULL;
CREATE INDEX resources_kind_idx ON resources (organization_id, project_id, kind, id) WHERE deleted_at IS NULL;
CREATE INDEX resources_deleted_idx ON resources (deleted_at) WHERE deleted_at IS NOT NULL;
-- Supports the project foreign key for purges (the partial indexes above cannot serve the cascade lookup).
CREATE INDEX resources_project_fk_idx ON resources (organization_id, project_id);

CREATE TABLE api_keys (
    id              UUID        PRIMARY KEY,
    organization_id UUID        NOT NULL,
    project_id      UUID        NOT NULL,
    name            TEXT        NOT NULL CHECK (char_length(name) BETWEEN 1 AND 200),
    prefix          TEXT        NOT NULL CHECK (prefix ~ '^ipk_[A-Za-z0-9_-]{8}$'),
    scopes          TEXT[]      NOT NULL CHECK (cardinality(scopes) BETWEEN 1 AND 3 AND scopes <@ ARRAY['read', 'write', 'ingest']),
    created_by      UUID        NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at      TIMESTAMPTZ,
    revoked_at      TIMESTAMPTZ,
    UNIQUE (organization_id, id),
    UNIQUE (organization_id, project_id, id),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id) ON DELETE CASCADE
);
CREATE INDEX api_keys_project_idx ON api_keys (organization_id, project_id, id);
CREATE INDEX api_keys_inactive_idx ON api_keys (coalesce(revoked_at, expires_at)) WHERE revoked_at IS NOT NULL OR expires_at IS NOT NULL;

-- Pre-tenant lookup (ADR-0018): resolves a presented key to its tenant before any tenant context exists.
-- Holds only what the authentication decision needs; maintained in the same transaction as api_keys.
CREATE TABLE api_key_index (
    key_hash        BYTEA       PRIMARY KEY CHECK (octet_length(key_hash) = 32),
    key_id          UUID        NOT NULL,
    organization_id UUID        NOT NULL,
    project_id      UUID        NOT NULL,
    scopes          TEXT[]      NOT NULL CHECK (cardinality(scopes) BETWEEN 1 AND 3 AND scopes <@ ARRAY['read', 'write', 'ingest']),
    expires_at      TIMESTAMPTZ,
    revoked_at      TIMESTAMPTZ,
    UNIQUE (key_id),
    FOREIGN KEY (organization_id, project_id, key_id) REFERENCES api_keys (organization_id, project_id, id) ON DELETE CASCADE
);

-- Revocation is written once, to api_keys; the lookup row follows in the same transaction, so the two can
-- never disagree about whether a key is active. Runs with the invoker's privileges (no SECURITY DEFINER).
-- +goose StatementBegin
CREATE FUNCTION api_keys_mirror_revocation() RETURNS trigger LANGUAGE plpgsql SET search_path = public AS $$
BEGIN
  UPDATE api_key_index SET revoked_at = NEW.revoked_at
  WHERE organization_id = NEW.organization_id AND key_id = NEW.id;
  RETURN NULL;
END
$$;
-- +goose StatementEnd
CREATE TRIGGER api_keys_mirror_revocation AFTER UPDATE OF revoked_at ON api_keys
    FOR EACH ROW WHEN (OLD.revoked_at IS DISTINCT FROM NEW.revoked_at) EXECUTE FUNCTION api_keys_mirror_revocation();

-- Append-only audit log (ADR-0022, ADR-0027): the runtime role can insert and read, never change or delete.
-- No foreign key to organizations: the history outlives the organization's purge and ages out after 400 days.
CREATE TABLE audit_events (
    id              UUID        PRIMARY KEY,
    organization_id UUID        NOT NULL,
    actor_kind      TEXT        NOT NULL CHECK (actor_kind IN ('user', 'api_key')),
    actor_id        UUID        NOT NULL,
    action          TEXT        NOT NULL CHECK (action ~ '^[a-z_]+\.[a-z_]+$'),
    target_type     TEXT        NOT NULL CHECK (char_length(target_type) BETWEEN 1 AND 64),
    target_id       TEXT        NOT NULL CHECK (char_length(target_id) BETWEEN 1 AND 255),
    details         JSONB       NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(details) = 'object' AND pg_column_size(details) <= 8192),
    request_id      TEXT        NOT NULL DEFAULT '' CHECK (char_length(request_id) <= 128),
    trace_id        TEXT        NOT NULL DEFAULT '' CHECK (char_length(trace_id) <= 64),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- Newest first per organization (ids are UUIDv7, time-ordered).
CREATE INDEX audit_events_list_idx ON audit_events (organization_id, id DESC);
CREATE INDEX audit_events_created_idx ON audit_events (created_at);

-- Idempotency records (ADR-0024), written in the creating transaction.
CREATE TABLE idempotency_keys (
    organization_id UUID        NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    principal_id    UUID        NOT NULL,
    key             TEXT        NOT NULL CHECK (char_length(key) BETWEEN 1 AND 128),
    operation       TEXT        NOT NULL CHECK (char_length(operation) BETWEEN 1 AND 64),
    request_hash    BYTEA       NOT NULL CHECK (octet_length(request_hash) = 32),
    resource_id     UUID        NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (organization_id, principal_id, key)
);
CREATE INDEX idempotency_keys_created_idx ON idempotency_keys (created_at);

-- Organization creates have no tenant yet: their Idempotency-Keys live in this non-tenant table and map to a random
-- organization id (an id derived from the key could reappear after the organization is purged).
CREATE TABLE org_create_keys (
    principal_id    UUID        NOT NULL,
    key             TEXT        NOT NULL CHECK (char_length(key) BETWEEN 1 AND 128),
    organization_id UUID        NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    request_hash    BYTEA       NOT NULL CHECK (octet_length(request_hash) = 32),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (principal_id, key)
);
CREATE INDEX org_create_keys_created_idx ON org_create_keys (created_at);
-- Supports the cascade when an organization is purged.
CREATE INDEX org_create_keys_org_idx ON org_create_keys (organization_id);

-- Row-level security: tenant policy on every tenant table, plus the read-only user policies of ADR-0023.
ALTER TABLE organizations    ENABLE ROW LEVEL SECURITY;
ALTER TABLE memberships      ENABLE ROW LEVEL SECURITY;
ALTER TABLE projects         ENABLE ROW LEVEL SECURITY;
ALTER TABLE resources        ENABLE ROW LEVEL SECURITY;
ALTER TABLE api_keys         ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_events     ENABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant ON organizations
    USING (organization_id = NULLIF(current_setting('app.org_id', true), '')::uuid)
    WITH CHECK (organization_id = NULLIF(current_setting('app.org_id', true), '')::uuid);
CREATE POLICY tenant ON memberships
    USING (organization_id = NULLIF(current_setting('app.org_id', true), '')::uuid)
    WITH CHECK (organization_id = NULLIF(current_setting('app.org_id', true), '')::uuid);
CREATE POLICY tenant ON projects
    USING (organization_id = NULLIF(current_setting('app.org_id', true), '')::uuid)
    WITH CHECK (organization_id = NULLIF(current_setting('app.org_id', true), '')::uuid);
CREATE POLICY tenant ON resources
    USING (organization_id = NULLIF(current_setting('app.org_id', true), '')::uuid)
    WITH CHECK (organization_id = NULLIF(current_setting('app.org_id', true), '')::uuid);
CREATE POLICY tenant ON api_keys
    USING (organization_id = NULLIF(current_setting('app.org_id', true), '')::uuid)
    WITH CHECK (organization_id = NULLIF(current_setting('app.org_id', true), '')::uuid);
CREATE POLICY tenant ON audit_events
    USING (organization_id = NULLIF(current_setting('app.org_id', true), '')::uuid)
    WITH CHECK (organization_id = NULLIF(current_setting('app.org_id', true), '')::uuid);
CREATE POLICY tenant ON idempotency_keys
    USING (organization_id = NULLIF(current_setting('app.org_id', true), '')::uuid)
    WITH CHECK (organization_id = NULLIF(current_setting('app.org_id', true), '')::uuid);

-- ADR-0023: a user sees their own memberships, and the organizations they are a member of. SELECT only.
CREATE POLICY member_self ON memberships FOR SELECT
    USING (user_id = NULLIF(current_setting('app.user_id', true), '')::uuid);
CREATE POLICY member_orgs ON organizations FOR SELECT
    USING (EXISTS (SELECT 1 FROM memberships m
                   WHERE m.organization_id = organizations.id
                     AND m.user_id = NULLIF(current_setting('app.user_id', true), '')::uuid));

-- +goose StatementBegin
DO $$
DECLARE
  app   text := regexp_replace(current_user, '_migrator$', '_app');
  batch text := regexp_replace(current_user, '_migrator$', '_batch');
BEGIN
  -- Column-level UPDATE grants: the runtime role can change only what the use cases change.
  EXECUTE format('GRANT SELECT, INSERT ON users TO %I', app);
  EXECUTE format('GRANT UPDATE (email, display_name, email_verified, updated_at, last_seen_at) ON users TO %I', app);
  EXECUTE format('GRANT SELECT, INSERT ON organizations, projects, resources TO %I', app);
  EXECUTE format('GRANT UPDATE (name, updated_at, deleted_at) ON organizations TO %I', app);
  EXECUTE format('GRANT UPDATE (name, slug, description, updated_at, deleted_at) ON projects TO %I', app);
  EXECUTE format('GRANT UPDATE (name, labels, updated_at, deleted_at) ON resources TO %I', app);
  EXECUTE format('GRANT SELECT, INSERT, DELETE ON memberships TO %I', app);
  EXECUTE format('GRANT UPDATE (role, updated_at) ON memberships TO %I', app);
  EXECUTE format('GRANT SELECT, INSERT ON api_keys, api_key_index TO %I', app);
  EXECUTE format('GRANT UPDATE (revoked_at) ON api_keys, api_key_index TO %I', app);
  EXECUTE format('GRANT SELECT, INSERT ON audit_events, idempotency_keys, org_create_keys TO %I', app);
  -- An expired record (older than 24 h, not yet purged) is replaced in place.
  EXECUTE format('GRANT UPDATE (operation, request_hash, resource_id, created_at) ON idempotency_keys TO %I', app);
  EXECUTE format('GRANT UPDATE (organization_id, request_hash, created_at) ON org_create_keys TO %I', app);
  -- Purge job (ADR-0027): reads to select expired rows, deletes in batches.
  EXECUTE format('GRANT SELECT, DELETE ON users, organizations, projects, resources, api_keys, api_key_index, audit_events, idempotency_keys, org_create_keys TO %I', batch);
  EXECUTE format('GRANT SELECT, DELETE ON memberships TO %I', batch);
  -- Row locks (FOR UPDATE SKIP LOCKED in the user purge) need UPDATE on some column; grant the narrowest one.
  EXECUTE format('GRANT UPDATE (last_seen_at) ON users TO %I', batch);
END
$$;
-- +goose StatementEnd

-- +goose Down
DROP TABLE org_create_keys;
DROP TABLE idempotency_keys;
DROP TRIGGER api_keys_mirror_revocation ON api_keys;
DROP FUNCTION api_keys_mirror_revocation();
DROP TABLE audit_events;
DROP TABLE api_key_index;
DROP TABLE api_keys;
DROP TABLE resources;
DROP TABLE projects;
DROP TABLE memberships;
DROP TABLE organizations;
DROP TABLE users;
