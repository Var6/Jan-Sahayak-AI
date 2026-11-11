-- Jan Sahayak central database: owner and runtime roles (decision 8).
--
-- Run ONCE as a PostgreSQL superuser:
--   sudo -u postgres psql -v ON_ERROR_STOP=1 -f postgres-roles.sql
-- then set passwords interactively (never on a command line):
--   sudo -u postgres psql -c '\password jansahayak_owner'
--   sudo -u postgres psql -c '\password jansahayak_app'
--
-- jansahayak_owner owns the database, the schema and every table. It is used
-- ONLY for `central-admin migrate` and `central-admin grant-runtime`.
-- jansahayak_app is what central-api logs in as. It owns nothing, cannot
-- create objects, and gets exactly db.RuntimeGrants (SELECT/INSERT only on
-- audit_logs, scheme_versions, knowledge_bundles and sync_events), so it
-- cannot disable or drop the append-only triggers.

CREATE ROLE jansahayak_owner LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
CREATE ROLE jansahayak_app   LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS
    NOINHERIT CONNECTION LIMIT 30;

CREATE DATABASE jansahayak OWNER jansahayak_owner ENCODING 'UTF8' TEMPLATE template0;
REVOKE ALL ON DATABASE jansahayak FROM PUBLIC;
GRANT CONNECT ON DATABASE jansahayak TO jansahayak_owner, jansahayak_app;

\connect jansahayak
-- Objects are created in schema public (search_path). Only the owner may
-- create there; PUBLIC (and therefore jansahayak_app) may not.
ALTER SCHEMA public OWNER TO jansahayak_owner;
REVOKE ALL ON SCHEMA public FROM PUBLIC;
GRANT USAGE ON SCHEMA public TO jansahayak_app;

-- Statement limits for the runtime role (the server also sets these per
-- connection; this is a backstop for ad-hoc sessions).
ALTER ROLE jansahayak_app SET statement_timeout = '15s';
ALTER ROLE jansahayak_app SET idle_in_transaction_session_timeout = '30s';
