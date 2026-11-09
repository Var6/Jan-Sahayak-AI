package server

import (
	"context"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/janmanindia/jan-sahayak/internal/ids"
	"github.com/janmanindia/jan-sahayak/services/central-api/internal/core"
)

// Analytics are computed from anonymous sync events (views analytics_sessions
// and analytics_turns). Days are calendar days in IST, bucketed by the
// event's created_at (device clock).

const maxAnalyticsDays = 366

type countRow struct {
	Key   string `json:"key"`
	Count int64  `json:"count"`
}

type dayRow struct {
	Day      string `json:"day"`
	Sessions int64  `json:"sessions"`
}

type categoryRow struct {
	Category   string `json:"category"`
	Turns      int64  `json:"turns"`
	Sessions   int64  `json:"sessions"`
	Unresolved int64  `json:"unresolved_turns"`
}

type analyticsSummary struct {
	From              string        `json:"from"`
	To                string        `json:"to"`
	Timezone          string        `json:"timezone"`
	DeviceID          string        `json:"device_id,omitempty"`
	District          string        `json:"district,omitempty"`
	SessionsTotal     int64         `json:"sessions_total"`
	AvgTurns          float64       `json:"avg_turns_per_session"`
	SessionsPerDay    []dayRow      `json:"sessions_per_day"`
	Languages         []countRow    `json:"languages"`
	EndReasons        []countRow    `json:"end_reasons"`
	Categories        []categoryRow `json:"categories"`
	TurnsTotal        int64         `json:"turns_total"`
	TurnsByResolution []countRow    `json:"turns_by_resolution"`
	TurnsBySource     []countRow    `json:"turns_by_source"`
	TurnsUsingLLM     int64         `json:"turns_using_llm"`
	// Session-level resolution, over sessions that have at least one turn event.
	SessionsWithTurns  int64   `json:"sessions_with_turns"`
	SessionsAnswered   int64   `json:"sessions_answered"`   // ≥1 turn "answered"
	SessionsUnresolved int64   `json:"sessions_unresolved"` // no turn answered/unverified_info/escalated/crisis
	ResolutionRate     float64 `json:"resolution_rate"`     // answered / with_turns
}

func (s *Server) handleAnalytics(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	now := s.now().In(ist)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, ist)
	from, to := today.AddDate(0, 0, -29), today
	var err error
	if v := q.Get("from"); v != "" {
		if from, err = time.ParseInLocation("2006-01-02", v, ist); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "from must be YYYY-MM-DD")
			return
		}
	}
	if v := q.Get("to"); v != "" {
		if to, err = time.ParseInLocation("2006-01-02", v, ist); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "to must be YYYY-MM-DD")
			return
		}
	}
	if to.Before(from) || to.Sub(from) > maxAnalyticsDays*24*time.Hour {
		writeError(w, http.StatusBadRequest, "bad_request", "range must be 0..366 days with from <= to")
		return
	}
	devID, district := q.Get("device_id"), q.Get("district")
	if devID != "" && ids.Validate(devID) != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "device_id must be a uuid")
		return
	}
	if len(district) > 60 {
		writeError(w, http.StatusBadRequest, "bad_request", "district too long")
		return
	}
	sum := analyticsSummary{From: from.Format("2006-01-02"), To: to.Format("2006-01-02"), Timezone: "Asia/Kolkata",
		DeviceID: devID, District: district}
	if err := s.computeAnalytics(r.Context(), &sum, from, to.AddDate(0, 0, 1), devID, district); err != nil {
		s.writeCoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sum)
}

func (s *Server) computeAnalytics(ctx context.Context, sum *analyticsSummary, from, toExcl time.Time, devID, district string) error {
	// Read-only, repeatable-read snapshot so all numbers agree.
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	nullable := func(v string) any {
		if v == "" {
			return nil
		}
		return v
	}
	args := []any{from, toExcl, nullable(devID), nullable(district)}
	// Every query shares this filter; $1..$4 are the args above.
	const filter = ` x.created_at >= $1 AND x.created_at < $2
		AND ($3::uuid IS NULL OR x.device_id = $3::uuid)
		AND ($4::text IS NULL OR x.device_id IN (SELECT id FROM devices WHERE district = $4)) `

	if err := tx.QueryRow(ctx, `SELECT count(*), COALESCE(avg(turns), 0)::float8 FROM analytics_sessions x WHERE`+filter, args...).
		Scan(&sum.SessionsTotal, &sum.AvgTurns); err != nil {
		return err
	}
	sum.SessionsPerDay = []dayRow{}
	if err := collect(ctx, tx, `SELECT to_char((x.created_at AT TIME ZONE 'Asia/Kolkata')::date, 'YYYY-MM-DD'), count(*)
		FROM analytics_sessions x WHERE`+filter+` GROUP BY 1 ORDER BY 1`, args, func(r pgx.Rows) error {
		var d dayRow
		if err := r.Scan(&d.Day, &d.Sessions); err != nil {
			return err
		}
		sum.SessionsPerDay = append(sum.SessionsPerDay, d)
		return nil
	}); err != nil {
		return err
	}
	counts := func(dst *[]countRow, sql string) error {
		*dst = []countRow{}
		return collect(ctx, tx, sql, args, func(r pgx.Rows) error {
			var c countRow
			if err := r.Scan(&c.Key, &c.Count); err != nil {
				return err
			}
			*dst = append(*dst, c)
			return nil
		})
	}
	if err := counts(&sum.Languages, `SELECT language, count(*) FROM analytics_sessions x WHERE`+filter+` GROUP BY 1 ORDER BY 2 DESC, 1`); err != nil {
		return err
	}
	if err := counts(&sum.EndReasons, `SELECT end_reason, count(*) FROM analytics_sessions x WHERE`+filter+` GROUP BY 1 ORDER BY 2 DESC, 1`); err != nil {
		return err
	}
	if err := counts(&sum.TurnsByResolution, `SELECT CASE WHEN resolution = '' THEN 'none' ELSE resolution END, count(*)
		FROM analytics_turns x WHERE`+filter+` GROUP BY 1 ORDER BY 2 DESC, 1`); err != nil {
		return err
	}
	if err := counts(&sum.TurnsBySource, `SELECT source, count(*) FROM analytics_turns x WHERE`+filter+` GROUP BY 1 ORDER BY 2 DESC, 1`); err != nil {
		return err
	}
	if err := tx.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE used_llm) FROM analytics_turns x WHERE`+filter, args...).
		Scan(&sum.TurnsTotal, &sum.TurnsUsingLLM); err != nil {
		return err
	}
	sum.Categories = []categoryRow{}
	if err := collect(ctx, tx, `SELECT CASE WHEN category = '' THEN 'uncategorised' ELSE category END, count(*),
		count(DISTINCT session_id), count(*) FILTER (WHERE resolution = 'unresolved')
		FROM analytics_turns x WHERE`+filter+` GROUP BY 1 ORDER BY 2 DESC, 1`, args, func(r pgx.Rows) error {
		var c categoryRow
		if err := r.Scan(&c.Category, &c.Turns, &c.Sessions, &c.Unresolved); err != nil {
			return err
		}
		sum.Categories = append(sum.Categories, c)
		return nil
	}); err != nil {
		return err
	}
	if err := tx.QueryRow(ctx, `WITH s AS (
			SELECT session_id,
			       bool_or(resolution = 'answered') AS answered,
			       bool_or(resolution IN ('answered', 'unverified_info', 'escalated', 'crisis')) AS handled
			FROM analytics_turns x WHERE`+filter+` GROUP BY session_id)
		SELECT count(*), count(*) FILTER (WHERE answered), count(*) FILTER (WHERE NOT handled) FROM s`, args...).
		Scan(&sum.SessionsWithTurns, &sum.SessionsAnswered, &sum.SessionsUnresolved); err != nil {
		return err
	}
	if sum.SessionsWithTurns > 0 {
		sum.ResolutionRate = float64(sum.SessionsAnswered) / float64(sum.SessionsWithTurns)
	}
	return nil
}

func collect(ctx context.Context, q core.Querier, sql string, args []any, fn func(pgx.Rows) error) error {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := fn(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}
