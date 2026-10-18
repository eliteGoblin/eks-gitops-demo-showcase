package graphql_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/graphql"
	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/memory"
	"github.com/eliteGoblin/eks-gitops-demo/internal/app"
	"github.com/eliteGoblin/eks-gitops-demo/internal/platform"
)

// postRaw sends body exactly as given (it may not be valid JSON) as staff.
func postRaw(t *testing.T, h http.Handler, body string) (status int, raw string, r result) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(app.WithPrincipal(req.Context(), *staff))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return rec.Code, rec.Body.String(), r
}

// A body the transport cannot read (malformed JSON, a wrong type) is the
// caller's mistake: BAD_USER_INPUT with a fixed message. Neither the response
// nor the log echoes the body, which may carry personal data in its variables.
func TestMalformedRequestsAreClientErrorsAndNotLogged(t *testing.T) {
	var logs bytes.Buffer
	st := memory.New()
	memory.Seed(st)
	h := serverWithOptions(t, st, nil, func(o *graphql.Options) { o.Log = slog.New(slog.NewTextHandler(&logs, nil)) })
	const private = "patient-note-123-45-6789"
	for _, body := range []string{
		`{"query":`,
		`not json ` + private,
		`{"query":123}`,
		`[1,2]`,
		`{"query":"{ me { subject } }","variables":"` + private + `"}`,
		`{"query":"{ me { subject } }","extensions":[1]}`,
	} {
		status, raw, r := postRaw(t, h, body)
		if code(r) != platform.CodeBadUserInput || r.Errors[0].Message != "malformed request" {
			t.Errorf("%q: status %d, want BAD_USER_INPUT \"malformed request\", got %s", body, status, raw)
		}
		if strings.Contains(raw, private) || strings.Contains(raw, "could not be decoded") {
			t.Errorf("%q: the response must not echo the body or the decoder: %s", body, raw)
		}
	}
	if strings.Contains(logs.String(), "level=ERROR") || strings.Contains(logs.String(), private) {
		t.Fatalf("a malformed request is not a server error and its body stays out of the log:\n%s", logs.String())
	}
}
