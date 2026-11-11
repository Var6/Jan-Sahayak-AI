# Central server deployment (example)

Files:

| File | Purpose |
|---|---|
| `postgres-roles.sql` | Creates `jansahayak_owner` (owns everything; migrations only) and `jansahayak_app` (runtime; least privilege). |
| `Caddyfile` | TLS termination and reverse proxy to `127.0.0.1:8750`. |
| `jan-sahayak-central.service` | Hardened systemd unit for `central-api`. |
| `central.env.example` | Environment for the unit (runtime role URL, `JS_TRUST_PROXY`, `JS_STRICT_DB_ROLE`). |

Procedure (first install and every upgrade):

1. Once: `sudo -u postgres psql -v ON_ERROR_STOP=1 -f postgres-roles.sql`,
   then set both passwords with `\password`.
2. Every release, **as the owner role**:
   ```
   central-admin migrate       -database-url postgres://jansahayak_owner@127.0.0.1/jansahayak
   central-admin grant-runtime -database-url postgres://jansahayak_owner@127.0.0.1/jansahayak -role jansahayak_app
   ```
   `grant-runtime` is idempotent; it revokes everything else the runtime role
   held and fails if a migration added a table without an entry in
   `db.RuntimeGrants`.
3. Check, **as the runtime role**:
   `central-admin check-db-role -database-url postgres://jansahayak_app@127.0.0.1/jansahayak`
4. Start `central-api` with `JS_STRICT_DB_ROLE=true`: it refuses to start
   if its role could act as a table owner or modify an append-only table.
5. Caddy in front, `JS_TRUST_PROXY=true`, `JS_COOKIE_SECURE=true`.

Staff accounts are created with `central-admin create-user` / `grant-role`
using the owner URL (the runtime role cannot insert users or roles).

See `docs/DATABASE_DESIGN.md` §2.6 for the privilege matrix.
