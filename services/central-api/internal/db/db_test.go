package db

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/goleak"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func freshSchema(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("JS_TEST_DATABASE_URL")
	explicit := url != ""
	if !explicit {
		url = "postgres://jansahayak@127.0.0.1:54329/jansahayak_test?sslmode=disable"
	}
	ctx := context.Background()
	admin, err := Open(ctx, url, Options{MaxConns: 1})
	if err != nil {
		if explicit {
			t.Fatal(err)
		}
		t.Skipf("test database unavailable: %v", err)
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	schema := "t_" + hex.EncodeToString(b)
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	pool, err := Open(ctx, url, Options{MaxConns: 4, Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
		admin.Close()
	})
	return pool
}

func TestMigrateIdempotentAndConcurrent(t *testing.T) {
	pool := freshSchema(t)
	ctx := context.Background()
	if err := CheckSchema(ctx, pool); err == nil {
		t.Fatal("unmigrated schema must fail CheckSchema")
	}
	// Two runners at once: the advisory lock serialises them; exactly one
	// applies each migration.
	var wg sync.WaitGroup
	applied := make([][]int, 2)
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			applied[i], errs[i] = Migrate(ctx, pool)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(applied[0])+len(applied[1]) != LatestVersion() {
		t.Fatalf("applied %v and %v", applied[0], applied[1])
	}
	again, err := Migrate(ctx, pool)
	if err != nil || len(again) != 0 {
		t.Fatalf("second run: %v %v", again, err)
	}
	if err := CheckSchema(ctx, pool); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateRefusesNewerSchemaAndEditedFiles(t *testing.T) {
	pool := freshSchema(t)
	ctx := context.Background()
	if _, err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE schema_migrations SET sha256 = 'x' WHERE version = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := Migrate(ctx, pool); !errors.Is(err, ErrChecksum) {
		t.Fatalf("edited migration: %v", err)
	}
	ms, _ := Migrations()
	if _, err := pool.Exec(ctx, `UPDATE schema_migrations SET sha256 = $1 WHERE version = 1`, ms[0].SHA256); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO schema_migrations (version, name, sha256) VALUES (999, 'future.sql', 'f')`); err != nil {
		t.Fatal(err)
	}
	if _, err := Migrate(ctx, pool); !errors.Is(err, ErrNewerSchema) {
		t.Fatalf("newer schema: %v", err)
	}
	if err := CheckSchema(ctx, pool); !errors.Is(err, ErrNewerSchema) {
		t.Fatalf("CheckSchema newer: %v", err)
	}
}

func TestGrantRuntime(t *testing.T) {
	pool := freshSchema(t)
	ctx := context.Background()
	if _, err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := GrantRuntime(ctx, pool, "Bad-Name"); !errors.Is(err, ErrRuntimeRole) {
		t.Fatalf("bad name: %v", err)
	}
	if err := GrantRuntime(ctx, pool, "js_no_such_role_x"); !errors.Is(err, ErrRuntimeRole) {
		t.Fatalf("missing role: %v", err)
	}
	var me string
	_ = pool.QueryRow(ctx, `SELECT current_user`).Scan(&me)
	if err := GrantRuntime(ctx, pool, me); !errors.Is(err, ErrRuntimeRole) {
		t.Fatalf("owner as runtime role: %v", err)
	}
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	role := "rt_db_" + hex.EncodeToString(b)
	if _, err := pool.Exec(ctx, `CREATE ROLE `+role+` NOLOGIN`); err != nil {
		t.Skipf("cannot create roles: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, `DROP OWNED BY `+role)
		_, _ = pool.Exec(bg, `DROP ROLE `+role)
	})
	for range 2 { // idempotent
		if err := GrantRuntime(ctx, pool, role); err != nil {
			t.Fatal(err)
		}
	}
	// Append-only tables: SELECT and INSERT only.
	for _, tbl := range AppendOnlyTables {
		for priv, want := range map[string]bool{"SELECT": true, "INSERT": true, "UPDATE": false, "DELETE": false, "TRUNCATE": false, "TRIGGER": false} {
			var has bool
			if err := pool.QueryRow(ctx, `SELECT has_table_privilege($1, to_regclass($2), $3)`, role, tbl, priv).Scan(&has); err != nil {
				t.Fatal(err)
			}
			if has != want {
				t.Errorf("%s %s: has=%v want %v", tbl, priv, has, want)
			}
		}
	}
	// A table added by a future migration without a grant entry is caught.
	if _, err := pool.Exec(ctx, `CREATE TABLE ungranted (id int)`); err != nil {
		t.Fatal(err)
	}
	if err := GrantRuntime(ctx, pool, role); err == nil || !strings.Contains(err.Error(), "ungranted") {
		t.Fatalf("ungranted table: %v", err)
	}
}

func TestRuntimeGrantsCoverAppendOnlyTables(t *testing.T) {
	for _, tbl := range AppendOnlyTables {
		found := false
		for _, g := range RuntimeGrants {
			if g.Table == tbl {
				found = true
				if len(g.UpdateColumns) > 0 || slices.ContainsFunc(g.Privileges, func(p string) bool { return p != "SELECT" && p != "INSERT" }) {
					t.Errorf("%s: runtime grant %v exceeds SELECT/INSERT", tbl, g)
				}
			}
		}
		if !found {
			t.Errorf("%s has no runtime grant", tbl)
		}
	}
}
