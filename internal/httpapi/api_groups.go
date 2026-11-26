package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/budget"
)

// User groups: a label per user for reporting spend. They carry no budgets
// of their own; see budgets.go for the per-user ones.

const (
	maxGroupNameRunes        = 100
	maxGroupDescriptionRunes = 1000
)

// groupView is one group in the admin API.
type groupView struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	MemberCount int    `json:"member_count"`
}

func (s *Server) handleAdminGroups(w http.ResponseWriter, r *http.Request) {
	rows, err := s.st.PG.Query(r.Context(), `
		SELECT g.id::text, g.name, g.description, count(u.id)::int
		FROM user_groups g LEFT JOIN users u ON u.group_id = g.id
		GROUP BY g.id ORDER BY lower(g.name)`)
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to list groups")
		return
	}
	defer rows.Close()
	out := []groupView{}
	for rows.Next() {
		var g groupView
		if err := rows.Scan(&g.ID, &g.Name, &g.Description, &g.MemberCount); err != nil {
			writeControlError(w, http.StatusInternalServerError, "failed to read groups")
			return
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to read groups")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": out})
}

// groupBody is the body of a group create or update. On update a field left
// out keeps its stored value.
type groupBody struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
}

// validate trims and checks the fields; creating requires a name.
func (b *groupBody) validate(creating bool) error {
	if b.Name != nil {
		n := strings.TrimSpace(*b.Name)
		b.Name = &n
		if n == "" {
			return errors.New("name is required")
		}
		if utf8.RuneCountInString(n) > maxGroupNameRunes {
			return fmt.Errorf("name must be at most %d characters", maxGroupNameRunes)
		}
	} else if creating {
		return errors.New("name is required")
	}
	if b.Description != nil && utf8.RuneCountInString(*b.Description) > maxGroupDescriptionRunes {
		return fmt.Errorf("description must be at most %d characters", maxGroupDescriptionRunes)
	}
	return nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func (s *Server) handleAdminCreateGroup(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	var body groupBody
	if err := decodeJSON(r, &body); err != nil {
		writeControlError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if err := body.validate(true); err != nil {
		writeControlError(w, http.StatusBadRequest, err.Error())
		return
	}
	g := groupView{Name: *body.Name}
	if body.Description != nil {
		g.Description = *body.Description
	}
	err := s.st.PG.QueryRow(r.Context(),
		`INSERT INTO user_groups (name, description) VALUES ($1, $2) RETURNING id::text`,
		g.Name, g.Description).Scan(&g.ID)
	if isUniqueViolation(err) {
		writeControlError(w, http.StatusConflict, "a group with this name already exists")
		return
	}
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to create group")
		return
	}
	s.audit(r.Context(), sess.principal.Subject, "group.create", g.ID, map[string]any{"name": g.Name})
	writeJSON(w, http.StatusCreated, g)
}

func (s *Server) handleAdminUpdateGroup(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	id := r.PathValue("id")
	var body groupBody
	if err := decodeJSON(r, &body); err != nil {
		writeControlError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if err := body.validate(false); err != nil {
		writeControlError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !isUUID(id) {
		writeControlError(w, http.StatusNotFound, "group not found")
		return
	}
	var g groupView
	err := s.st.PG.QueryRow(r.Context(), `
		UPDATE user_groups SET name = COALESCE($2, name), description = COALESCE($3, description)
		WHERE id = $1
		RETURNING id::text, name, description,
			(SELECT count(*)::int FROM users WHERE group_id = $1)`,
		id, body.Name, body.Description).Scan(&g.ID, &g.Name, &g.Description, &g.MemberCount)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeControlError(w, http.StatusNotFound, "group not found")
		return
	case isUniqueViolation(err):
		writeControlError(w, http.StatusConflict, "a group with this name already exists")
		return
	case err != nil:
		writeControlError(w, http.StatusInternalServerError, "failed to update group")
		return
	}
	s.reloadUserInfo(r.Context()) // the live view shows group names
	s.audit(r.Context(), sess.principal.Subject, "group.update", id, map[string]any{"name": g.Name})
	writeJSON(w, http.StatusOK, g)
}

func (s *Server) handleAdminDeleteGroup(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	id := r.PathValue("id")
	if !isUUID(id) {
		writeControlError(w, http.StatusNotFound, "group not found")
		return
	}
	// Members are ungrouped by the foreign key (ON DELETE SET NULL).
	var name string
	err := s.st.PG.QueryRow(r.Context(), `DELETE FROM user_groups WHERE id = $1 RETURNING name`, id).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		writeControlError(w, http.StatusNotFound, "group not found")
		return
	}
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to delete group")
		return
	}
	s.reloadUserInfo(r.Context())
	s.audit(r.Context(), sess.principal.Subject, "group.delete", id, map[string]any{"name": name})
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// ungroupedName labels usage of users without a group, and of ledger rows
// whose user was deleted.
const ungroupedName = "Ungrouped"

// usageTotals is a usage sum in the group report.
type usageTotals struct {
	Tokens   int64   `json:"tokens"`
	CostUSD  float64 `json:"cost_usd"`
	Requests int64   `json:"requests"`
}

type groupUsageUser struct {
	UserID   *string `json:"user_id"` // null for usage of a deleted user
	Subject  string  `json:"subject"`
	Label    string  `json:"label"` // see userLabelSQL; "" for usage of a deleted user
	Tokens   int64   `json:"tokens"`
	CostUSD  float64 `json:"cost_usd"`
	Requests int64   `json:"requests"`

	costMicro int64
}

type groupUsage struct {
	GroupID  *string          `json:"group_id"` // null for Ungrouped
	Name     string           `json:"name"`
	Tokens   int64            `json:"tokens"`
	CostUSD  float64          `json:"cost_usd"`
	Requests int64            `json:"requests"`
	Users    []groupUsageUser `json:"users"`

	costMicro int64
}

// handleAdminUsageGroups reports the current day, week or month's usage per
// group (each user's CURRENT group) and per user within it. Every defined
// group is listed, and Ungrouped always is; the total is the whole ledger
// for the period, so it equals the sum of the groups.
func (s *Server) handleAdminUsageGroups(w http.ResponseWriter, r *http.Request) {
	periodName := r.URL.Query().Get("period")
	if periodName == "" {
		periodName = budget.Month.Name()
	}
	var period budget.Period
	found := false
	for _, p := range budget.Periods {
		if p.Name() == periodName {
			period, found = p, true
		}
	}
	if !found {
		writeControlError(w, http.StatusBadRequest, "period must be day, week or month")
		return
	}
	win := s.currentPeriods()[period]
	rep, err := s.reports.get("groups:"+period.Name()+":"+win.Start.Format(time.RFC3339), s.clock(), func() (any, error) {
		return s.groupReport(r.Context(), period, win)
	})
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to load usage")
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// groupReport builds the body of GET /api/admin/usage/groups.
func (s *Server) groupReport(ctx context.Context, period budget.Period, win periodWindow) (map[string]any, error) {
	groups := map[string]*groupUsage{}
	ungrouped := &groupUsage{Name: ungroupedName, Users: []groupUsageUser{}}
	grows, err := s.st.PG.Query(ctx, `SELECT id::text, name FROM user_groups`)
	if err != nil {
		return nil, err
	}
	for grows.Next() {
		var id, name string
		if err := grows.Scan(&id, &name); err != nil {
			grows.Close()
			return nil, err
		}
		groups[id] = &groupUsage{GroupID: &id, Name: name, Users: []groupUsageUser{}}
	}
	grows.Close()
	if err := grows.Err(); err != nil {
		return nil, err
	}

	// Per user through the (user_id, ts) index, so the cost follows the
	// period's rows rather than the whole ledger; rows of deleted users
	// (user_id NULL) are summed through the same index.
	rows, err := s.st.PG.Query(ctx, `
		SELECT u.id::text, u.subject, `+userLabelSQL+`, u.group_id::text, s.tokens, s.cost, s.n
		FROM users u CROSS JOIN LATERAL (
			SELECT COALESCE(SUM(l.prompt_tokens + l.completion_tokens), 0)::bigint AS tokens,
			       COALESCE(round(SUM(l.cost_usd) * 1000000), 0)::bigint AS cost,
			       count(*) AS n
			FROM usage_ledger l
			WHERE l.user_id = u.id AND l.ts >= $1 AND l.ts < $2
		) s
		WHERE s.n > 0
		UNION ALL
		SELECT NULL, '', '', NULL,
		       COALESCE(SUM(prompt_tokens + completion_tokens), 0)::bigint,
		       COALESCE(round(SUM(cost_usd) * 1000000), 0)::bigint,
		       count(*)
		FROM usage_ledger
		WHERE user_id IS NULL AND ts >= $1 AND ts < $2
		HAVING count(*) > 0`, win.Start, win.End)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var total usageTotals
	var totalMicro int64
	for rows.Next() {
		var u groupUsageUser
		var groupID *string
		if err := rows.Scan(&u.UserID, &u.Subject, &u.Label, &groupID, &u.Tokens, &u.costMicro, &u.Requests); err != nil {
			return nil, err
		}
		u.CostUSD = microToUSD(u.costMicro)
		g := ungrouped
		if groupID != nil && groups[*groupID] != nil {
			g = groups[*groupID]
		}
		g.Users = append(g.Users, u)
		g.Tokens += u.Tokens
		g.costMicro += u.costMicro
		g.Requests += u.Requests
		total.Tokens += u.Tokens
		totalMicro += u.costMicro
		total.Requests += u.Requests
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	total.CostUSD = microToUSD(totalMicro)

	out := make([]*groupUsage, 0, len(groups)+1)
	for _, g := range groups {
		out = append(out, g)
	}
	out = append(out, ungrouped)
	for _, g := range out {
		g.CostUSD = microToUSD(g.costMicro)
		sort.SliceStable(g.Users, func(i, j int) bool {
			if g.Users[i].costMicro != g.Users[j].costMicro {
				return g.Users[i].costMicro > g.Users[j].costMicro
			}
			return g.Users[i].Subject < g.Users[j].Subject
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].costMicro != out[j].costMicro {
			return out[i].costMicro > out[j].costMicro
		}
		// Equal spend: named groups alphabetically, Ungrouped after them.
		if (out[i].GroupID == nil) != (out[j].GroupID == nil) {
			return out[j].GroupID == nil
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return map[string]any{
		"period": period.Name(),
		"tz":     s.limitsLoc().String(),
		"start":  win.Start,
		"end":    win.End,
		"total":  total,
		"groups": out,
	}, nil
}
