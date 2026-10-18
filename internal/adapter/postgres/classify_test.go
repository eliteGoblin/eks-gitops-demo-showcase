package postgres

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/eliteGoblin/eks-gitops-demo/internal/app"
	"github.com/eliteGoblin/eks-gitops-demo/internal/domain"
)

// Waits that ran out, sessions cut off and serialization conflicts say "try
// again"; they surface as app.ErrUnavailable and keep the original error for
// the log. Every other failure passes through unchanged.
func TestClassifyMarksTransientFailuresUnavailable(t *testing.T) {
	for _, code := range []string{"55P03", "57014", "25P03", "40001", "40P01"} {
		err := classify(fmt.Errorf("query: %w", &pgconn.PgError{Code: code}))
		var pgErr *pgconn.PgError
		if !errors.Is(err, app.ErrUnavailable) || !errors.As(err, &pgErr) || pgErr.Code != code {
			t.Errorf("SQLSTATE %s: got %v, want app.ErrUnavailable wrapping the original", code, err)
		}
	}
	for _, err := range []error{&pgconn.PgError{Code: "23505"}, &pgconn.PgError{Code: "42P01"}, pgx.ErrNoRows, errors.New("other")} {
		if got := classify(err); got != err { //nolint:errorlint // identity: the error must pass through untouched
			t.Errorf("%v must pass through unchanged, got %v", err, got)
		}
	}
	if classify(nil) != nil {
		t.Error("nil must stay nil")
	}
}

// A data exception a request value can cause (a NUL byte or invalid UTF-8 in a
// parameter, text that is not a number or a date, a value out of range) means
// the request is at fault: it surfaces as domain.ErrInvalidInput
// (BAD_USER_INPUT) with fixed text, since the database's message can quote the
// value, and keeps the original and its SQLSTATE for the log.
func TestClassifyMarksRequestValueExceptionsInvalidInput(t *testing.T) {
	for _, code := range []string{"22021", "22P05", "22P02", "22001", "22003", "22007", "22008"} {
		err := classify(fmt.Errorf("query: %w", &pgconn.PgError{Code: code, Message: "value 0x00 refused"}))
		var pgErr *pgconn.PgError
		if !errors.Is(err, domain.ErrInvalidInput) || !errors.As(err, &pgErr) || pgErr.Code != code {
			t.Errorf("SQLSTATE %s: got %v, want domain.ErrInvalidInput wrapping the original", code, err)
		}
		if err.Error() != "invalid input" {
			t.Errorf("SQLSTATE %s: the text must not carry the database's message, got %q", code, err.Error())
		}
		var state interface{ SQLState() string }
		if !errors.As(err, &state) || state.SQLState() != code {
			t.Errorf("SQLSTATE %s: the code must be available for the log, got %v", code, err)
		}
		if errors.Is(err, app.ErrUnavailable) {
			t.Errorf("SQLSTATE %s is not transient", code)
		}
	}
}

// Other data exceptions (a division by zero, a null where one is not allowed)
// come from stored data or the server's own work, not the request's values:
// they stay server errors.
func TestClassifyLeavesOtherDataExceptionsAlone(t *testing.T) {
	for _, code := range []string{"22012", "22004", "2200F"} {
		err := classify(&pgconn.PgError{Code: code})
		if errors.Is(err, domain.ErrInvalidInput) {
			t.Errorf("SQLSTATE %s is not the request's fault, got %v", code, err)
		}
	}
}
