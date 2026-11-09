// Command central-admin is the operator CLI for the central API.
//
//	central-admin migrate
//	central-admin gen-server-key -out FILE
//	central-admin server-public-key -key FILE
//	central-admin create-user -username U -display-name NAME   (password on stdin)
//	central-admin grant-role -username U -role ROLE
//	central-admin register-device -id UUID -public-key BASE64 -name NAME -district DISTRICT
//	central-admin revoke-device -id UUID -reason TEXT
//	central-admin list-devices
//
// Database commands read -database-url or JS_DATABASE_URL. Every mutation
// is written to the audit log with actor "cli:<os user>".
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/user"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/janmanindia/jan-sahayak/services/central-api/internal/core"
	"github.com/janmanindia/jan-sahayak/services/central-api/internal/db"
	"github.com/janmanindia/jan-sahayak/services/central-api/internal/keys"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "central-admin:", err)
		os.Exit(1)
	}
}

const usage = `usage: central-admin <command> [flags]
commands: migrate, gen-server-key, server-public-key, create-user, grant-role,
          register-device, revoke-device, list-devices`

func cliActor() core.Actor {
	name := "unknown"
	if u, err := user.Current(); err == nil && u.Username != "" {
		name = u.Username
	}
	return core.Actor{Label: "cli:" + name}
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	cmd, rest := args[0], args[1:]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	dbURL := fs.String("database-url", os.Getenv("JS_DATABASE_URL"), "PostgreSQL URL")
	schema := fs.String("schema", "", "search_path schema (tests)")
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	openDB := func() (*pgxpool.Pool, error) {
		if *dbURL == "" {
			return nil, errors.New("-database-url or JS_DATABASE_URL required")
		}
		return db.Open(ctx, *dbURL, db.Options{MaxConns: 2, Schema: *schema, StatementTimeout: time.Minute})
	}

	switch cmd {
	case "migrate":
		if err := fs.Parse(rest); err != nil {
			return err
		}
		pool, err := openDB()
		if err != nil {
			return err
		}
		defer pool.Close()
		done, err := db.Migrate(ctx, pool)
		if err != nil {
			return err
		}
		v, _ := db.SchemaVersion(ctx, pool)
		fmt.Fprintf(stdout, "applied %v; schema version %d\n", done, v)
		return nil

	case "gen-server-key":
		out := fs.String("out", "", "key file to create (must not exist)")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if *out == "" {
			return errors.New("-out required")
		}
		s, err := keys.Generate(*out, time.Now())
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "wrote %s (mode 0600)\n", *out)
		_, err = stdout.Write(keys.MarshalPublic(s.Public()))
		return err

	case "server-public-key":
		kp := fs.String("key", "", "server key file")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		s, err := keys.LoadSigner(*kp)
		if err != nil {
			return err
		}
		_, err = stdout.Write(keys.MarshalPublic(s.Public()))
		return err

	case "create-user":
		username := fs.String("username", "", "login name")
		display := fs.String("display-name", "", "display name")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		pw, err := readPassword(stdin)
		if err != nil {
			return err
		}
		pool, err := openDB()
		if err != nil {
			return err
		}
		defer pool.Close()
		if *display == "" {
			*display = *username
		}
		id, err := core.CreateUser(ctx, pool, cliActor(), *username, *display, pw)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "created user %s id=%s\n", strings.ToLower(*username), id)
		return nil

	case "grant-role":
		username := fs.String("username", "", "login name")
		role := fs.String("role", "", strings.Join(core.AllRoles, "|"))
		if err := fs.Parse(rest); err != nil {
			return err
		}
		pool, err := openDB()
		if err != nil {
			return err
		}
		defer pool.Close()
		if err := core.GrantRole(ctx, pool, cliActor(), *username, *role); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "granted %s to %s\n", *role, *username)
		return nil

	case "register-device":
		var d core.NewDevice
		fs.StringVar(&d.ID, "id", "", "device id (uuid from the device identity file)")
		fs.StringVar(&d.PublicKey, "public-key", "", "device Ed25519 public key, base64")
		fs.StringVar(&d.Name, "name", "", "device name")
		fs.StringVar(&d.District, "district", "", "district")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		pool, err := openDB()
		if err != nil {
			return err
		}
		defer pool.Close()
		if err := core.RegisterDevice(ctx, pool, cliActor(), d); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "registered device %s\n", d.ID)
		return nil

	case "revoke-device":
		id := fs.String("id", "", "device id")
		reason := fs.String("reason", "", "reason")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		pool, err := openDB()
		if err != nil {
			return err
		}
		defer pool.Close()
		if err := core.RevokeDevice(ctx, pool, cliActor(), *id, *reason); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "revoked device %s\n", *id)
		return nil

	case "list-devices":
		asJSON := fs.Bool("json", false, "print JSON")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		pool, err := openDB()
		if err != nil {
			return err
		}
		defer pool.Close()
		list, err := core.ListDevices(ctx, pool)
		if err != nil {
			return err
		}
		if *asJSON {
			enc := json.NewEncoder(stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(list)
		}
		tw := tabwriter.NewWriter(stdout, 2, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tNAME\tDISTRICT\tSTATUS\tLAST SEEN\tHEALTH\tPENDING")
		for _, d := range list {
			seen, health, pending := "-", "-", "-"
			if d.LastSeenAt != nil {
				seen = d.LastSeenAt.UTC().Format(time.RFC3339)
			}
			if d.LastHeartbeat != nil {
				health = d.LastHeartbeat.OverallHealth
				pending = fmt.Sprint(d.LastHeartbeat.OutboxPending)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", d.ID, d.Name, d.District, d.Status, seen, health, pending)
		}
		return tw.Flush()

	default:
		return errors.New(usage)
	}
}

// readPassword reads the first line of stdin (no echo handling: pipe it in,
// e.g. from a password manager). The line ending is stripped.
func readPassword(r io.Reader) (string, error) {
	br := bufio.NewReaderSize(io.LimitReader(r, 4096), 4096)
	line, err := br.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return "", errors.New("password must be provided on stdin")
	}
	return line, nil
}
