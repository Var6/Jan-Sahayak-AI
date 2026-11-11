package db

// Database roles (security review finding 4, decision 8).
//
// The append-only guarantees (audit_logs, scheme_versions, knowledge_bundles,
// sync_events) are triggers. A table's owner can ALTER TABLE ... DISABLE
// TRIGGER or DROP TRIGGER, so the role the API runs as must not own any
// table, nor be able to become the owner. Two roles are used:
//
//   - owner role (e.g. jansahayak_owner): owns the schema and every object;
//     used only by `central-admin migrate` and `central-admin grant-runtime`.
//   - runtime role (e.g. jansahayak_app): used by central-api. It gets the
//     privileges in RuntimeGrants and nothing else: SELECT/INSERT only on the
//     append-only tables, column-limited UPDATE elsewhere, no TRUNCATE, no
//     TRIGGER, no CREATE on the schema, no ownership.
//
// Deployment: deployment/server/postgres-roles.sql creates the roles;
// docs/DATABASE_DESIGN.md §2.6 describes the procedure.

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Grant is the privilege set of the runtime role on one table or view.
type Grant struct {
	Table string
	// Privileges are table-level privileges (SELECT, INSERT, DELETE).
	Privileges []string
	// UpdateColumns, if any, are the only columns the runtime may UPDATE.
	UpdateColumns []string
}

// AppendOnlyTables must never be writable beyond INSERT by the runtime role.
var AppendOnlyTables = []string{"audit_logs", "scheme_versions", "knowledge_bundles", "sync_events"}

// RuntimeGrants is the complete runtime privilege set. Every table and view
// created by the migrations must appear here (GrantRuntime fails otherwise),
// so a new migration cannot silently leave the runtime role with no or too
// many privileges.
var RuntimeGrants = []Grant{
	{Table: "schema_migrations", Privileges: []string{"SELECT"}},
	{Table: "users", Privileges: []string{"SELECT"}, UpdateColumns: []string{"failed_logins", "locked_until", "last_login_at"}},
	{Table: "roles", Privileges: []string{"SELECT"}},
	{Table: "user_roles", Privileges: []string{"SELECT"}},
	{Table: "admin_sessions", Privileges: []string{"SELECT", "INSERT", "DELETE"}, UpdateColumns: []string{"last_seen_at", "idle_expires_at"}},
	{Table: "audit_logs", Privileges: []string{"SELECT", "INSERT"}},
	{Table: "devices", Privileges: []string{"SELECT", "INSERT"}, UpdateColumns: []string{"status", "revoked_at", "revoked_reason", "last_seen_at"}},
	{Table: "device_heartbeats", Privileges: []string{"SELECT", "INSERT", "DELETE"}},
	{Table: "sync_events", Privileges: []string{"SELECT", "INSERT"}},
	{Table: "sync_nonces", Privileges: []string{"SELECT", "INSERT", "DELETE"}},
	{Table: "schemes", Privileges: []string{"SELECT", "INSERT"}, UpdateColumns: []string{"current_version", "updated_at"}},
	{Table: "scheme_versions", Privileges: []string{"SELECT", "INSERT"}},
	{Table: "knowledge_bundles", Privileges: []string{"SELECT", "INSERT"}},
	{Table: "analytics_sessions", Privileges: []string{"SELECT"}},
	{Table: "analytics_turns", Privileges: []string{"SELECT"}},
}

var roleNameRe = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// ErrRuntimeRole reports an unsafe runtime role choice.
var ErrRuntimeRole = errors.New("db: unsafe runtime role")

// GrantRuntime (re)applies RuntimeGrants to role on every table in the
// current schema, revoking anything else the role held there. Run it as
// the owner role after every migration; it is idempotent.
func GrantRuntime(ctx context.Context, pool *pgxpool.Pool, role string) error {
	if !roleNameRe.MatchString(role) {
		return fmt.Errorf("%w: role name %q must match %s", ErrRuntimeRole, role, roleNameRe)
	}
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var schema, me string
		var exists, super, actsAsMe bool
		if err := tx.QueryRow(ctx, `SELECT current_schema(), current_user,
				EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1),
				COALESCE((SELECT rolsuper FROM pg_roles WHERE rolname = $1), false)`, role).
			Scan(&schema, &me, &exists, &super); err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("%w: role %q does not exist (see deployment/server/postgres-roles.sql)", ErrRuntimeRole, role)
		}
		if role == me || super {
			return fmt.Errorf("%w: %q is the owner or a superuser", ErrRuntimeRole, role)
		}
		if err := tx.QueryRow(ctx, `SELECT pg_has_role($1, current_user, 'MEMBER')`, role).Scan(&actsAsMe); err != nil {
			return err
		}
		if actsAsMe {
			return fmt.Errorf("%w: %q is a member of the owner role %q and could act as owner", ErrRuntimeRole, role, me)
		}
		objs, err := schemaObjects(ctx, tx)
		if err != nil {
			return err
		}
		known := map[string]bool{}
		for _, g := range RuntimeGrants {
			known[g.Table] = true
		}
		var missing []string
		for _, o := range objs {
			if !known[o] {
				missing = append(missing, o)
			}
		}
		if len(missing) > 0 {
			return fmt.Errorf("db: tables without a runtime grant entry (update db.RuntimeGrants): %s", strings.Join(missing, ", "))
		}
		r := pgx.Identifier{role}.Sanitize()
		s := pgx.Identifier{schema}.Sanitize()
		stmts := []string{
			`REVOKE ALL ON ALL TABLES IN SCHEMA ` + s + ` FROM ` + r,
			`REVOKE CREATE ON SCHEMA ` + s + ` FROM ` + r,
			`GRANT USAGE ON SCHEMA ` + s + ` TO ` + r,
		}
		for _, g := range RuntimeGrants {
			if !slices.Contains(objs, g.Table) {
				return fmt.Errorf("db: runtime grant for unknown table %s", g.Table)
			}
			t := s + "." + pgx.Identifier{g.Table}.Sanitize()
			if len(g.Privileges) > 0 {
				stmts = append(stmts, `GRANT `+strings.Join(g.Privileges, ", ")+` ON `+t+` TO `+r)
			}
			if len(g.UpdateColumns) > 0 {
				cols := make([]string, len(g.UpdateColumns))
				for i, c := range g.UpdateColumns {
					cols[i] = pgx.Identifier{c}.Sanitize()
				}
				stmts = append(stmts, `GRANT UPDATE (`+strings.Join(cols, ", ")+`) ON `+t+` TO `+r)
			}
		}
		for _, q := range stmts {
			if _, err := tx.Exec(ctx, q); err != nil {
				return fmt.Errorf("db: %s: %w", q, err)
			}
		}
		return nil
	})
}

// schemaObjects lists tables and views in the current schema.
func schemaObjects(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = current_schema() AND c.relkind IN ('r', 'v', 'm', 'p') ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// CheckRuntimeRole reports why the connected role is NOT a safe runtime
// role: superuser, able to act as the owner of any table, CREATE on the
// schema, or more than INSERT/SELECT on an append-only table. An empty
// result means the role cannot rewrite or unprotect the append-only tables.
func CheckRuntimeRole(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	var problems []string
	var super, create bool
	if err := pool.QueryRow(ctx, `SELECT rolsuper, has_schema_privilege(current_user, current_schema(), 'CREATE')
		FROM pg_roles WHERE rolname = current_user`).Scan(&super, &create); err != nil {
		return nil, err
	}
	if super {
		return []string{"runtime role is a superuser"}, nil
	}
	if create {
		problems = append(problems, "runtime role has CREATE on the schema")
	}
	rows, err := pool.Query(ctx, `SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = current_schema() AND c.relkind IN ('r', 'v', 'm', 'p')
		  AND pg_has_role(current_user, c.relowner, 'MEMBER') ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	owned, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	for _, t := range owned {
		problems = append(problems, "runtime role owns (or can act as owner of) "+t)
	}
	for _, t := range AppendOnlyTables {
		for _, priv := range []string{"UPDATE", "DELETE", "TRUNCATE", "TRIGGER"} {
			var has bool
			err := pool.QueryRow(ctx, `SELECT COALESCE(has_table_privilege(current_user, to_regclass($1), $2), false)`, t, priv).Scan(&has)
			if err != nil {
				return nil, err
			}
			if has {
				problems = append(problems, fmt.Sprintf("runtime role has %s on append-only table %s", priv, t))
			}
		}
	}
	sort.Strings(problems)
	return problems, nil
}
