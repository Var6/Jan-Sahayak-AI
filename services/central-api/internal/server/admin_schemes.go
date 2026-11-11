package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/janmanindia/jan-sahayak/internal/deviceapi"
	"github.com/janmanindia/jan-sahayak/internal/ids"
	"github.com/janmanindia/jan-sahayak/internal/scheme"
	"github.com/janmanindia/jan-sahayak/services/central-api/internal/core"
)

// ist is India Standard Time (no DST), used for calendar dates.
var ist = time.FixedZone("IST", 5*3600+1800)

const (
	maxHistory       = 100
	maxBundleRecords = 5000
	bundleLockKey    = int64(0x4a53_424e_444c) // "JSBNDL"
)

type schemeListItem struct {
	ID                    string    `json:"id"`
	CurrentVersion        int       `json:"current_version"`
	Status                string    `json:"status"`
	Kind                  string    `json:"kind"`
	Category              string    `json:"category"`
	NameHi                string    `json:"name_hi"`
	NameEn                string    `json:"name_en"`
	UpdatedAt             time.Time `json:"updated_at"`
	LatestVerifiedVersion *int      `json:"latest_verified_version,omitempty"`
	// Withdrawn is true when the latest VERIFIED version has been marked
	// OUTDATED by a later withdrawal; bundles no longer carry it.
	Withdrawn bool `json:"withdrawn"`
}

func (s *Server) handleListSchemes(w http.ResponseWriter, r *http.Request) {
	rows, err := s.pool.Query(r.Context(), `SELECT s.id, s.current_version, v.status, v.record->>'kind', v.record->>'category',
		v.record->'name'->>'hi', v.record->'name'->>'en', s.updated_at,
		lv.v, COALESCE(lo.v > COALESCE(lv.v, 0), false)
		FROM schemes s JOIN scheme_versions v ON v.scheme_id = s.id AND v.version = s.current_version
		LEFT JOIN LATERAL (SELECT max(x.version) AS v FROM scheme_versions x WHERE x.scheme_id = s.id AND x.status = 'VERIFIED') lv ON true
		LEFT JOIN LATERAL (SELECT max(x.version) AS v FROM scheme_versions x WHERE x.scheme_id = s.id AND x.status = 'OUTDATED') lo ON true
		ORDER BY s.id LIMIT $1`, maxBundleRecords)
	if err != nil {
		s.writeCoreError(w, err)
		return
	}
	defer rows.Close()
	out := []schemeListItem{}
	for rows.Next() {
		var it schemeListItem
		if err := rows.Scan(&it.ID, &it.CurrentVersion, &it.Status, &it.Kind, &it.Category, &it.NameHi, &it.NameEn,
			&it.UpdatedAt, &it.LatestVerifiedVersion, &it.Withdrawn); err != nil {
			s.writeCoreError(w, err)
			return
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		s.writeCoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"schemes": out})
}

type versionMeta struct {
	Version        int        `json:"version"`
	Status         string     `json:"status"`
	EditedBy       string     `json:"edited_by"` // username
	EditedAt       time.Time  `json:"edited_at"`
	VerifiedBy     string     `json:"verified_by,omitempty"`
	VerifiedAt     *time.Time `json:"verified_at,omitempty"`
	WithdrawnBy    string     `json:"withdrawn_by,omitempty"` // username, OUTDATED versions only
	BasedOnVersion *int       `json:"based_on_version,omitempty"`
	ChangeNote     string     `json:"change_note,omitempty"`
}

// versionMetaCols needs the joins in versionMetaJoins.
const versionMetaCols = `v.version, v.status, eu.username, v.edited_at, COALESCE(vu.username, ''), v.verified_at,
	COALESCE(wu.username, ''), v.based_on_version, v.change_note`

const versionMetaJoins = ` JOIN users eu ON eu.id = v.edited_by LEFT JOIN users vu ON vu.id = v.verified_by
	LEFT JOIN users wu ON wu.id = v.withdrawn_by `

func scanVersionMeta(row interface{ Scan(...any) error }, m *versionMeta, extra ...any) error {
	return row.Scan(append([]any{&m.Version, &m.Status, &m.EditedBy, &m.EditedAt, &m.VerifiedBy, &m.VerifiedAt,
		&m.WithdrawnBy, &m.BasedOnVersion, &m.ChangeNote}, extra...)...)
}

func (s *Server) getVersion(ctx context.Context, id string, version int) (versionMeta, json.RawMessage, error) {
	var m versionMeta
	var rec string
	q := `SELECT ` + versionMetaCols + `, v.record::text FROM scheme_versions v` + versionMetaJoins +
		`WHERE v.scheme_id = $1 AND v.version = `
	var err error
	if version > 0 {
		err = scanVersionMeta(s.pool.QueryRow(ctx, q+`$2`, id, version), &m, &rec)
	} else {
		err = scanVersionMeta(s.pool.QueryRow(ctx, q+`(SELECT current_version FROM schemes WHERE id = $1)`, id), &m, &rec)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return m, nil, core.ErrNotFound
	}
	return m, json.RawMessage(rec), err
}

func (s *Server) handleGetScheme(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	meta, rec, err := s.getVersion(ctx, id, 0)
	if err != nil {
		s.writeCoreError(w, err)
		return
	}
	rows, err := s.pool.Query(ctx, `SELECT `+versionMetaCols+` FROM scheme_versions v`+versionMetaJoins+
		`WHERE v.scheme_id = $1 ORDER BY v.version DESC LIMIT $2`, id, maxHistory)
	if err != nil {
		s.writeCoreError(w, err)
		return
	}
	defer rows.Close()
	hist := []versionMeta{}
	for rows.Next() {
		var m versionMeta
		if err := scanVersionMeta(rows, &m); err != nil {
			s.writeCoreError(w, err)
			return
		}
		hist = append(hist, m)
	}
	if err := rows.Err(); err != nil {
		s.writeCoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "current": meta, "record": rec, "history": hist})
}

func (s *Server) handleGetSchemeVersion(w http.ResponseWriter, r *http.Request) {
	v, err := strconv.Atoi(r.PathValue("version"))
	if err != nil || v < 1 {
		writeError(w, http.StatusBadRequest, "bad_request", "version must be a positive integer")
		return
	}
	meta, rec, err := s.getVersion(r.Context(), r.PathValue("id"), v)
	if err != nil {
		s.writeCoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": r.PathValue("id"), "version": meta, "record": rec})
}

type putSchemeReq struct {
	// BaseVersion is the version the editor started from (0 for a new
	// scheme). A mismatch with the current version returns 409.
	BaseVersion int           `json:"base_version"`
	ChangeNote  string        `json:"change_note"`
	Record      scheme.Record `json:"record"`
}

// handlePutScheme saves a new draft version. Whatever status the client
// sends, the stored status is NEEDS_REVIEW and verification fields are
// cleared: only POST .../verify by a different human can set VERIFIED.
func (s *Server) handlePutScheme(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := principalFrom(r)
	id := r.PathValue("id")
	var req putSchemeReq
	if !decodeAdmin(w, r, &req) {
		return
	}
	if len(id) > 80 {
		writeError(w, http.StatusBadRequest, "bad_request", "id too long")
		return
	}
	if req.Record.ID == "" {
		req.Record.ID = id
	}
	if req.Record.ID != id {
		writeError(w, http.StatusBadRequest, "bad_request", "record.id must equal the path id")
		return
	}
	if err := core.ValidateText("change_note", req.ChangeNote, 0, 500); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	var newVersion int
	err := core.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		var cur int
		err := tx.QueryRow(ctx, `SELECT current_version FROM schemes WHERE id = $1 FOR UPDATE`, id).Scan(&cur)
		exists := err == nil
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if req.BaseVersion != cur {
			return fmt.Errorf("%w: base_version %d but current version is %d", core.ErrConflict, req.BaseVersion, cur)
		}
		newVersion = cur + 1
		rec := req.Record
		rec.Version = newVersion
		rec.Verification = scheme.Verification{Status: scheme.NeedsReview, Notes: rec.Verification.Notes}
		rec.LastVerified = ""
		if err := rec.Validate(); err != nil {
			return &validationError{err}
		}
		b, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		if !exists {
			if _, err := tx.Exec(ctx, `INSERT INTO schemes (id, current_version) VALUES ($1, $2)`, id, newVersion); err != nil {
				return err
			}
		} else if _, err := tx.Exec(ctx, `UPDATE schemes SET current_version = $2, updated_at = now() WHERE id = $1`, id, newVersion); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO scheme_versions (scheme_id, version, record, status, edited_by, based_on_version, change_note)
			VALUES ($1, $2, $3::jsonb, $4, $5, $6, $7)`, id, newVersion, string(b), string(scheme.NeedsReview), p.UserID,
			nullIfZero(cur), req.ChangeNote); err != nil {
			return err
		}
		return core.Audit(ctx, tx, p.actor(), "scheme.draft_saved", "scheme", id,
			map[string]any{"version": newVersion, "base_version": cur, "change_note": req.ChangeNote})
	})
	var ve *validationError
	if errors.As(err, &ve) {
		writeError(w, http.StatusUnprocessableEntity, "invalid_record", "record failed validation", splitErrors(ve.err)...)
		return
	}
	if err != nil {
		s.writeCoreError(w, err)
		return
	}
	meta, rec, err := s.getVersion(ctx, id, newVersion)
	if err != nil {
		s.writeCoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "version": meta, "record": rec})
}

type validationError struct{ err error }

func (v *validationError) Error() string { return v.err.Error() }

func splitErrors(err error) []string {
	var out []string
	for _, l := range strings.Split(err.Error(), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

func nullIfZero(v int) any {
	if v == 0 {
		return nil
	}
	return v
}

type verifyReq struct {
	Version   int    `json:"version"`              // the NEEDS_REVIEW version being approved
	Notes     string `json:"notes,omitempty"`      // reviewer notes
	ReviewDue string `json:"review_due,omitempty"` // YYYY-MM-DD, optional
}

// handleVerifyScheme records a human verification. It appends a new
// VERIFIED version copied from the reviewed draft. Four-eyes rule: the
// reviewer must not have edited ANY draft of this scheme since its last
// VERIFIED version — not just the reviewed one — because a later draft by
// someone else still carries the earlier author's content. Also enforced in
// the database (CHECK verified_by <> edited_by and trigger
// scheme_versions_four_eyes).
func (s *Server) handleVerifyScheme(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := principalFrom(r)
	id := r.PathValue("id")
	var req verifyReq
	if !decodeAdmin(w, r, &req) {
		return
	}
	if err := core.ValidateText("notes", req.Notes, 0, 500); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	now := s.now()
	if req.ReviewDue != "" {
		due, err := time.ParseInLocation("2006-01-02", req.ReviewDue, ist)
		if err != nil || !due.After(now) {
			writeError(w, http.StatusBadRequest, "bad_request", "review_due must be a future date YYYY-MM-DD")
			return
		}
	}
	var newVersion int
	err := core.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		var cur int
		err := tx.QueryRow(ctx, `SELECT current_version FROM schemes WHERE id = $1 FOR UPDATE`, id).Scan(&cur)
		if errors.Is(err, pgx.ErrNoRows) {
			return core.ErrNotFound
		}
		if err != nil {
			return err
		}
		if req.Version != cur {
			return fmt.Errorf("%w: version %d is not the current version (%d)", core.ErrConflict, req.Version, cur)
		}
		var status, editedBy, recJSON string
		if err := tx.QueryRow(ctx, `SELECT status, edited_by::text, record::text FROM scheme_versions WHERE scheme_id = $1 AND version = $2`,
			id, cur).Scan(&status, &editedBy, &recJSON); err != nil {
			return err
		}
		if status != string(scheme.NeedsReview) {
			return fmt.Errorf("%w: version %d has status %s, only NEEDS_REVIEW can be verified", core.ErrConflict, cur, status)
		}
		if editedBy == p.UserID {
			return errOwnEdit
		}
		var ownEdit bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM scheme_versions
			WHERE scheme_id = $1 AND edited_by = $2 AND status IN ('NEEDS_REVIEW', 'UNVERIFIED')
			  AND version > COALESCE((SELECT max(version) FROM scheme_versions WHERE scheme_id = $1 AND status = 'VERIFIED'), 0))`,
			id, p.UserID).Scan(&ownEdit); err != nil {
			return err
		}
		if ownEdit {
			return errOwnEdit
		}
		var rec scheme.Record
		if err := json.Unmarshal([]byte(recJSON), &rec); err != nil {
			return err
		}
		newVersion = cur + 1
		rec.Version = newVersion
		rec.Verification = scheme.Verification{
			Status:     scheme.Verified,
			VerifiedBy: p.UserID, // the reviewer's user id (UUID); names are resolved via users
			VerifiedAt: now.UTC().Format(time.RFC3339),
			ReviewDue:  req.ReviewDue,
			Notes:      req.Notes,
		}
		rec.LastVerified = now.In(ist).Format("2006-01-02")
		if err := rec.Validate(); err != nil {
			return &validationError{err}
		}
		b, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO scheme_versions (scheme_id, version, record, status, edited_by, verified_by,
			verified_at, based_on_version, change_note) VALUES ($1, $2, $3::jsonb, 'VERIFIED', $4, $5, $6, $7, $8)`,
			id, newVersion, string(b), editedBy, p.UserID, now, cur, "verified: "+trunc(req.Notes, 480)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE schemes SET current_version = $2, updated_at = now() WHERE id = $1`, id, newVersion); err != nil {
			return err
		}
		return core.Audit(ctx, tx, p.actor(), "scheme.verified", "scheme", id,
			map[string]any{"reviewed_version": cur, "verified_version": newVersion, "editor_user_id": editedBy,
				"review_due": req.ReviewDue, "notes": req.Notes})
	})
	var ve *validationError
	switch {
	case errors.Is(err, errOwnEdit):
		writeError(w, http.StatusForbidden, "own_edit",
			"a reviewer cannot verify a scheme they edited since its last verified version")
		return
	case errors.As(err, &ve):
		writeError(w, http.StatusUnprocessableEntity, "invalid_record", "record failed validation", splitErrors(ve.err)...)
		return
	case err != nil:
		s.writeCoreError(w, err)
		return
	}
	meta, rec, err := s.getVersion(ctx, id, newVersion)
	if err != nil {
		s.writeCoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "version": meta, "record": rec})
}

var errOwnEdit = errors.New("reviewer edited this scheme since its last verified version")

type withdrawReq struct {
	Version int    `json:"version"` // the current version (optimistic concurrency)
	Reason  string `json:"reason"`  // why the verified facts are no longer valid
}

// handleWithdrawScheme marks the live VERIFIED version OUTDATED. Versions
// are immutable, so it appends a new OUTDATED version that copies the
// withdrawn record (edited_by stays its content author; withdrawn_by is the
// reviewer). From then on bundles never carry that VERIFIED version: they
// carry a pending NEEDS_REVIEW correction if there is one (shown on devices
// only as "not yet verified"), else nothing for the scheme. Withdrawal is a
// safety action, so any reviewer may do it, including the content author.
func (s *Server) handleWithdrawScheme(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := principalFrom(r)
	id := r.PathValue("id")
	var req withdrawReq
	if !decodeAdmin(w, r, &req) {
		return
	}
	if err := core.ValidateText("reason", strings.TrimSpace(req.Reason), 3, 400); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	reason := strings.TrimSpace(req.Reason)
	var newVersion, withdrawn int
	err := core.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		var cur int
		err := tx.QueryRow(ctx, `SELECT current_version FROM schemes WHERE id = $1 FOR UPDATE`, id).Scan(&cur)
		if errors.Is(err, pgx.ErrNoRows) {
			return core.ErrNotFound
		}
		if err != nil {
			return err
		}
		if req.Version != cur {
			return fmt.Errorf("%w: version %d is not the current version (%d)", core.ErrConflict, req.Version, cur)
		}
		var lv, lo *int
		if err := tx.QueryRow(ctx, `SELECT max(version) FILTER (WHERE status = 'VERIFIED'),
			max(version) FILTER (WHERE status = 'OUTDATED') FROM scheme_versions WHERE scheme_id = $1`, id).Scan(&lv, &lo); err != nil {
			return err
		}
		if lv == nil || (lo != nil && *lo > *lv) {
			return fmt.Errorf("%w: scheme has no live VERIFIED version to withdraw", core.ErrConflict)
		}
		withdrawn = *lv
		var editedBy, recJSON string
		if err := tx.QueryRow(ctx, `SELECT edited_by::text, record::text FROM scheme_versions WHERE scheme_id = $1 AND version = $2`,
			id, withdrawn).Scan(&editedBy, &recJSON); err != nil {
			return err
		}
		var rec scheme.Record
		if err := json.Unmarshal([]byte(recJSON), &rec); err != nil {
			return err
		}
		newVersion = cur + 1
		rec.Version = newVersion
		rec.Verification = scheme.Verification{Status: scheme.Outdated, Notes: trunc("OUTDATED: "+reason, 500)}
		rec.LastVerified = ""
		if err := rec.Validate(); err != nil {
			return &validationError{err}
		}
		b, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO scheme_versions (scheme_id, version, record, status, edited_by, withdrawn_by,
			based_on_version, change_note) VALUES ($1, $2, $3::jsonb, 'OUTDATED', $4, $5, $6, $7)`,
			id, newVersion, string(b), editedBy, p.UserID, withdrawn, trunc("withdrawn: "+reason, 500)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE schemes SET current_version = $2, updated_at = now() WHERE id = $1`, id, newVersion); err != nil {
			return err
		}
		return core.Audit(ctx, tx, p.actor(), "scheme.withdrawn", "scheme", id,
			map[string]any{"withdrawn_version": withdrawn, "outdated_version": newVersion, "base_version": cur, "reason": reason})
	})
	var ve *validationError
	switch {
	case errors.As(err, &ve):
		writeError(w, http.StatusUnprocessableEntity, "invalid_record", "record failed validation", splitErrors(ve.err)...)
		return
	case err != nil:
		s.writeCoreError(w, err)
		return
	}
	meta, rec, err := s.getVersion(ctx, id, newVersion)
	if err != nil {
		s.writeCoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "version": meta, "withdrawn_version": withdrawn, "record": rec})
}

type bundleMeta struct {
	BundleVersion string    `json:"bundle_version"`
	CreatedAt     time.Time `json:"created_at"`
	PublishedBy   string    `json:"published_by"`
	KeyID         string    `json:"key_id"`
	RecordCount   int       `json:"record_count"`
	VerifiedCount int       `json:"verified_count"`
	SHA256        string    `json:"sha256"`
}

func (s *Server) handleListBundles(w http.ResponseWriter, r *http.Request) {
	rows, err := s.pool.Query(r.Context(), `SELECT b.bundle_version, b.created_at, u.username, b.key_id, b.record_count,
		b.verified_count, b.sha256 FROM knowledge_bundles b JOIN users u ON u.id = b.published_by
		ORDER BY b.created_at DESC, b.id DESC LIMIT 100`)
	if err != nil {
		s.writeCoreError(w, err)
		return
	}
	defer rows.Close()
	out := []bundleMeta{}
	for rows.Next() {
		var m bundleMeta
		if err := rows.Scan(&m.BundleVersion, &m.CreatedAt, &m.PublishedBy, &m.KeyID, &m.RecordCount, &m.VerifiedCount, &m.SHA256); err != nil {
			s.writeCoreError(w, err)
			return
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		s.writeCoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"bundles": out})
}

// handlePublishBundle signs and stores a new bundle. Per scheme:
//
//   - the latest VERIFIED version, unless a later OUTDATED version exists
//     (it was withdrawn): a withdrawn VERIFIED version is never shipped;
//   - otherwise the latest NEEDS_REVIEW version newer than the latest
//     VERIFIED one (a pending draft or correction; devices show such records
//     only as "not yet verified", never as fact);
//   - otherwise nothing. UNVERIFIED and OUTDATED versions are never shipped.
//
// Devices replace their whole scheme table from a bundle, so a scheme that
// drops out of the bundle disappears from kiosks.
func (s *Server) handlePublishBundle(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := principalFrom(r)
	var empty struct{}
	if r.ContentLength != 0 {
		if !decodeAdmin(w, r, &empty) {
			return
		}
	}
	var meta bundleMeta
	err := core.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, bundleLockKey); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `WITH m AS (
				SELECT scheme_id,
				       COALESCE(max(version) FILTER (WHERE status = 'VERIFIED'), 0) AS lv,
				       COALESCE(max(version) FILTER (WHERE status = 'OUTDATED'), 0) AS lo
				FROM scheme_versions GROUP BY scheme_id)
			SELECT DISTINCT ON (v.scheme_id) v.scheme_id, v.version, v.status, v.record::text
			FROM scheme_versions v JOIN m ON m.scheme_id = v.scheme_id
			WHERE (v.status = 'VERIFIED' AND v.version = m.lv AND m.lo < m.lv)
			   OR (v.status = 'NEEDS_REVIEW' AND v.version > m.lv AND NOT (m.lv > 0 AND m.lo < m.lv))
			ORDER BY v.scheme_id, (v.status = 'VERIFIED') DESC, v.version DESC
			LIMIT $1`, maxBundleRecords+1)
		if err != nil {
			return err
		}
		var recs []scheme.Record
		type item struct {
			ID      string `json:"id"`
			Version int    `json:"version"`
			Status  string `json:"status"`
		}
		var contents []item
		verified := 0
		for rows.Next() {
			var it item
			var rj string
			if err := rows.Scan(&it.ID, &it.Version, &it.Status, &rj); err != nil {
				rows.Close()
				return err
			}
			var rec scheme.Record
			if err := json.Unmarshal([]byte(rj), &rec); err != nil {
				rows.Close()
				return err
			}
			if rec.Verification.Status != scheme.Status(it.Status) {
				rows.Close()
				return fmt.Errorf("scheme %s v%d: record status disagrees with row status", it.ID, it.Version)
			}
			if it.Status == string(scheme.Verified) {
				verified++
			}
			recs = append(recs, rec)
			contents = append(contents, it)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(recs) == 0 {
			return fmt.Errorf("%w: no VERIFIED or NEEDS_REVIEW records to publish", core.ErrConflict)
		}
		if len(recs) > maxBundleRecords {
			return fmt.Errorf("%w: more than %d records", core.ErrConflict, maxBundleRecords)
		}
		now := s.now()
		day := now.In(ist).Format("2006-01-02")
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM knowledge_bundles WHERE bundle_version LIKE $1 || '.%'`, day).Scan(&n); err != nil {
			return err
		}
		signer := s.keys.Load().Active()
		b := scheme.Bundle{BundleVersion: day + "." + strconv.Itoa(n+1), CreatedAt: now.UTC().Format(time.RFC3339), Records: recs}
		if err := deviceapi.SignBundle(signer.Priv, signer.KeyID, &b); err != nil {
			return err
		}
		signed, err := json.Marshal(b)
		if err != nil {
			return err
		}
		if len(signed) > deviceapi.MaxBundleBytes {
			return fmt.Errorf("%w: bundle exceeds %d bytes", core.ErrConflict, deviceapi.MaxBundleBytes)
		}
		// Self-check exactly as a device will: decode the served bytes and verify.
		var back scheme.Bundle
		if err := json.Unmarshal(signed, &back); err != nil {
			return err
		}
		if err := deviceapi.VerifyBundle(s.keys.Load().VerifyKeys(), back); err != nil {
			return fmt.Errorf("bundle self-verification failed: %w", err)
		}
		sum := sha256.Sum256(signed)
		cj, _ := json.Marshal(contents)
		meta = bundleMeta{BundleVersion: b.BundleVersion, CreatedAt: now, PublishedBy: p.Username, KeyID: signer.KeyID,
			RecordCount: len(recs), VerifiedCount: verified, SHA256: hex.EncodeToString(sum[:])}
		if _, err := tx.Exec(ctx, `INSERT INTO knowledge_bundles (id, bundle_version, created_at, published_by, key_id,
			record_count, verified_count, contents, sha256, signed_json) VALUES ($1,$2,$3,$4,$5,$6,$7,$8::jsonb,$9,$10)`,
			ids.NewV7(), meta.BundleVersion, now, p.UserID, meta.KeyID, meta.RecordCount, meta.VerifiedCount, string(cj),
			meta.SHA256, signed); err != nil {
			return err
		}
		return core.Audit(ctx, tx, p.actor(), "bundle.published", "bundle", meta.BundleVersion,
			map[string]any{"record_count": meta.RecordCount, "verified_count": meta.VerifiedCount, "sha256": meta.SHA256,
				"key_id": meta.KeyID})
	})
	if err != nil {
		s.writeCoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"bundle": meta})
}
