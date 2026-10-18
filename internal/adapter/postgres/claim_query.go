package postgres

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/postgres/sqlcdb"
	"github.com/eliteGoblin/eks-gitops-demo/internal/app"
	"github.com/eliteGoblin/eks-gitops-demo/internal/domain"
)

// The claim listing combines optional filters with one of four orders, so
// its SQL is assembled here instead of written out as a sqlc query per
// combination. Only fixed fragments are written into the text (columns come
// from a closed set chosen by the order); every value the caller supplies,
// including the search prefix, travels as a bind parameter.

const claimColumns = `c.id, c.plan_id, c.provider_id, c.category, c.amount_cents, c.status,
       c.service_date, c.submitted_at, c.payment_status, c.payment_ref`

// claimSQL collects WHERE conditions and their bind parameters.
type claimSQL struct {
	where []string
	args  []any
}

// bind adds a parameter and returns its placeholder.
func (b *claimSQL) bind(v any) string {
	b.args = append(b.args, v)
	return fmt.Sprintf("$%d", len(b.args))
}

func (b *claimSQL) and(condition string) { b.where = append(b.where, condition) }

func (b *claimSQL) whereClause() string {
	if len(b.where) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(b.where, " AND ")
}

// claimFilter applies the scope, the filter and the id prefix of q.
func claimFilter(q app.ClaimQuery) *claimSQL {
	b := &claimSQL{}
	if !q.Scope.All {
		b.and(fmt.Sprintf("(p.participant_id = ANY(%s::text[]) OR c.provider_id = %s)",
			b.bind(nonNil(q.Scope.ParticipantIDs)), b.bind(q.Scope.ProviderID)))
	}
	f := q.Filter
	if f.PlanID != "" {
		b.and("c.plan_id = " + b.bind(f.PlanID))
	}
	if f.Status != nil {
		b.and("c.status = " + b.bind(string(*f.Status)))
	}
	if f.Category != nil {
		b.and("c.category = " + b.bind(string(*f.Category)))
	}
	if f.ProviderID != "" {
		b.and("c.provider_id = " + b.bind(f.ProviderID))
	}
	if f.SubmittedAfter != nil {
		b.and("c.submitted_at >= " + b.bind(*f.SubmittedAfter))
	}
	if f.SubmittedBefore != nil {
		b.and("c.submitted_at < " + b.bind(*f.SubmittedBefore))
	}
	if q.IDPrefix != "" {
		b.and("c.id ILIKE " + b.bind(likeEscape(q.IDPrefix)+"%") + ` ESCAPE '\'`)
	}
	return b
}

// sortKey returns the order's key column, direction and the keyset
// comparison that means "after" in it. Ids compare byte-wise (COLLATE "C").
func sortKey(o app.ClaimOrder) (column, direction, after string) {
	column, direction, after = "c.submitted_at", "ASC", ">"
	if o.Field == app.OrderByAmount {
		column = "c.amount_cents"
	}
	if o.Descending {
		direction, after = "DESC", "<"
	}
	return column, direction, after
}

// QueryClaims returns one keyset page of q in its order.
func (s *Store) QueryClaims(ctx context.Context, q app.ClaimQuery) ([]domain.Claim, error) {
	b := claimFilter(q)
	order := q.Order.Normalized()
	column, direction, after := sortKey(order)
	if q.After != nil {
		var key any = q.After.SubmittedAt
		if order.Field == app.OrderByAmount {
			key = int64(q.After.Amount)
		}
		b.and(fmt.Sprintf(`(%s, c.id COLLATE "C") %s (%s, %s)`, column, after, b.bind(key), b.bind(q.After.ID)))
	}
	limit := maxLimit
	if q.Limit > 0 && q.Limit < maxLimit {
		limit = q.Limit
	}
	sql := "SELECT " + claimColumns + " FROM claims c JOIN plans p ON p.id = c.plan_id" + b.whereClause() +
		fmt.Sprintf(` ORDER BY %s %s, c.id COLLATE "C" %s LIMIT %s`, column, direction, direction, b.bind(limit))
	rows, err := classifying{db: s.pool}.Query(ctx, sql, b.args...)
	if err != nil {
		return nil, err
	}
	claims, err := pgx.CollectRows(rows, scanClaim)
	if err != nil {
		return nil, err
	}
	return mapRows(claims, toClaim), nil
}

// CountClaims counts what q selects, ignoring its order and page.
func (s *Store) CountClaims(ctx context.Context, q app.ClaimQuery) (int, error) {
	b := claimFilter(q)
	var n int
	err := classifying{db: s.pool}.QueryRow(ctx, "SELECT count(*) FROM claims c JOIN plans p ON p.id = c.plan_id"+b.whereClause(), b.args...).Scan(&n)
	return n, err
}

func scanClaim(row pgx.CollectableRow) (sqlcdb.Claim, error) {
	var c sqlcdb.Claim
	err := row.Scan(&c.ID, &c.PlanID, &c.ProviderID, &c.Category, &c.AmountCents, &c.Status,
		&c.ServiceDate, &c.SubmittedAt, &c.PaymentStatus, &c.PaymentRef)
	return c, err
}

// likeEscape makes s match literally inside a LIKE pattern (ESCAPE '\').
func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}
