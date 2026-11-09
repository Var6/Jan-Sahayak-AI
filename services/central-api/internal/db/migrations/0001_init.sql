-- Jan Sahayak central database, schema version 1.
-- Applied in one transaction by internal/db.Migrate. Objects are created in
-- the connection's current schema (search_path), never schema-qualified.
--
-- PERSONAL DATA: none. Device events are anonymous by construction (see
-- internal/deviceapi). Consented citizen assistance records are DEFERRED; if
-- added they go into a separate, encrypted, access-restricted schema — never
-- into these tables.

-- Generic guard used by append-only / immutable tables.
CREATE FUNCTION js_reject_modification() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'table % is append-only: % is not allowed', TG_TABLE_NAME, TG_OP
        USING ERRCODE = 'insufficient_privilege';
END;
$$;

-------------------------------------------------------------------------------
-- Staff users, roles, sessions
-------------------------------------------------------------------------------
CREATE TABLE users (
    id                  uuid PRIMARY KEY,
    username            text NOT NULL CHECK (username ~ '^[a-z0-9][a-z0-9._-]{2,39}$'),
    display_name        text NOT NULL CHECK (length(display_name) BETWEEN 1 AND 100),
    password_hash       text NOT NULL CHECK (password_hash LIKE '$argon2id$%'),
    disabled            boolean NOT NULL DEFAULT false,
    failed_logins       integer NOT NULL DEFAULT 0 CHECK (failed_logins >= 0),
    locked_until        timestamptz,
    created_at          timestamptz NOT NULL DEFAULT now(),
    password_changed_at timestamptz NOT NULL DEFAULT now(),
    last_login_at       timestamptz
);
CREATE UNIQUE INDEX users_username_uq ON users (username);

CREATE TABLE roles (
    name        text PRIMARY KEY,
    description text NOT NULL
);
INSERT INTO roles (name, description) VALUES
    ('viewer',          'Read analytics, devices, schemes and bundles'),
    ('device_admin',    'Register and revoke devices'),
    ('scheme_editor',   'Create and edit scheme drafts (always NEEDS_REVIEW)'),
    ('scheme_reviewer', 'Verify scheme versions edited by someone else; publish bundles'),
    ('auditor',         'Read the audit log'),
    ('admin',           'All permissions');

CREATE TABLE user_roles (
    user_id    uuid NOT NULL REFERENCES users (id),
    role       text NOT NULL REFERENCES roles (name),
    granted_at timestamptz NOT NULL DEFAULT now(),
    granted_by uuid REFERENCES users (id),          -- NULL = granted via CLI
    PRIMARY KEY (user_id, role)
);

-- Server-side admin sessions. Only the SHA-256 of the cookie token is stored.
CREATE TABLE admin_sessions (
    token_sha256    bytea PRIMARY KEY CHECK (length(token_sha256) = 32),
    user_id         uuid NOT NULL REFERENCES users (id),
    created_at      timestamptz NOT NULL DEFAULT now(),
    last_seen_at    timestamptz NOT NULL DEFAULT now(),
    expires_at      timestamptz NOT NULL,         -- absolute lifetime
    idle_expires_at timestamptz NOT NULL          -- sliding inactivity limit
);
CREATE INDEX admin_sessions_user ON admin_sessions (user_id);
CREATE INDEX admin_sessions_expiry ON admin_sessions (expires_at);

-- Append-only audit log. UPDATE, DELETE and TRUNCATE are blocked by triggers.
CREATE TABLE audit_logs (
    id            uuid PRIMARY KEY,                 -- UUIDv7, time ordered
    at            timestamptz NOT NULL DEFAULT clock_timestamp(),
    actor_user_id uuid REFERENCES users (id),       -- NULL for CLI / system
    actor         text NOT NULL CHECK (length(actor) BETWEEN 1 AND 100),
    action        text NOT NULL CHECK (action ~ '^[a-z_]+(\.[a-z_]+)+$'),
    target_type   text NOT NULL DEFAULT '' CHECK (length(target_type) <= 40),
    target_id     text NOT NULL DEFAULT '' CHECK (length(target_id) <= 100),
    details       jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(details) = 'object')
);
CREATE INDEX audit_logs_target ON audit_logs (target_type, target_id);
CREATE TRIGGER audit_logs_no_update_delete BEFORE UPDATE OR DELETE ON audit_logs
    FOR EACH ROW EXECUTE FUNCTION js_reject_modification();
CREATE TRIGGER audit_logs_no_truncate BEFORE TRUNCATE ON audit_logs
    FOR EACH STATEMENT EXECUTE FUNCTION js_reject_modification();

-------------------------------------------------------------------------------
-- Devices
-------------------------------------------------------------------------------
CREATE TABLE devices (
    id             uuid PRIMARY KEY,                -- generated on the device
    name           text NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
    district       text NOT NULL CHECK (length(district) BETWEEN 1 AND 60),
    public_key     bytea NOT NULL CHECK (length(public_key) = 32),   -- Ed25519
    status         text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'revoked')),
    registered_at  timestamptz NOT NULL DEFAULT now(),
    registered_by  uuid REFERENCES users (id),      -- NULL = CLI provisioning
    revoked_at     timestamptz,
    revoked_reason text CHECK (length(revoked_reason) <= 200),
    last_seen_at   timestamptz,
    CHECK ((status = 'revoked') = (revoked_at IS NOT NULL))
);
CREATE UNIQUE INDEX devices_public_key_uq ON devices (public_key);

-- Health snapshots. Retention: rows older than the retention window are
-- deleted by the server janitor, except the latest row per device.
CREATE TABLE device_heartbeats (
    id                        uuid PRIMARY KEY,
    device_id                 uuid NOT NULL REFERENCES devices (id),
    received_at               timestamptz NOT NULL DEFAULT now(),
    device_time               timestamptz NOT NULL,
    agent_version             text NOT NULL,
    os_info                   text NOT NULL DEFAULT '',
    bundle_version            text NOT NULL DEFAULT '',
    models                    jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(models) = 'object'),
    overall_health            text NOT NULL CHECK (overall_health IN
                              ('HEALTHY','DEGRADED','UNAVAILABLE','RECOVERING','REQUIRES_ATTENTION')),
    components                jsonb NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(components) = 'array'),
    outbox_pending            integer NOT NULL CHECK (outbox_pending >= 0),
    outbox_uploading          integer NOT NULL CHECK (outbox_uploading >= 0),
    outbox_acknowledged       integer NOT NULL CHECK (outbox_acknowledged >= 0),
    outbox_delete_eligible    integer NOT NULL CHECK (outbox_delete_eligible >= 0),
    outbox_failed             integer NOT NULL CHECK (outbox_failed >= 0),
    outbox_conflict           integer NOT NULL CHECK (outbox_conflict >= 0),
    outbox_total              integer NOT NULL CHECK (outbox_total >= 0),
    outbox_capacity           integer NOT NULL CHECK (outbox_capacity >= 0),
    outbox_oldest_pending_at  timestamptz,
    sync_last_success_at      timestamptz,
    sync_consecutive_failures integer NOT NULL DEFAULT 0 CHECK (sync_consecutive_failures >= 0),
    disk_free_bytes           bigint CHECK (disk_free_bytes >= 0),
    disk_total_bytes          bigint CHECK (disk_total_bytes >= 0),
    mem_available_bytes       bigint CHECK (mem_available_bytes >= 0),
    mem_total_bytes           bigint CHECK (mem_total_bytes >= 0),
    cpu_temp_c                real,
    throttled                 boolean,
    uptime_seconds            bigint CHECK (uptime_seconds >= 0)
);
CREATE INDEX device_heartbeats_device_received ON device_heartbeats (device_id, received_at DESC);
CREATE INDEX device_heartbeats_received ON device_heartbeats (received_at);

-------------------------------------------------------------------------------
-- Sync ingest
-------------------------------------------------------------------------------
-- One row per device outbox event. id is the device's event id (idempotency
-- key). Rows are immutable: UPDATE is blocked.
CREATE TABLE sync_events (
    id             uuid PRIMARY KEY,
    device_id      uuid NOT NULL REFERENCES devices (id),
    batch_id       uuid NOT NULL,
    type           text NOT NULL CHECK (length(type) BETWEEN 1 AND 64),
    schema_version integer NOT NULL CHECK (schema_version >= 1),
    created_at     timestamptz NOT NULL,                      -- device clock
    received_at    timestamptz NOT NULL DEFAULT now(),
    payload        jsonb NOT NULL CHECK (jsonb_typeof(payload) = 'object'),
    payload_sha256 text NOT NULL CHECK (payload_sha256 ~ '^[0-9a-f]{64}$'),  -- of the original bytes
    receipt        uuid NOT NULL UNIQUE
);
CREATE INDEX sync_events_type_created ON sync_events (type, created_at);
CREATE INDEX sync_events_device_received ON sync_events (device_id, received_at);
CREATE TRIGGER sync_events_immutable BEFORE UPDATE ON sync_events
    FOR EACH ROW EXECUTE FUNCTION js_reject_modification();

-- Replay protection. A (device, nonce) pair is accepted once; rows expire
-- after twice the allowed clock skew and are removed by the janitor.
CREATE TABLE sync_nonces (
    device_id  uuid NOT NULL REFERENCES devices (id),
    nonce      text NOT NULL CHECK (nonce ~ '^[A-Za-z0-9_-]{16,64}$'),
    expires_at timestamptz NOT NULL,
    PRIMARY KEY (device_id, nonce)
);
CREATE INDEX sync_nonces_expiry ON sync_nonces (expires_at);

-- Anonymous analytics derived from events. Payloads were validated against
-- internal/deviceapi schemas at ingest, so the casts below cannot fail.
CREATE VIEW analytics_sessions AS
SELECT e.id                                   AS event_id,
       e.device_id,
       (e.payload ->> 'session_id')::uuid     AS session_id,
       e.payload ->> 'language'               AS language,
       (e.payload ->> 'started_at')::timestamptz AS started_at,
       (e.payload ->> 'ended_at')::timestamptz   AS ended_at,
       e.payload ->> 'end_reason'             AS end_reason,
       (e.payload ->> 'turns')::integer       AS turns,
       e.created_at,
       e.received_at
FROM sync_events e
WHERE e.type = 'session.ended' AND e.schema_version = 1;

CREATE VIEW analytics_turns AS
SELECT e.id                                   AS event_id,
       e.device_id,
       (e.payload ->> 'session_id')::uuid     AS session_id,
       (e.payload ->> 'seq')::integer         AS seq,
       e.payload ->> 'language'               AS language,
       e.payload ->> 'source'                 AS source,
       e.payload ->> 'intent'                 AS intent,
       COALESCE(e.payload ->> 'category', '')   AS category,
       COALESCE(e.payload ->> 'service', '')    AS service,
       COALESCE(e.payload ->> 'resolution', '') AS resolution,
       COALESCE((e.payload ->> 'used_llm')::boolean, false) AS used_llm,
       e.created_at,
       e.received_at
FROM sync_events e
WHERE e.type = 'dialogue.turn' AND e.schema_version = 1;

-------------------------------------------------------------------------------
-- Scheme knowledge base
-------------------------------------------------------------------------------
CREATE TABLE schemes (
    id              text PRIMARY KEY CHECK (id ~ '^[a-z0-9]+(-[a-z0-9]+)*$' AND length(id) <= 80),
    current_version integer NOT NULL CHECK (current_version >= 1),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

-- Immutable versions. Verification never edits a row: it appends a new
-- VERIFIED version (based_on_version = the reviewed draft). The four-eyes
-- rule is enforced here too: verified_by <> edited_by.
CREATE TABLE scheme_versions (
    scheme_id        text NOT NULL REFERENCES schemes (id),
    version          integer NOT NULL CHECK (version >= 1),
    record           jsonb NOT NULL CHECK (jsonb_typeof(record) = 'object'),
    status           text NOT NULL CHECK (status IN ('VERIFIED','NEEDS_REVIEW','OUTDATED','UNVERIFIED')),
    edited_by        uuid NOT NULL REFERENCES users (id),   -- author of the content
    edited_at        timestamptz NOT NULL DEFAULT now(),
    verified_by      uuid REFERENCES users (id),
    verified_at      timestamptz,
    based_on_version integer,
    change_note      text NOT NULL DEFAULT '' CHECK (length(change_note) <= 500),
    PRIMARY KEY (scheme_id, version),
    CHECK ((status = 'VERIFIED') = (verified_by IS NOT NULL AND verified_at IS NOT NULL)),
    CHECK (verified_by IS NULL OR verified_by <> edited_by),
    CHECK (record ->> 'id' = scheme_id),
    CHECK ((record ->> 'version')::integer = version),
    CHECK (record -> 'verification' ->> 'status' = status)
);
CREATE INDEX scheme_versions_status ON scheme_versions (scheme_id, status, version DESC);
CREATE TRIGGER scheme_versions_immutable BEFORE UPDATE OR DELETE ON scheme_versions
    FOR EACH ROW EXECUTE FUNCTION js_reject_modification();

ALTER TABLE schemes ADD CONSTRAINT schemes_current_version_fk
    FOREIGN KEY (id, current_version) REFERENCES scheme_versions (scheme_id, version)
    DEFERRABLE INITIALLY DEFERRED;

-- Published, signed bundles. signed_json is the exact byte string served to
-- devices; sha256 guards it against silent corruption.
CREATE TABLE knowledge_bundles (
    id             uuid PRIMARY KEY,
    bundle_version text NOT NULL UNIQUE CHECK (bundle_version ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}\.[0-9]+$'),
    created_at     timestamptz NOT NULL DEFAULT now(),
    published_by   uuid NOT NULL REFERENCES users (id),
    key_id         text NOT NULL,
    record_count   integer NOT NULL CHECK (record_count >= 0),
    verified_count integer NOT NULL CHECK (verified_count >= 0),
    contents       jsonb NOT NULL CHECK (jsonb_typeof(contents) = 'array'),  -- [{id, version, status}]
    sha256         text NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    signed_json    bytea NOT NULL
);
CREATE INDEX knowledge_bundles_created ON knowledge_bundles (created_at DESC);
CREATE TRIGGER knowledge_bundles_immutable BEFORE UPDATE OR DELETE ON knowledge_bundles
    FOR EACH ROW EXECUTE FUNCTION js_reject_modification();
