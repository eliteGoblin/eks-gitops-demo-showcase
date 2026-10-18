package graphql

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/gqlerror"

	"github.com/eliteGoblin/eks-gitops-demo/internal/app"
	"github.com/eliteGoblin/eks-gitops-demo/internal/domain"
	"github.com/eliteGoblin/eks-gitops-demo/internal/platform"
)

// A client that goes away is not a server failure: its request is CANCELLED
// (a client outcome for the metrics and the error-rate alert), even when the
// database reports the statement it was running as cut short. A request that
// ran out of time, or storage that is busy, stays UNAVAILABLE (server side).
func TestPresenterSeparatesClientCancellationFromServerFailures(t *testing.T) {
	present := presenter(slog.New(slog.NewTextHandler(io.Discard, nil)))
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		name string
		ctx  context.Context
		err  error
		code string
	}{
		{"client cancelled", context.Background(), fmt.Errorf("query: %w", context.Canceled), platform.CodeCancelled},
		{"client gone while its statement was cut short", gone, fmt.Errorf("%w: %w", app.ErrUnavailable, errors.New("SQLSTATE 57014")), platform.CodeCancelled},
		{"request timed out", context.Background(), fmt.Errorf("query: %w", context.DeadlineExceeded), platform.CodeUnavailable},
		{"storage busy", context.Background(), fmt.Errorf("insert: %w", app.ErrUnavailable), platform.CodeUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := present(tc.ctx, tc.err)
			if got.Extensions["code"] != tc.code {
				t.Fatalf("code = %v, want %s (message %q)", got.Extensions["code"], tc.code, got.Message)
			}
		})
	}
}

// refusedValue mimics the storage adapter's error for a request value the
// database refused: fixed text, BAD_USER_INPUT, and a SQLSTATE for the log.
type refusedValue struct {
	code  string
	cause error
}

func (e refusedValue) Error() string    { return "invalid input" }
func (e refusedValue) Unwrap() []error  { return []error{domain.ErrInvalidInput, e.cause} }
func (e refusedValue) SQLState() string { return e.code }

// The database's message can quote the refused value (a NUL byte, invalid
// UTF-8), so it never reaches the client; the log keeps only the SQLSTATE.
func TestRefusedValuesHideTheDatabaseText(t *testing.T) {
	var logs bytes.Buffer
	present := presenter(slog.New(slog.NewTextHandler(&logs, nil)))
	cause := errors.New(`ERROR: invalid byte sequence for encoding "UTF8": 0x00 (SQLSTATE 22021)`)
	got := present(context.Background(), fmt.Errorf("query claims: %w", refusedValue{code: "22021", cause: cause}))
	if got.Extensions["code"] != platform.CodeBadUserInput {
		t.Fatalf("code = %v, want BAD_USER_INPUT", got.Extensions["code"])
	}
	if strings.Contains(got.Message, "SQLSTATE") || strings.Contains(got.Message, "ERROR:") || strings.Contains(got.Message, "0x00") {
		t.Fatalf("the response must not carry the database's text: %q", got.Message)
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "sqlstate=22021") || strings.Contains(logs.String(), "0x00") {
		t.Fatalf("want one WARN line with the SQLSTATE and no value, got:\n%s", logs.String())
	}
}

// A GraphQL error that wraps a Go error (a recovered panic, a failure outside
// any field), or one a resolver raised on its field, is still the server's:
// only gqlgen's own path-less, code-less transport errors read as a malformed
// request.
func TestServerErrorsAreNotMistakenForMalformedRequests(t *testing.T) {
	present := presenter(slog.New(slog.NewTextHandler(io.Discard, nil)))
	for name, err := range map[string]error{
		"wraps a Go error":  gqlerror.Wrap(errors.New("boom")),
		"raised on a field": gqlerror.ErrorPathf(ast.Path{ast.PathName("me")}, "resolver failed"),
	} {
		if got := present(context.Background(), err); got.Extensions["code"] != platform.CodeInternal {
			t.Errorf("%s: code = %v (%q), want INTERNAL", name, got.Extensions["code"], got.Message)
		}
	}
}
