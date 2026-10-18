package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/postgres/sqlcdb"
	"github.com/eliteGoblin/eks-gitops-demo/internal/app"
	"github.com/eliteGoblin/eks-gitops-demo/internal/domain"
)

// transientCodes are the SQLSTATEs that say "try again" rather than "this
// request is wrong": a wait that ran out (the API sets lock_timeout and
// statement_timeout), an idle transaction cut off, a serialization conflict
// or a deadlock.
var transientCodes = map[string]bool{
	"55P03": true, // lock_not_available: lock_timeout
	"57014": true, // query_canceled: statement_timeout
	"25P03": true, // idle_in_transaction_session_timeout
	"40001": true, // serialization_failure
	"40P01": true, // deadlock_detected
}

// requestValueCodes are the data exceptions (SQLSTATE class 22) a request's
// own values can cause. Other class 22 errors, such as a division by zero,
// come from stored data or the server's own work and stay server errors.
var requestValueCodes = map[string]bool{
	"22021": true, // character_not_in_repertoire: a NUL byte or invalid UTF-8
	"22P05": true, // untranslatable_character
	"22P02": true, // invalid_text_representation: not a number, a UUID…
	"22001": true, // string_data_right_truncation: too long for the column
	"22003": true, // numeric_value_out_of_range
	"22007": true, // invalid_datetime_format
	"22008": true, // datetime_field_overflow
}

// refusedValue is a request value the database refused. Its text is fixed,
// because the database's message can quote the value; the original error
// stays in the chain and its SQLSTATE is available for the log.
type refusedValue struct {
	code  string
	cause error
}

func (e refusedValue) Error() string   { return "invalid input" }
func (e refusedValue) Unwrap() []error { return []error{domain.ErrInvalidInput, e.cause} }

// SQLState is the database's error code, safe to log: it names the kind of
// refusal without the value.
func (e refusedValue) SQLState() string { return e.code }

// classify marks transient database failures as app.ErrUnavailable and
// refused request values as domain.ErrInvalidInput, keeping the original
// error in the chain for the log; anything else is returned as is.
func classify(err error) error {
	var pgErr *pgconn.PgError
	switch {
	case !errors.As(err, &pgErr):
		return err
	case transientCodes[pgErr.Code]:
		return fmt.Errorf("%w: %w", app.ErrUnavailable, err)
	case requestValueCodes[pgErr.Code]:
		return refusedValue{code: pgErr.Code, cause: err}
	}
	return err
}

// classifying wraps the connection the sqlc queries run on, so every query
// result, wherever it fails (the statement, a row's Scan, the end of a result
// set), is classified in one place.
type classifying struct{ db sqlcdb.DBTX }

func (c classifying) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	tag, err := c.db.Exec(ctx, sql, args...)
	return tag, classify(err)
}

func (c classifying) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	rows, err := c.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, classify(err)
	}
	return classifyingRows{rows}, nil
}

func (c classifying) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return classifyingRow{c.db.QueryRow(ctx, sql, args...)}
}

type classifyingRows struct{ pgx.Rows }

func (r classifyingRows) Scan(dest ...any) error { return classify(r.Rows.Scan(dest...)) }
func (r classifyingRows) Err() error             { return classify(r.Rows.Err()) }

type classifyingRow struct{ pgx.Row }

func (r classifyingRow) Scan(dest ...any) error { return classify(r.Row.Scan(dest...)) }

// queries returns the sqlc queries on db with classified errors.
func queries(db sqlcdb.DBTX) *sqlcdb.Queries { return sqlcdb.New(classifying{db}) }
