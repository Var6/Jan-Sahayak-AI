// Command central-api is the Jan Sahayak central server: device sync
// ingest, heartbeats, knowledge bundles and the admin API.
//
// Configuration (flags override environment):
//
//	-addr          JS_ADDR           listen address (default 127.0.0.1:8750)
//	-database-url  JS_DATABASE_URL   PostgreSQL URL (required)
//	-server-key    JS_SERVER_KEY     active Ed25519 signing key file (required)
//	-verify-key    (repeatable)      retired public key files kept for verification
//	-cookie-secure JS_COOKIE_SECURE  Secure flag on admin cookie (default true)
//
// The database must already be migrated (`central-admin migrate`); the
// server refuses to start on a schema version it does not expect.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/janmanindia/jan-sahayak/services/central-api/internal/db"
	"github.com/janmanindia/jan-sahayak/services/central-api/internal/keys"
	"github.com/janmanindia/jan-sahayak/services/central-api/internal/server"
)

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(log); err != nil {
		log.Error("central-api: fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	secureDefault, _ := strconv.ParseBool(env("JS_COOKIE_SECURE", "true"))
	addr := flag.String("addr", env("JS_ADDR", "127.0.0.1:8750"), "listen address")
	dbURL := flag.String("database-url", env("JS_DATABASE_URL", ""), "PostgreSQL URL")
	keyPath := flag.String("server-key", env("JS_SERVER_KEY", ""), "server signing key file")
	cookieSecure := flag.Bool("cookie-secure", secureDefault, "set Secure on the admin session cookie")
	var verifyKeys multiFlag
	flag.Var(&verifyKeys, "verify-key", "retired server public key file (repeatable)")
	flag.Parse()
	if *dbURL == "" || *keyPath == "" {
		return errors.New("-database-url and -server-key are required")
	}

	signer, err := keys.LoadSigner(*keyPath)
	if err != nil {
		return err
	}
	var retired []keys.PublicKey
	for _, p := range verifyKeys {
		pk, err := keys.LoadPublic(p)
		if err != nil {
			return err
		}
		retired = append(retired, pk)
	}
	kr, err := keys.NewKeyring(signer, retired...)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pool, err := db.Open(ctx, *dbURL, db.Options{})
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := db.CheckSchema(ctx, pool); err != nil {
		return err
	}
	if !*cookieSecure {
		log.Warn("admin cookie Secure flag is OFF — use only for localhost development")
	}

	srv := server.New(server.Config{CookieSecure: *cookieSecure}, pool, kr, log)
	hs := &http.Server{
		Addr:              *addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		srv.RunJanitor(ctx, 10*time.Minute)
	}()
	errc := make(chan error, 1)
	go func() {
		log.Info("central-api listening", "addr", *addr, "key_id", signer.KeyID, "schema", db.LatestVersion())
		errc <- hs.ListenAndServe()
	}()
	select {
	case err := <-errc:
		stop()
		wg.Wait()
		return fmt.Errorf("listen: %w", err)
	case <-ctx.Done():
	}
	log.Info("central-api shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err = hs.Shutdown(sctx)
	wg.Wait()
	return err
}
