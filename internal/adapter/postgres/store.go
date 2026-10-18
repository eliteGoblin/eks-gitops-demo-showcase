// Package postgres implements the app's repository ports on PostgreSQL with
// pgx and sqlc-generated queries. Budget-changing writes run in one
// transaction that locks the plan row first, so concurrent claims and
// approvals on a plan are serialized.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/postgres/sqlcdb"
	"github.com/eliteGoblin/eks-gitops-demo/internal/app"
	"github.com/eliteGoblin/eks-gitops-demo/internal/domain"
)

// Store implements every repository port and app.Relationships.
type Store struct {
	pool *pgxpool.Pool
	q    *sqlcdb.Queries
}

// New returns a store on an open pool. The caller owns the pool.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, q: queries(pool)}
}

// PoolOptions bound the connection pool and the sessions it hands out. Zero
// values keep pgx's defaults (apart from a connection lifetime), which suits
// the migrate Job; the API sets all of them so one slow transaction cannot
// hold the plan lock, and a pool connection, for longer than a request.
type PoolOptions struct {
	MaxConns                 int32           // connections per pod; 0 keeps pgx's default
	LockTimeout              time.Duration   // how long a statement waits for a row lock
	StatementTimeout         time.Duration   // how long any statement may run
	IdleInTransactionTimeout time.Duration   // how long a transaction may sit idle
	Tracer                   pgx.QueryTracer // observes every query (telemetry); nil for none
}

// Connections are recycled so that the pool follows the database's failover
// and rotation events instead of pinning old sessions.
const (
	connLifetime       = 30 * time.Minute
	connLifetimeJitter = 5 * time.Minute
	connIdleTime       = 5 * time.Minute
)

// Open connects to dsn (libpq URL or key/value; empty uses the PG* environment
// variables), applies opts and checks the connection. Errors name the host and
// database only: the connection string carries the password and is never echoed.
func Open(ctx context.Context, dsn string, opts PoolOptions) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("postgres: DATABASE_URL is not a valid connection string")
	}
	opts.apply(cfg)
	where := fmt.Sprintf("%s:%d/%s", cfg.ConnConfig.Host, cfg.ConnConfig.Port, cfg.ConnConfig.Database)
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: connect to %s: %s", where, redact(err, cfg.ConnConfig.Password))
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: ping %s: %s", where, redact(err, cfg.ConnConfig.Password))
	}
	return pool, nil
}

func (o PoolOptions) apply(cfg *pgxpool.Config) {
	if o.MaxConns > 0 {
		cfg.MaxConns = o.MaxConns
	}
	cfg.MaxConnLifetime = connLifetime
	cfg.MaxConnLifetimeJitter = connLifetimeJitter
	cfg.MaxConnIdleTime = connIdleTime
	if o.Tracer != nil {
		cfg.ConnConfig.Tracer = o.Tracer
	}
	params := cfg.ConnConfig.RuntimeParams
	if _, set := params["application_name"]; !set {
		params["application_name"] = "demo-api"
	}
	for name, d := range map[string]time.Duration{
		"lock_timeout":                        o.LockTimeout,
		"statement_timeout":                   o.StatementTimeout,
		"idle_in_transaction_session_timeout": o.IdleInTransactionTimeout,
	} {
		if d > 0 {
			params[name] = pgDuration(d)
		}
	}
}

// pgDuration formats d the way Postgres prints it back (whole seconds when possible).
func pgDuration(d time.Duration) string {
	if d%time.Second == 0 {
		return fmt.Sprintf("%ds", int(d/time.Second))
	}
	return fmt.Sprintf("%dms", d.Milliseconds())
}

// redact removes the password from an error message, in case a driver ever
// includes connection details in it.
func redact(err error, password string) string {
	if password == "" {
		return err.Error()
	}
	return strings.ReplaceAll(err.Error(), password, "xxxxx")
}

// Reachable reports whether the database answers, without competing with
// requests for the pool: when every connection is busy the database is
// serving queries, so that counts as reachable; otherwise a ping takes an idle
// connection or opens a new one. Run it in the background (platform.CachedCheck)
// rather than per readiness probe.
func (s *Store) Reachable(ctx context.Context) error {
	if st := s.pool.Stat(); st.IdleConns() == 0 && st.TotalConns() >= st.MaxConns() {
		return nil
	}
	return s.pool.Ping(ctx)
}

// Notify sends payload to every session listening on channel (NOTIFY).
func (s *Store) Notify(ctx context.Context, channel, payload string) error {
	_, err := s.pool.Exec(ctx, "SELECT pg_notify($1, $2)", channel, payload)
	return classify(err)
}

// Connect opens a dedicated connection with the pool's settings, outside the
// pool, for a LISTEN session (one per replica; count it in the connection
// budget next to the pool).
func (s *Store) Connect(ctx context.Context) (*pgx.Conn, error) {
	cfg := s.pool.Config().ConnConfig.Copy()
	cfg.RuntimeParams["application_name"] = "demo-api-events"
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: connect for events: %s", redact(err, cfg.Password))
	}
	return conn, nil
}

// notFound maps "no rows" to domain.ErrNotFound.
func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrNotFound
	}
	return err
}

// Transaction modes. Budget writes lock the plan row and then read the plan's
// claims and bookings: only READ COMMITTED lets those reads see what the
// previous lock holder committed, so it is pinned rather than left to the
// database default. Multi-query reads take one consistent read-only snapshot.
var (
	writeTx = pgx.TxOptions{IsoLevel: pgx.ReadCommitted}
	readTx  = pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}
)

// rollbackTimeout bounds a rollback on a connection that stopped answering.
const rollbackTimeout = 5 * time.Second

// inTx runs fn in one read-write transaction (the atomic budget paths).
func (s *Store) inTx(ctx context.Context, fn func(q *sqlcdb.Queries) error) error {
	return s.withTx(ctx, writeTx, func(tx pgx.Tx) error { return fn(queries(tx)) })
}

// inReadTx runs fn in one read-only transaction, so reads that take several
// queries see one consistent snapshot and cannot write by mistake.
func (s *Store) inReadTx(ctx context.Context, fn func(q *sqlcdb.Queries) error) error {
	return s.withTx(ctx, readTx, func(tx pgx.Tx) error { return fn(queries(tx)) })
}

// withTx commits when fn returns nil and rolls back otherwise, including when
// fn panics: the deferred rollback runs regardless, so a plan lock and its
// pool connection are never leaked. The rollback outlives the request's
// cancellation (a client disconnect still ends the transaction) but is bounded,
// so a dead connection cannot hold the request. A failed commit or rollback is
// returned, never swallowed.
func (s *Store) withTx(ctx context.Context, opts pgx.TxOptions, fn func(tx pgx.Tx) error) (err error) {
	tx, err := s.pool.BeginTx(ctx, opts)
	if err != nil {
		return fmt.Errorf("begin: %w", classify(err))
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		rbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
		defer cancel()
		if rbErr := tx.Rollback(rbCtx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
			err = errors.Join(err, fmt.Errorf("rollback: %w", rbErr))
		}
	}()
	if fnErr := fn(tx); fnErr != nil {
		return fnErr
	}
	if commitErr := tx.Commit(ctx); commitErr != nil {
		return fmt.Errorf("commit: %w", classify(commitErr))
	}
	committed = true
	return nil
}

func (s *Store) GetParticipant(ctx context.Context, id string) (domain.Participant, error) {
	p, err := s.q.GetParticipant(ctx, id)
	if err != nil {
		return domain.Participant{}, notFound(err)
	}
	return toParticipant(p), nil
}

func (s *Store) GetParticipants(ctx context.Context, ids []string) ([]domain.Participant, error) {
	rows, err := s.q.GetParticipants(ctx, ids)
	return mapRows(rows, toParticipant), err
}

func (s *Store) GetProvider(ctx context.Context, id string) (domain.Provider, error) {
	p, err := s.q.GetProvider(ctx, id)
	if err != nil {
		return domain.Provider{}, notFound(err)
	}
	return toProvider(p), nil
}

func (s *Store) GetProviders(ctx context.Context, ids []string) ([]domain.Provider, error) {
	rows, err := s.q.GetProviders(ctx, ids)
	return mapRows(rows, toProvider), err
}

// Plans are read with their allocations: two queries in one read-only
// transaction, so a plan never pairs with another moment's allocations.

func (s *Store) GetPlan(ctx context.Context, id string) (domain.Plan, error) {
	var plans []domain.Plan
	err := s.inReadTx(ctx, func(q *sqlcdb.Queries) error {
		p, err := q.GetPlan(ctx, id)
		if err != nil {
			return notFound(err)
		}
		plans, err = withAllocations(ctx, q, []sqlcdb.Plan{p})
		return err
	})
	if err != nil {
		return domain.Plan{}, err
	}
	return plans[0], nil
}

func (s *Store) GetPlans(ctx context.Context, ids []string) ([]domain.Plan, error) {
	return s.readPlans(ctx, func(q *sqlcdb.Queries) ([]sqlcdb.Plan, error) { return q.GetPlans(ctx, ids) })
}

func (s *Store) ListPlansByParticipant(ctx context.Context, participantID string) ([]domain.Plan, error) {
	return s.readPlans(ctx, func(q *sqlcdb.Queries) ([]sqlcdb.Plan, error) {
		return q.ListPlansByParticipant(ctx, participantID)
	})
}

func (s *Store) readPlans(ctx context.Context, rows func(q *sqlcdb.Queries) ([]sqlcdb.Plan, error)) ([]domain.Plan, error) {
	var plans []domain.Plan
	err := s.inReadTx(ctx, func(q *sqlcdb.Queries) error {
		found, err := rows(q)
		if err != nil {
			return err
		}
		plans, err = withAllocations(ctx, q, found)
		return err
	})
	return plans, err
}

// withAllocations loads the allocations of all plans in one query.
func withAllocations(ctx context.Context, q *sqlcdb.Queries, rows []sqlcdb.Plan) ([]domain.Plan, error) {
	ids := make([]string, len(rows))
	for i, p := range rows {
		ids[i] = p.ID
	}
	allocs, err := q.AllocationsOfPlans(ctx, ids)
	if err != nil {
		return nil, err
	}
	byPlan := map[string][]domain.Allocation{}
	for _, a := range allocs {
		byPlan[a.PlanID] = append(byPlan[a.PlanID], domain.Allocation{Category: domain.Category(a.Category), Allocated: domain.Money(a.AllocatedCents)})
	}
	out := make([]domain.Plan, len(rows))
	for i, p := range rows {
		out[i] = toPlan(p, byPlan[p.ID])
	}
	return out, nil
}

func (s *Store) GetClaim(ctx context.Context, id string) (domain.Claim, error) {
	c, err := s.q.GetClaim(ctx, id)
	if err != nil {
		return domain.Claim{}, notFound(err)
	}
	return toClaim(c), nil
}

func (s *Store) GetClaims(ctx context.Context, ids []string) ([]domain.Claim, error) {
	rows, err := s.q.GetClaims(ctx, ids)
	return mapRows(rows, toClaim), err
}

func (s *Store) ClaimsOfPlan(ctx context.Context, planID string) ([]domain.Claim, error) {
	rows, err := s.q.ClaimsOfPlan(ctx, planID)
	return mapRows(rows, toClaim), err
}

// maxLimit caps a page; the app asks for at most 101 rows.
const maxLimit = 1000

// InsertClaimWithinBudget locks the plan row, reads its claims and bookings,
// runs check and inserts c in one transaction.
func (s *Store) InsertClaimWithinBudget(ctx context.Context, c domain.Claim, check app.BudgetCheck) error {
	return s.inTx(ctx, func(q *sqlcdb.Queries) error {
		if _, err := q.LockPlan(ctx, c.PlanID); err != nil {
			return notFound(err)
		}
		claims, err := q.ClaimsOfPlan(ctx, c.PlanID)
		if err != nil {
			return err
		}
		bookings, err := q.BookingsOfPlan(ctx, c.PlanID)
		if err != nil {
			return err
		}
		if err := check(mapRows(claims, toClaim), mapRows(bookings, toBooking)); err != nil {
			return err
		}
		return q.InsertClaim(ctx, insertClaimParams(c))
	})
}

// UpdateClaim is one conditional UPDATE of the status and payment: zero rows
// means the claim is gone or no longer in the expected status.
func (s *Store) UpdateClaim(ctx context.Context, c domain.Claim, from domain.ClaimStatus) error {
	n, err := s.q.UpdateClaim(ctx, updateClaimParams(c, from))
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	if _, err := s.q.GetClaim(ctx, c.ID); err != nil {
		return notFound(err)
	}
	return domain.ClaimNotInStatus(from)
}

func (s *Store) BookingsOfPlan(ctx context.Context, planID string) ([]domain.ServiceBooking, error) {
	rows, err := s.q.BookingsOfPlan(ctx, planID)
	return mapRows(rows, toBooking), err
}

func (s *Store) IsActiveDelegate(ctx context.Context, nomineeID, participantID string) (bool, error) {
	return s.q.IsActiveDelegate(ctx, sqlcdb.IsActiveDelegateParams{NomineeID: nomineeID, ParticipantID: participantID})
}

func (s *Store) DelegatedParticipants(ctx context.Context, nomineeID string) ([]string, error) {
	return s.q.DelegatedParticipants(ctx, nomineeID)
}

func (s *Store) BookedCategories(ctx context.Context, providerID, planID string) ([]domain.Category, error) {
	rows, err := s.q.BookedCategories(ctx, sqlcdb.BookedCategoriesParams{ProviderID: providerID, PlanID: planID})
	return mapRows(rows, func(c string) domain.Category { return domain.Category(c) }), err
}

func (s *Store) HasBookingForParticipant(ctx context.Context, providerID, participantID string) (bool, error) {
	return s.q.HasBookingForParticipant(ctx, sqlcdb.HasBookingForParticipantParams{ProviderID: providerID, ParticipantID: participantID})
}
