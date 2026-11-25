package httpapi

import (
	"context"
	"log/slog"
	"regexp"
	"time"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/budget"
)

// Per-user budgets on the data plane, and the per-user data the gateway keeps
// in memory for them.
//
// Every replica holds a snapshot of the users that have budgets or a group
// (userInfo), so neither the budget check nor the live view asks the
// database per request. It is reloaded on every RefreshConfig, so an edit
// made through another replica applies here within CONFIG_REFRESH_INTERVAL,
// and at once on the replica that saved it. It is keyed by the immutable
// user id, never by anything cached on an API key.

// userInfo is what the data plane needs to know about a key's owner.
type userInfo struct {
	Budgets   budget.Budgets
	GroupName string
}

// limitsLoc is the zone budget periods follow (LIMITS_TZ).
func (s *Server) limitsLoc() *time.Location {
	if s.cfg != nil && s.cfg.LimitsTZ != nil {
		return s.cfg.LimitsTZ
	}
	return time.UTC
}

// userInfoFor returns the snapshot entry for a user; a user missing from it
// has no budgets and no group.
func (s *Server) userInfoFor(userID string) userInfo {
	if m := s.userInfo.Load(); m != nil && userID != "" {
		return (*m)[userID]
	}
	return userInfo{}
}

// reloadUserInfo re-reads the snapshot after a change saved on this replica.
// It holds s.config.mu, so a refresh that read the table before the save
// cannot install its older copy afterwards.
func (s *Server) reloadUserInfo(ctx context.Context) {
	s.config.mu.Lock()
	defer s.config.mu.Unlock()
	if err := s.loadUserInfoLocked(ctx); err != nil {
		slog.Warn("user budgets and groups not reloaded; keeping the current ones", "err", err)
	}
}

// loadUserInfoLocked reads every user with budgets or a group and installs
// the result. A failed read keeps the current snapshot. Callers hold
// s.config.mu (or are NewServer, before anything else runs).
func (s *Server) loadUserInfoLocked(ctx context.Context) error {
	if s.st == nil || s.st.PG == nil {
		return nil
	}
	rows, err := s.st.PG.Query(ctx, `
		SELECT u.id::text, u.budgets, COALESCE(g.name, '')
		FROM users u LEFT JOIN user_groups g ON g.id = u.group_id
		WHERE u.budgets <> '{}'::jsonb OR u.group_id IS NOT NULL`)
	if err != nil {
		return err
	}
	defer rows.Close()
	m := map[string]userInfo{}
	for rows.Next() {
		var id, group string
		var raw []byte
		if err := rows.Scan(&id, &raw, &group); err != nil {
			return err
		}
		m[id] = userInfo{Budgets: budget.Parse(raw), GroupName: group}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	s.userInfo.Store(&m)
	return nil
}

// ledgerUsage sums one user's ledger rows in [start, end) for the budget
// counters' rebuild.
func (s *Server) ledgerUsage(ctx context.Context, userID string, start, end time.Time) (tokens, costMicro int64, err error) {
	err = s.st.PG.QueryRow(ctx, `
		SELECT COALESCE(SUM(prompt_tokens + completion_tokens), 0)::bigint,
		       COALESCE(round(SUM(cost_usd) * 1000000), 0)::bigint
		FROM usage_ledger WHERE user_id = $1 AND ts >= $2 AND ts < $3`,
		userID, start, end).Scan(&tokens, &costMicro)
	return tokens, costMicro, err
}

// budgetDenied checks the key owner's budgets. It returns a 429-ready
// message and true when a cap is reached. Redis and ledger errors fail open.
func (s *Server) budgetDenied(ctx context.Context, userID string) (string, bool) {
	if s.budgets == nil || userID == "" {
		return "", false
	}
	b := s.userInfoFor(userID).Budgets
	if !b.Any() {
		return "", false
	}
	d, denied, err := s.budgets.Check(ctx, userID, b)
	if err != nil {
		slog.Error("budget check failed; failing open", "err", err, "user_id", userID)
	}
	if !denied {
		return "", false
	}
	return d.Message(), true
}

// addBudgetUsage counts a settled request against its owner's budget
// counters: the same tokens and cost its ledger row records, in the periods
// of the same instant (at) the row is stamped with, so a counter rebuilt
// from the ledger and one kept up to date here agree.
func (s *Server) addBudgetUsage(ctx context.Context, userID string, at time.Time, tokens, costMicro int64) {
	if s.budgets == nil {
		return
	}
	if err := s.budgets.Add(ctx, userID, at, tokens, costMicro); err != nil {
		slog.Error("budget add failed", "err", err, "user_id", userID)
	}
}

// periodSpend is one period's usage.
type periodSpend struct {
	Tokens    int64
	CostMicro int64
}

// periodWindow is one period's bounds.
type periodWindow struct {
	Start, End time.Time
}

// currentPeriods returns the current day, week and month in LIMITS_TZ.
func (s *Server) currentPeriods() map[budget.Period]periodWindow {
	now := time.Now
	if s.now != nil {
		now = s.now
	}
	t := now()
	out := make(map[budget.Period]periodWindow, len(budget.Periods))
	for _, p := range budget.Periods {
		start, end := budget.Bounds(p, t, s.limitsLoc())
		out[p] = periodWindow{start, end}
	}
	return out
}

// spendByUser sums every user's ledger rows over the current day, week and
// month. It reads the ledger per user through the (user_id, ts) index — a
// LATERAL sum per users row — so its cost follows the rows in the periods,
// not the size of the whole ledger. Users without usage are left out.
func (s *Server) spendByUser(ctx context.Context, periods map[budget.Period]periodWindow) (map[string]map[budget.Period]periodSpend, error) {
	d, w, m := periods[budget.Day], periods[budget.Week], periods[budget.Month]
	from, to := d.Start, d.End
	for _, pw := range []periodWindow{w, m} {
		if pw.Start.Before(from) {
			from = pw.Start
		}
		if pw.End.After(to) {
			to = pw.End
		}
	}
	rows, err := s.st.PG.Query(ctx, `
		SELECT u.id::text, s.*
		FROM users u CROSS JOIN LATERAL (
			SELECT COALESCE(SUM(l.prompt_tokens + l.completion_tokens) FILTER (WHERE l.ts >= $1 AND l.ts < $2), 0)::bigint,
			       COALESCE(round(SUM(l.cost_usd) FILTER (WHERE l.ts >= $1 AND l.ts < $2) * 1000000), 0)::bigint,
			       COALESCE(SUM(l.prompt_tokens + l.completion_tokens) FILTER (WHERE l.ts >= $3 AND l.ts < $4), 0)::bigint,
			       COALESCE(round(SUM(l.cost_usd) FILTER (WHERE l.ts >= $3 AND l.ts < $4) * 1000000), 0)::bigint,
			       COALESCE(SUM(l.prompt_tokens + l.completion_tokens) FILTER (WHERE l.ts >= $5 AND l.ts < $6), 0)::bigint,
			       COALESCE(round(SUM(l.cost_usd) FILTER (WHERE l.ts >= $5 AND l.ts < $6) * 1000000), 0)::bigint,
			       count(*) AS n
			FROM usage_ledger l
			WHERE l.user_id = u.id AND l.ts >= $7 AND l.ts < $8
		) s
		WHERE s.n > 0`,
		d.Start, d.End, w.Start, w.End, m.Start, m.End, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[budget.Period]periodSpend{}
	for rows.Next() {
		var id string
		var n int64
		var ds, ws, ms periodSpend
		if err := rows.Scan(&id, &ds.Tokens, &ds.CostMicro, &ws.Tokens, &ws.CostMicro, &ms.Tokens, &ms.CostMicro, &n); err != nil {
			return nil, err
		}
		out[id] = map[budget.Period]periodSpend{budget.Day: ds, budget.Week: ws, budget.Month: ms}
	}
	return out, rows.Err()
}

// spendView is one period's usage in the admin users list.
type spendView struct {
	Tokens  int64   `json:"tokens"`
	CostUSD float64 `json:"cost_usd"`
}

// spendViews shapes a user's spend as {"day":…, "week":…, "month":…}.
func spendViews(sp map[budget.Period]periodSpend) map[string]spendView {
	out := make(map[string]spendView, len(budget.Periods))
	for _, p := range budget.Periods {
		v := sp[p]
		out[p.Name()] = spendView{Tokens: v.Tokens, CostUSD: microToUSD(v.CostMicro)}
	}
	return out
}

func microToUSD(m int64) float64 { return float64(m) / 1e6 }

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// isUUID reports whether s is a textual UUID, so a malformed id in a path or
// body is answered as not found / bad request rather than a database error.
func isUUID(s string) bool { return uuidPattern.MatchString(s) }
