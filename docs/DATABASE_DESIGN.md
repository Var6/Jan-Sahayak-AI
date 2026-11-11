# Database Design

Last updated 2026-11-09. Two databases:

1. **Device** — SQLite on every kiosk, owned by the Go device agent.
   Source of truth: `services/device-agent/internal/store/migrations/0001_init.sql`.
2. **Central** — PostgreSQL 17, owned by `services/central-api`.
   Source of truth: `services/central-api/internal/db/migrations/` (`0001_init.sql`,
   `0002_review_hardening.sql`) and the runtime grants in
   `services/central-api/internal/db/roles.go`.

Both hold **no personal data** in v1. Device events are anonymous by
construction (schemas in `internal/deviceapi`), and the central server rejects
any event type or field it does not know.

---

## 1. Device database (SQLite)

Opened by `internal/store` with WAL; outbox writes use `synchronous=FULL`.
All tables are `STRICT`. Timestamps are RFC 3339 UTC text.

| Table | Purpose | Key points |
|---|---|---|
| `device_meta` | Key/value runtime metadata (identity, versions). | `key` PK. |
| `sessions` | Anonymous session log. | `id` random UUIDv4; language, start/end, end_reason, turns. No names, phone numbers, audio or transcripts. Index on `started_at`. |
| `outbox_events` | Events waiting for the central server. | `id` UUID = idempotency key; `payload` must be valid JSON; `payload_sha256`; state machine below; `ack_receipt` = server receipt. Index `(state, next_attempt_at)`. |
| `schemes` | Local copy of the knowledge base from signed bundles. | One row per scheme (`id` PK) with status, category, names, full `record` JSON, `bundle_version`. |
| `schemes_fts` | FTS5 index over names/aliases/keywords (Hindi + English). | `unicode61`, diacritics kept (Devanagari matras matter). |

### Outbox state machine (enforced by triggers)

```
PENDING → UPLOADING → ACKNOWLEDGED → DELETE_ELIGIBLE → (deleted)
UPLOADING → PENDING (retry) | FAILED | CONFLICT
FAILED → PENDING, CONFLICT → PENDING (operator/automatic retry)
```

* `outbox_no_unsafe_delete` — a row can be deleted only in `DELETE_ELIGIBLE`.
  Unacknowledged events can never be deleted, whatever code runs.
* `outbox_legal_transition` — only the transitions above.
* `outbox_ack_requires_receipt` — `ACKNOWLEDGED` needs `ack_receipt` and `acked_at`.
* `outbox_payload_immutable` — id, type, payload, hash and created_at never change.

An event becomes `ACKNOWLEDGED` only after the device verifies the server's
Ed25519-signed ack (`syncproto.VerifyAck`) and the result is `accepted` or
`duplicate` with the device's own payload hash and a receipt. A `rejected`
result moves the row to `FAILED` (kept, never deleted); `conflict` to `CONFLICT`.

---

## 2. Central database (PostgreSQL)

Conventions: UUID primary keys (UUIDv7 generated in Go where the server creates
the id; device-generated ids for devices and events), `timestamptz` everywhere,
CHECK constraints on every enumerated or bounded column. Objects are created in
the connection's `search_path` schema (tests use a fresh schema per test).

### 2.1 Migrations

* Embedded SQL files `NNNN_name.sql`, applied by `internal/db.Migrate`
  (`central-admin migrate`).
* `schema_migrations(version PK, name, sha256, applied_at)`.
* One transaction per file (DDL + its `schema_migrations` row).
* Session advisory lock serialises concurrent runners.
* Applied files are checksum-verified; an edited migration is an error.
* A database newer than the binary is refused (`ErrNewerSchema`); the server
  also refuses to start unless the schema is exactly the expected version.

### 2.2 Tables

**Staff and access**

| Table | Columns (main) | Notes |
|---|---|---|
| `users` | id, username (unique, `^[a-z0-9][a-z0-9._-]{2,39}$`), display_name, password_hash (argon2id PHC string), disabled, failed_logins, locked_until, created_at, password_changed_at, last_login_at | Staff only — never citizens. **No account lock** (decision 7): `failed_logins` is informational and `locked_until` is no longer set. Login is throttled in memory instead (§2.7). |
| `roles` | name PK, description | Seeded: viewer, device_admin, scheme_editor, scheme_reviewer, auditor, admin. |
| `user_roles` | (user_id, role) PK, granted_at, granted_by | `granted_by` NULL = CLI. |
| `admin_sessions` | token_sha256 (bytea[32]) PK, user_id, created_at, last_seen_at, expires_at (absolute, 12 h), idle_expires_at (sliding, 30 min) | Only the SHA-256 of the cookie token is stored. Expired rows removed by the janitor. |
| `audit_logs` | id (UUIDv7), at, actor_user_id, actor, action (`noun.verb`), target_type, target_id, details jsonb | **Append-only**: row triggers block UPDATE/DELETE, a statement trigger blocks TRUNCATE, and the runtime role has only SELECT/INSERT and owns nothing, so it cannot disable the triggers (§2.6). Every admin and CLI mutation writes a row in the same transaction. Failed logins for unknown usernames are logged with actor `anonymous` and without the submitted name. |

**Devices**

| Table | Columns (main) | Notes |
|---|---|---|
| `devices` | id (device-generated UUID), name, district, public_key (bytea[32], unique), status (`active`/`revoked`), registered_at, registered_by, revoked_at, revoked_reason, last_seen_at | CHECK: revoked ⇔ revoked_at set. Revocation is permanent; a replacement registers a new identity. |
| `device_heartbeats` | id, device_id, received_at, device_time, agent_version, os_info, bundle_version, models jsonb, overall_health, components jsonb, outbox_* counts, outbox_oldest_pending_at, sync_last_success_at, sync_consecutive_failures, disk/mem bytes, cpu_temp_c, throttled, uptime_seconds | Software versions live here (no separate versions table). Bounded: ≥ 20 s between heartbeats per device; rows older than 30 days are deleted by the janitor except each device's latest row. |

**Sync ingest**

| Table | Columns | Notes |
|---|---|---|
| `sync_events` | id PK (= device event id), device_id, batch_id, type, schema_version, created_at (device clock), received_at, payload jsonb (object), payload_sha256 (hex of the original bytes), receipt uuid UNIQUE | Inserted with `ON CONFLICT (id) DO NOTHING`; a duplicate returns the stored receipt, a different hash is a conflict. UPDATE is blocked by trigger. Indexes `(type, created_at)`, `(device_id, received_at)`. |
| `sync_nonces` | (device_id, nonce) PK, expires_at | Replay protection, inserted only after the signature verifies. Expire at request ts + 10 min (2 × allowed skew); janitor deletes expired rows. |

Only `session.ended` v1 and `dialogue.turn` v1 are accepted; payloads are
strictly validated (unknown fields rejected, enumerations, bounded integers).

**Analytics (views, anonymous)**

| View | Derived from | Columns |
|---|---|---|
| `analytics_sessions` | `sync_events` type `session.ended` | session_id, language, started_at, ended_at, end_reason, turns, device_id, created_at |
| `analytics_turns` | `sync_events` type `dialogue.turn` | session_id, seq, language, source, intent, category, service, resolution, used_llm, device_id, created_at |

Views (not tables) keep a single source of truth. If volume grows, they can be
replaced by materialised daily rollups without changing the API.

**Small-cell suppression (decision 9).** `GET /v1/admin/analytics/summary`
returns counts from 1 to 4 as `null` for every role except `admin`, with
secondary suppression in additive groups (if exactly one cell of a group
would be hidden, the next smallest is hidden too) and `null` averages/rates
when their base is hidden. Decision 9 also requires session start/end times to
be rounded to the minute on the device before they are recorded (device-agent
side); the central API only ever returns aggregates, never event timestamps. The threshold (5) needs Janman and legal sign-off
under the DPDP Act.

**Knowledge base**

| Table | Columns | Notes |
|---|---|---|
| `schemes` | id (slug) PK, current_version, created_at, updated_at | `(id, current_version)` → `scheme_versions` FK, DEFERRABLE INITIALLY DEFERRED. |
| `scheme_versions` | (scheme_id, version) PK, record jsonb (`scheme.Record`), status, edited_by, edited_at, verified_by, verified_at, withdrawn_by, based_on_version, change_note | **Immutable** (UPDATE/DELETE blocked). Drafts are NEEDS_REVIEW. Verification appends a new VERIFIED version copied from the reviewed draft; withdrawal appends an OUTDATED copy of the live VERIFIED version (`edited_by` stays the content author, `withdrawn_by` = reviewer). CHECKs: VERIFIED ⇔ verified_by and verified_at; OUTDATED ⇔ withdrawn_by; `verified_by <> edited_by`; record id/version/status agree with the columns. Trigger `scheme_versions_four_eyes`: a VERIFIED row is refused if its reviewer edited **any** NEEDS_REVIEW/UNVERIFIED version of the scheme since the previous VERIFIED version (closes the A→B→A bypass; OUTDATED copies do not count). |
| `knowledge_bundles` | id, bundle_version (`YYYY-MM-DD.N`, unique), created_at, published_by, key_id, record_count, verified_count, contents jsonb ([{id, version, status}]), sha256, signed_json bytea | Immutable. `signed_json` is the exact byte string served to devices; its SHA-256 is re-checked on every download. Per scheme a bundle holds the latest VERIFIED version unless a later OUTDATED (withdrawal) version exists; otherwise the latest NEEDS_REVIEW version newer than the latest VERIFIED one (a pending correction); otherwise nothing. A withdrawn VERIFIED version is never shipped again; devices replace their whole scheme table per bundle, so a dropped scheme disappears from kiosks. |

### 2.3 Rules that the schema enforces

* Nothing becomes VERIFIED without a named human reviewer who edited no draft
  of that scheme since its last verified version.
* Audit log, scheme versions, bundles and sync events cannot be rewritten —
  by triggers, and by privileges: the runtime role can only SELECT/INSERT them.
* Every bounded input has a CHECK (lengths, enumerations, non-negative counts).
* Retention is explicit: nonces (10 min), admin sessions (12 h / 30 min idle),
  heartbeats (30 days, latest kept). Events, audit rows, scheme versions and
  bundles are kept indefinitely in v1.

### 2.4 Personal data — deferred

v1 stores no personal data centrally. **Citizen assistance records** (consented
personal data such as a callback number for a follow-up) are deferred. If added,
they will live in a **separate PostgreSQL schema** with: column-level encryption
using keys held outside the database, a dedicated role with no access from the
analytics or admin read paths, retention limits with automatic deletion,
consent records, and access logging in `audit_logs`. They must never be written
to `sync_events` or any table above.

### 2.5 Operations

* `central-admin migrate | grant-runtime | check-db-role | gen-server-key |
  server-public-key | create-user | grant-role | register-device |
  revoke-device | list-devices`.
* Dev: `scripts/dev/central.sh` (migrates the dev DB and runs the server on
  127.0.0.1:8750).
* Pool: max 10 connections, `statement_timeout` 15 s,
  `idle_in_transaction_session_timeout` 30 s, session `timezone=UTC`.

### 2.6 Database roles (decision 8)

Triggers alone do not make a table append-only: the table **owner** can
`ALTER TABLE … DISABLE TRIGGER` or `DROP TRIGGER`. Production therefore uses
two roles (created by `deployment/server/postgres-roles.sql`):

| Role | Owns | Used by |
|---|---|---|
| `jansahayak_owner` | database, schema `public`, every table, view and function | `central-admin migrate`, `central-admin grant-runtime`, `create-user`, `grant-role` |
| `jansahayak_app` | nothing; no CREATE on the schema; not a member of the owner role | `central-api` |

Runtime privileges (`db.RuntimeGrants`, applied by `central-admin grant-runtime
-role jansahayak_app` after every migration; it revokes anything else and
fails if a table has no entry):

| Table / view | Runtime privileges |
|---|---|
| `audit_logs`, `scheme_versions`, `knowledge_bundles`, `sync_events` | SELECT, INSERT |
| `users` | SELECT; UPDATE (failed_logins, locked_until, last_login_at) only |
| `roles`, `user_roles`, `schema_migrations` | SELECT |
| `admin_sessions` | SELECT, INSERT, DELETE; UPDATE (last_seen_at, idle_expires_at) |
| `devices` | SELECT, INSERT; UPDATE (status, revoked_at, revoked_reason, last_seen_at) — the public key is immutable |
| `device_heartbeats`, `sync_nonces` | SELECT, INSERT, DELETE (janitor retention) |
| `schemes` | SELECT, INSERT; UPDATE (current_version, updated_at) |
| `analytics_sessions`, `analytics_turns` | SELECT |

`central-api` checks its own role at startup (`db.CheckRuntimeRole`): superuser,
able to act as a table owner, CREATE on the schema, or UPDATE/DELETE/TRUNCATE/
TRIGGER on an append-only table is reported; with `-strict-db-role` /
`JS_STRICT_DB_ROLE=true` (production) the server refuses to start, otherwise it
logs a warning (local development uses one role). `central-admin check-db-role`
runs the same check. The server test suite runs every handler as a per-test
runtime role created with `GrantRuntime`, and `TestRuntimeRoleCannotTamper`
proves that role cannot update, delete, truncate or unprotect the append-only
tables. Shipping audit rows to an external store is a possible later step.

### 2.7 Admin login throttling (decision 7)

No account lock. Each attempt is delayed by the recent failures (15-minute
window) of its client IP and of the submitted username — whichever is larger:
none for the first 3, then 0.5 s doubling to 8 s at 10. Unknown usernames are
counted exactly like real ones, get the same 401 after the same argon2 work
and an audit row, so neither the answer nor its timing reveals which accounts
exist. At most 2 attempts per IP run at once; clients with no recent failures
have their own admission pool and a reserved argon2 slot, so a guessing flood
cannot starve real logins. State is in memory (bounded to 20 000 IPs and
20 000 usernames, entries expire after the window), so a restart clears it.
Behind the TLS proxy, `-trust-proxy` makes the IP key the proxy-set
`X-Forwarded-For` (IPv6 keyed by /64).

