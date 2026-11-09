package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/janmanindia/jan-sahayak/internal/ids"
)

type auditEntry struct {
	ID          string          `json:"id"`
	At          time.Time       `json:"at"`
	ActorUserID *string         `json:"actor_user_id,omitempty"`
	Actor       string          `json:"actor"`
	Action      string          `json:"action"`
	TargetType  string          `json:"target_type"`
	TargetID    string          `json:"target_id"`
	Details     json.RawMessage `json:"details"`
}

// handleAudit lists audit entries newest first. Query: limit (1..200,
// default 50), before (an entry id, for paging), action, target_type,
// target_id (exact-match filters).
func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 50
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			writeError(w, http.StatusBadRequest, "bad_request", "limit must be 1..200")
			return
		}
		limit = n
	}
	var before any
	if v := q.Get("before"); v != "" {
		if ids.Validate(v) != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "before must be an entry id")
			return
		}
		before = v
	}
	opt := func(k string) any {
		if v := q.Get(k); v != "" {
			return trunc(v, 100)
		}
		return nil
	}
	rows, err := s.pool.Query(r.Context(), `SELECT id::text, at, actor_user_id::text, actor, action, target_type, target_id, details::text
		FROM audit_logs
		WHERE ($1::uuid IS NULL OR id < $1::uuid)
		  AND ($2::text IS NULL OR action = $2) AND ($3::text IS NULL OR target_type = $3) AND ($4::text IS NULL OR target_id = $4)
		ORDER BY id DESC LIMIT $5`, before, opt("action"), opt("target_type"), opt("target_id"), limit)
	if err != nil {
		s.writeCoreError(w, err)
		return
	}
	defer rows.Close()
	out := []auditEntry{}
	for rows.Next() {
		var e auditEntry
		var d string
		if err := rows.Scan(&e.ID, &e.At, &e.ActorUserID, &e.Actor, &e.Action, &e.TargetType, &e.TargetID, &d); err != nil {
			s.writeCoreError(w, err)
			return
		}
		e.Details = json.RawMessage(d)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		s.writeCoreError(w, err)
		return
	}
	resp := map[string]any{"entries": out}
	if len(out) == limit {
		resp["next_before"] = out[len(out)-1].ID
	}
	writeJSON(w, http.StatusOK, resp)
}
