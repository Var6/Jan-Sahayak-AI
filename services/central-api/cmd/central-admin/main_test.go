package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/janmanindia/jan-sahayak/internal/ids"
	"github.com/janmanindia/jan-sahayak/services/central-api/internal/db"
)

func TestCLI(t *testing.T) {
	url := os.Getenv("JS_TEST_DATABASE_URL")
	explicit := url != ""
	if !explicit {
		url = "postgres://jansahayak@127.0.0.1:54329/jansahayak_test?sslmode=disable"
	}
	ctx := context.Background()
	admin, err := db.Open(ctx, url, db.Options{MaxConns: 1})
	if err != nil {
		if explicit {
			t.Fatal(err)
		}
		t.Skipf("test database unavailable: %v", err)
	}
	defer admin.Close()
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	schema := "t_" + hex.EncodeToString(b)
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)

	runCmd := func(stdin string, args ...string) (string, error) {
		var out bytes.Buffer
		full := append([]string{args[0], "-database-url", url, "-schema", schema}, args[1:]...)
		if args[0] == "gen-server-key" || args[0] == "server-public-key" {
			full = args
		}
		err := run(ctx, full, strings.NewReader(stdin), &out)
		return out.String(), err
	}
	if out, err := runCmd("", "migrate"); err != nil || !strings.Contains(out, fmt.Sprintf("schema version %d", db.LatestVersion())) {
		t.Fatalf("migrate: %q %v", out, err)
	}
	if out, err := runCmd("", "migrate"); err != nil || !strings.Contains(out, "applied []") {
		t.Fatalf("migrate again: %q %v", out, err)
	}
	if _, err := runCmd("", "create-user", "-username", "ops1"); err == nil {
		t.Fatal("create-user without stdin password succeeded")
	}
	if _, err := runCmd("tiny\n", "create-user", "-username", "ops1"); err == nil {
		t.Fatal("weak password accepted")
	}
	if out, err := runCmd("a-very-long-password\n", "create-user", "-username", "Ops1", "-display-name", "Ops One"); err != nil || !strings.Contains(out, "created user ops1") {
		t.Fatalf("create-user: %q %v", out, err)
	}
	if _, err := runCmd("", "grant-role", "-username", "ops1", "-role", "superuser"); err == nil {
		t.Fatal("unknown role accepted")
	}
	if _, err := runCmd("", "grant-role", "-username", "ops1", "-role", "admin"); err != nil {
		t.Fatal(err)
	}
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	devID := ids.NewV4()
	if _, err := runCmd("", "register-device", "-id", devID, "-public-key", base64.StdEncoding.EncodeToString(pub),
		"-name", "Kiosk A", "-district", "TestDistrict"); err != nil {
		t.Fatal(err)
	}
	out, err := runCmd("", "list-devices")
	if err != nil || !strings.Contains(out, devID) || !strings.Contains(out, "active") {
		t.Fatalf("list-devices: %q %v", out, err)
	}
	// Owner vs runtime role.
	if _, err := runCmd("", "check-db-role"); err == nil {
		t.Fatal("check-db-role accepted the owner (superuser) role")
	}
	role := "rt_cli_" + schema[2:]
	if _, err := admin.Exec(ctx, `CREATE ROLE `+role+` NOLOGIN`); err == nil {
		defer func() {
			_, _ = admin.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
			_, _ = admin.Exec(context.Background(), `DROP OWNED BY `+role)
			_, _ = admin.Exec(context.Background(), `DROP ROLE `+role)
		}()
		if out, err := runCmd("", "grant-runtime", "-role", role); err != nil || !strings.Contains(out, "granted runtime privileges") {
			t.Fatalf("grant-runtime: %q %v", out, err)
		}
	}
	if _, err := runCmd("", "grant-runtime", "-role", "js_no_such_role_y"); err == nil {
		t.Fatal("grant-runtime to a missing role succeeded")
	}
	var n int
	_ = admin.QueryRow(ctx, `SELECT count(*) FROM `+schema+`.audit_logs WHERE actor LIKE 'cli:%'`).Scan(&n)
	if n < 3 {
		t.Fatalf("CLI mutations not audited (%d rows)", n)
	}

	kp := filepath.Join(t.TempDir(), "server.key")
	if out, err := runCmd("", "gen-server-key", "-out", kp); err != nil || !strings.Contains(out, `"public_key"`) {
		t.Fatalf("gen-server-key: %q %v", out, err)
	}
	if out, err := runCmd("", "server-public-key", "-key", kp); err != nil || !strings.Contains(out, `"key_id": "srv-`) {
		t.Fatalf("server-public-key: %q %v", out, err)
	}
}
