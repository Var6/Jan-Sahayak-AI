// Package core holds database operations shared by the HTTP server and the
// central-admin CLI: audit logging, staff users and roles, device registry.
// Every mutation writes its audit row in the same transaction.
package core

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/janmanindia/jan-sahayak/internal/ids"
	"github.com/janmanindia/jan-sahayak/services/central-api/internal/auth"
)

// Roles.
const (
	RoleViewer         = "viewer"
	RoleDeviceAdmin    = "device_admin"
	RoleSchemeEditor   = "scheme_editor"
	RoleSchemeReviewer = "scheme_reviewer"
	RoleAuditor        = "auditor"
	RoleAdmin          = "admin"
)

// AllRoles lists every role.
var AllRoles = []string{RoleViewer, RoleDeviceAdmin, RoleSchemeEditor, RoleSchemeReviewer, RoleAuditor, RoleAdmin}

// Errors.
var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
	ErrInvalid  = errors.New("invalid input")
)

// Actor identifies who performs a mutation.
type Actor struct {
	UserID string // empty for CLI/system
	Label  string // username, or "cli:<os user>", or "system"
}

func (a Actor) userID() any {
	if a.UserID == "" {
		return nil
	}
	return a.UserID
}

// Querier is satisfied by pgx.Tx and *pgxpool.Pool.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Audit writes one audit row. details must marshal to a JSON object.
func Audit(ctx context.Context, q Querier, a Actor, action, targetType, targetID string, details map[string]any) error {
	if details == nil {
		details = map[string]any{}
	}
	b, err := json.Marshal(details)
	if err != nil {
		return err
	}
	label := a.Label
	if label == "" {
		label = "system"
	}
	_, err = q.Exec(ctx, `INSERT INTO audit_logs (id, actor_user_id, actor, action, target_type, target_id, details)
		VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb)`, ids.NewV7(), a.userID(), label, action, targetType, targetID, string(b))
	return err
}

// InTx runs fn in a transaction.
func InTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func isUniqueViolation(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505"
}

var usernameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{2,39}$`)

// ValidateText checks a short single-line human string.
func ValidateText(field, s string, min, max int) error {
	n := utf8.RuneCountInString(s)
	if !utf8.ValidString(s) || n < min || n > max {
		return fmt.Errorf("%w: %s must be %d..%d characters", ErrInvalid, field, min, max)
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: %s contains control characters", ErrInvalid, field)
		}
	}
	return nil
}

// CreateUser creates a staff user.
func CreateUser(ctx context.Context, pool *pgxpool.Pool, a Actor, username, displayName, password string) (string, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	if !usernameRe.MatchString(username) {
		return "", fmt.Errorf("%w: username must match %s", ErrInvalid, usernameRe)
	}
	if err := ValidateText("display_name", displayName, 1, 100); err != nil {
		return "", err
	}
	if err := auth.CheckPasswordPolicy(password); err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	hash, err := auth.HashPassword(ctx, password)
	if err != nil {
		return "", err
	}
	id := ids.NewV7()
	err = InTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO users (id, username, display_name, password_hash) VALUES ($1, $2, $3, $4)`,
			id, username, displayName, hash); err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("%w: username exists", ErrConflict)
			}
			return err
		}
		return Audit(ctx, tx, a, "user.created", "user", id, map[string]any{"username": username})
	})
	return id, err
}

// GrantRole grants a role to a user (idempotent).
func GrantRole(ctx context.Context, pool *pgxpool.Pool, a Actor, username, role string) error {
	if !slices.Contains(AllRoles, role) {
		return fmt.Errorf("%w: unknown role %q", ErrInvalid, role)
	}
	return InTx(ctx, pool, func(tx pgx.Tx) error {
		var uid string
		err := tx.QueryRow(ctx, `SELECT id FROM users WHERE username = $1`, strings.ToLower(username)).Scan(&uid)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: user %q", ErrNotFound, username)
		}
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `INSERT INTO user_roles (user_id, role, granted_by) VALUES ($1, $2, $3)
			ON CONFLICT DO NOTHING`, uid, role, a.userID())
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		return Audit(ctx, tx, a, "user.role_granted", "user", uid, map[string]any{"username": username, "role": role})
	})
}

// Device is a registry entry.
type Device struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	District      string     `json:"district"`
	PublicKey     string     `json:"public_key"` // base64
	Status        string     `json:"status"`
	RegisteredAt  time.Time  `json:"registered_at"`
	RevokedAt     *time.Time `json:"revoked_at,omitempty"`
	RevokedReason string     `json:"revoked_reason,omitempty"`
	LastSeenAt    *time.Time `json:"last_seen_at,omitempty"`
}

// NewDevice is input for registration.
type NewDevice struct {
	ID        string `json:"id"`
	PublicKey string `json:"public_key"` // base64 (std) Ed25519 public key
	Name      string `json:"name"`
	District  string `json:"district"`
}

// RegisterDevice adds a device to the registry.
func RegisterDevice(ctx context.Context, pool *pgxpool.Pool, a Actor, d NewDevice) error {
	if ids.Validate(d.ID) != nil {
		return fmt.Errorf("%w: id must be a lowercase uuid", ErrInvalid)
	}
	pk, err := base64.StdEncoding.DecodeString(strings.TrimSpace(d.PublicKey))
	if err != nil || len(pk) != ed25519.PublicKeySize {
		return fmt.Errorf("%w: public_key must be base64 of a 32-byte Ed25519 key", ErrInvalid)
	}
	if err := ValidateText("name", d.Name, 1, 100); err != nil {
		return err
	}
	if err := ValidateText("district", d.District, 1, 60); err != nil {
		return err
	}
	return InTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO devices (id, name, district, public_key, registered_by)
			VALUES ($1, $2, $3, $4, $5)`, d.ID, d.Name, d.District, pk, a.userID()); err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("%w: device id or public key already registered", ErrConflict)
			}
			return err
		}
		return Audit(ctx, tx, a, "device.registered", "device", d.ID, map[string]any{"name": d.Name, "district": d.District})
	})
}

// RevokeDevice revokes a device key permanently. Its stored events remain;
// it can no longer upload. A replacement device must register a new identity.
func RevokeDevice(ctx context.Context, pool *pgxpool.Pool, a Actor, id, reason string) error {
	if ids.Validate(id) != nil {
		return fmt.Errorf("%w: id must be a uuid", ErrInvalid)
	}
	if err := ValidateText("reason", reason, 1, 200); err != nil {
		return err
	}
	return InTx(ctx, pool, func(tx pgx.Tx) error {
		var status string
		err := tx.QueryRow(ctx, `SELECT status FROM devices WHERE id = $1 FOR UPDATE`, id).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if status == "revoked" {
			return fmt.Errorf("%w: already revoked", ErrConflict)
		}
		if _, err := tx.Exec(ctx, `UPDATE devices SET status = 'revoked', revoked_at = now(), revoked_reason = $2 WHERE id = $1`,
			id, reason); err != nil {
			return err
		}
		// Outstanding nonces are useless now; remove them.
		if _, err := tx.Exec(ctx, `DELETE FROM sync_nonces WHERE device_id = $1`, id); err != nil {
			return err
		}
		return Audit(ctx, tx, a, "device.revoked", "device", id, map[string]any{"reason": reason})
	})
}

// HeartbeatSummary is the subset of the latest heartbeat shown in lists.
type HeartbeatSummary struct {
	ReceivedAt     time.Time `json:"received_at"`
	AgentVersion   string    `json:"agent_version"`
	BundleVersion  string    `json:"bundle_version"`
	OverallHealth  string    `json:"overall_health"`
	OutboxPending  int       `json:"outbox_pending"`
	OutboxFailed   int       `json:"outbox_failed"`
	OutboxConflict int       `json:"outbox_conflict"`
}

// DeviceListItem is a device with its latest heartbeat summary.
type DeviceListItem struct {
	Device
	LastHeartbeat *HeartbeatSummary `json:"last_heartbeat,omitempty"`
}

// MaxDeviceList bounds ListDevices.
const MaxDeviceList = 1000

const deviceCols = `d.id, d.name, d.district, d.public_key, d.status, d.registered_at, d.revoked_at,
	COALESCE(d.revoked_reason, ''), d.last_seen_at`

func scanDevice(row interface{ Scan(...any) error }, d *Device, extra ...any) error {
	var pk []byte
	dest := append([]any{&d.ID, &d.Name, &d.District, &pk, &d.Status, &d.RegisteredAt, &d.RevokedAt, &d.RevokedReason, &d.LastSeenAt}, extra...)
	if err := row.Scan(dest...); err != nil {
		return err
	}
	d.PublicKey = base64.StdEncoding.EncodeToString(pk)
	return nil
}

// ListDevices returns up to MaxDeviceList devices ordered by district, name.
func ListDevices(ctx context.Context, q Querier) ([]DeviceListItem, error) {
	rows, err := q.Query(ctx, `SELECT `+deviceCols+`,
		h.received_at, h.agent_version, h.bundle_version, h.overall_health, h.outbox_pending, h.outbox_failed, h.outbox_conflict
		FROM devices d
		LEFT JOIN LATERAL (SELECT * FROM device_heartbeats hb WHERE hb.device_id = d.id
		                   ORDER BY hb.received_at DESC LIMIT 1) h ON true
		ORDER BY d.district, d.name, d.id LIMIT $1`, MaxDeviceList)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DeviceListItem{}
	for rows.Next() {
		var it DeviceListItem
		var recv *time.Time
		var av, bv, oh *string
		var op, of, oc *int
		if err := scanDevice(rows, &it.Device, &recv, &av, &bv, &oh, &op, &of, &oc); err != nil {
			return nil, err
		}
		if recv != nil {
			it.LastHeartbeat = &HeartbeatSummary{ReceivedAt: *recv, AgentVersion: *av, BundleVersion: *bv,
				OverallHealth: *oh, OutboxPending: *op, OutboxFailed: *of, OutboxConflict: *oc}
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// GetDevice returns one device.
func GetDevice(ctx context.Context, q Querier, id string) (Device, error) {
	var d Device
	if ids.Validate(id) != nil {
		return d, ErrNotFound
	}
	err := scanDevice(q.QueryRow(ctx, `SELECT `+deviceCols+` FROM devices d WHERE d.id = $1`, id), &d)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, ErrNotFound
	}
	return d, err
}
