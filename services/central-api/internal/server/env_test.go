package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/goleak"

	"github.com/janmanindia/jan-sahayak/internal/deviceapi"
	"github.com/janmanindia/jan-sahayak/internal/ids"
	"github.com/janmanindia/jan-sahayak/internal/syncproto"
	"github.com/janmanindia/jan-sahayak/services/central-api/internal/core"
	"github.com/janmanindia/jan-sahayak/services/central-api/internal/db"
	"github.com/janmanindia/jan-sahayak/services/central-api/internal/keys"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

const defaultTestDB = "postgres://jansahayak@127.0.0.1:54329/jansahayak_test?sslmode=disable"

type env struct {
	t      testing.TB
	pool   *pgxpool.Pool
	srv    *Server
	hs     *httptest.Server
	signer keys.Signer
	kr     *keys.Keyring
	client *http.Client
	schema string
	// rtRole is the least-privilege runtime role the server's pool runs as
	// ("" if the test database user cannot create roles; the server then
	// runs as the owner).
	rtRole string
	rtPool *pgxpool.Pool
}

// newEnv creates a fresh schema in the test database, migrates it as the
// owner, creates a per-test runtime role with db.GrantRuntime, and starts
// the server on an httptest listener with its pool running as that role —
// so every handler test also proves the runtime grants suffice. e.pool
// stays the owner (fixtures, direct assertions). Everything is torn down
// (schema and role dropped) when the test ends.
func newEnv(t testing.TB) *env {
	t.Helper()
	url := os.Getenv("JS_TEST_DATABASE_URL")
	explicit := url != ""
	if !explicit {
		url = defaultTestDB
	}
	ctx := context.Background()
	admin, err := db.Open(ctx, url, db.Options{MaxConns: 1})
	if err != nil {
		if explicit {
			t.Fatalf("test database: %v", err)
		}
		t.Skipf("test database unavailable (%v); set JS_TEST_DATABASE_URL", err)
	}
	sb := make([]byte, 6)
	_, _ = rand.Read(sb)
	schema := "t_" + hex.EncodeToString(sb)
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	pool, err := db.Open(ctx, url, db.Options{MaxConns: 6, Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	rtRole, rtPool := "rt_"+schema[2:], pool
	if _, err := admin.Exec(ctx, `CREATE ROLE `+rtRole+` NOLOGIN`); err != nil {
		t.Logf("cannot create runtime role (%v); server runs as the owner", err)
		rtRole = ""
	} else {
		if err := db.GrantRuntime(ctx, pool, rtRole); err != nil {
			t.Fatal(err)
		}
		if rtPool, err = db.Open(ctx, url, db.Options{MaxConns: 6, Schema: schema, Role: rtRole}); err != nil {
			t.Fatal(err)
		}
	}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := keys.Signer{KeyID: "test-k1", Priv: priv}
	kr, err := keys.NewKeyring(signer)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	// Small login delays keep throttling tests fast; the shape is the same.
	srv, err := New(Config{CookieSecure: false, LoginBaseDelay: 2 * time.Millisecond, LoginMaxDelay: 20 * time.Millisecond},
		rtPool, kr, log)
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(srv.Handler())
	tr := &http.Transport{MaxIdleConns: 4, IdleConnTimeout: time.Second}
	e := &env{t: t, pool: pool, srv: srv, hs: hs, signer: signer, kr: kr, schema: schema, rtRole: rtRole, rtPool: rtPool,
		client: &http.Client{Transport: tr, Timeout: 30 * time.Second}}
	t.Cleanup(func() {
		tr.CloseIdleConnections()
		hs.Close()
		if rtPool != pool {
			rtPool.Close()
		}
		pool.Close()
		bg := context.Background()
		if _, err := admin.Exec(bg, `DROP SCHEMA `+schema+` CASCADE`); err != nil {
			t.Errorf("drop schema: %v", err)
		}
		if rtRole != "" {
			if _, err := admin.Exec(bg, `DROP OWNED BY `+rtRole); err != nil {
				t.Errorf("drop owned: %v", err)
			}
			if _, err := admin.Exec(bg, `DROP ROLE `+rtRole); err != nil {
				t.Errorf("drop role: %v", err)
			}
		}
		admin.Close()
	})
	return e
}

// device is a simulated kiosk identity.
type device struct {
	id   string
	priv ed25519.PrivateKey
}

func (e *env) newDevice(name string) device {
	e.t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	d := device{id: ids.NewV4(), priv: priv}
	err := core.RegisterDevice(context.Background(), e.pool, core.Actor{Label: "test"}, core.NewDevice{
		ID: d.id, PublicKey: base64.StdEncoding.EncodeToString(pub), Name: name, District: "TestDistrict"})
	if err != nil {
		e.t.Fatal(err)
	}
	return d
}

// mkEvent builds a wire event the way the device outbox does.
func mkEvent(typ string, payload any) syncproto.Event {
	b, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return syncproto.Event{ID: ids.NewV7(), Type: typ, SchemaVersion: 1,
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), Payload: b, PayloadSHA256: syncproto.PayloadHash(b)}
}

func sessionEnded(sessionID string, lang string, turns int) deviceapi.SessionEnded {
	now := time.Now().UTC()
	return deviceapi.SessionEnded{SessionID: sessionID, Language: lang,
		StartedAt: now.Add(-3 * time.Minute).Format(time.RFC3339), EndedAt: now.Format(time.RFC3339),
		EndReason: "citizen_ended", Turns: turns}
}

func turn(sessionID string, seq int, category, resolution string) deviceapi.Turn {
	return deviceapi.Turn{SessionID: sessionID, Seq: seq, Language: "hi", Source: "voice",
		Intent: "scheme_info", Category: category, Resolution: resolution}
}

func (d device) batch(evs ...syncproto.Event) syncproto.BatchRequest {
	return syncproto.BatchRequest{BatchID: ids.NewV7(), DeviceID: d.id,
		SentAt: time.Now().UTC().Format(time.RFC3339), Events: evs}
}

type signedReq struct {
	method, path string
	body         []byte
	ts           int64
	nonce        string
	sig          string
	deviceHeader string
}

// sign prepares headers exactly like the device syncer does.
func (d device) sign(method, path string, body []byte, now time.Time) signedReq {
	nonce, _ := deviceapi.NewNonce()
	ts := now.Unix()
	return signedReq{method: method, path: path, body: body, ts: ts, nonce: nonce, deviceHeader: d.id,
		sig: syncproto.SignRequest(d.priv, method, path, ts, nonce, body)}
}

func (e *env) do(sr signedReq) (int, []byte) {
	e.t.Helper()
	req, err := http.NewRequest(sr.method, e.hs.URL+sr.path, bytes.NewReader(sr.body))
	if err != nil {
		e.t.Fatal(err)
	}
	if sr.body == nil {
		req.Body = nil
		req.ContentLength = 0
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(syncproto.HeaderDevice, sr.deviceHeader)
	req.Header.Set(syncproto.HeaderTS, strconv.FormatInt(sr.ts, 10))
	req.Header.Set(syncproto.HeaderNonce, sr.nonce)
	req.Header.Set(syncproto.HeaderSig, sr.sig)
	resp, err := e.client.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	return resp.StatusCode, b
}

// upload signs and posts a batch; returns status, raw body.
func (e *env) upload(d device, br syncproto.BatchRequest) (int, []byte) {
	e.t.Helper()
	body, err := json.Marshal(br)
	if err != nil {
		e.t.Fatal(err)
	}
	return e.do(d.sign(http.MethodPost, syncproto.Path, body, time.Now()))
}

// uploadOK uploads and verifies the ack exactly as the device does.
func (e *env) uploadOK(d device, br syncproto.BatchRequest) map[string]syncproto.Result {
	e.t.Helper()
	code, body := e.upload(d, br)
	if code != http.StatusOK {
		e.t.Fatalf("upload: HTTP %d: %s", code, body)
	}
	var ack syncproto.BatchAck
	if err := json.Unmarshal(body, &ack); err != nil {
		e.t.Fatal(err)
	}
	res, err := syncproto.VerifyAck(e.kr.VerifyKeys(), br, ack)
	if err != nil {
		e.t.Fatalf("VerifyAck: %v", err)
	}
	if len(res) != len(br.Events) {
		e.t.Fatalf("ack has %d results for %d events", len(res), len(br.Events))
	}
	return res
}

func (e *env) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// ---- admin helpers ----

func (e *env) createUser(username string, roles ...string) string {
	e.t.Helper()
	ctx := context.Background()
	a := core.Actor{Label: "test"}
	id, err := core.CreateUser(ctx, e.pool, a, username, "Test "+username, "correct-horse-"+username)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, r := range roles {
		if err := core.GrantRole(ctx, e.pool, a, username, r); err != nil {
			e.t.Fatal(err)
		}
	}
	return id
}

type adminClient struct {
	e *env
	c *http.Client
}

func (e *env) adminClient() *adminClient {
	jar, _ := cookiejar.New(nil)
	return &adminClient{e: e, c: &http.Client{Transport: e.client.Transport, Jar: jar, Timeout: 30 * time.Second}}
}

func (a *adminClient) req(method, path string, body any, hdr ...string) (int, []byte, *http.Response) {
	a.e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			a.e.t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, a.e.hs.URL+path, rd)
	if err != nil {
		a.e.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := a.c.Do(req)
	if err != nil {
		a.e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	return resp.StatusCode, b, resp
}

func (a *adminClient) login(username string) {
	a.e.t.Helper()
	code, b, _ := a.req("POST", "/v1/admin/login", map[string]string{"username": username, "password": "correct-horse-" + username})
	if code != http.StatusOK {
		a.e.t.Fatalf("login %s: %d %s", username, code, b)
	}
}

func mustJSON(t *testing.T, b []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
}

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}
