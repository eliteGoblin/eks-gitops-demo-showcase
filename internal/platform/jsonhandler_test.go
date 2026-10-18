package platform

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

var errNoSuchThing = errors.New("no such thing")

func mapTestErrors(err error) *Error {
	if errors.Is(err, errNoSuchThing) {
		return &Error{Status: http.StatusNotFound, Code: CodeNotFound, Message: "thing not found"}
	}
	return nil
}

func TestJSONAdapter(t *testing.T) {
	tests := []struct {
		name       string
		handler    JSONHandler
		body       string
		wantStatus int
		wantBody   string // substring
	}{
		{"value → 200 JSON", func(http.ResponseWriter, *http.Request) (any, error) { return map[string]int{"n": 1}, nil }, "", http.StatusOK, `{"n":1}`},
		{"Response sets the status", func(http.ResponseWriter, *http.Request) (any, error) {
			return Response{Status: http.StatusCreated, Body: map[string]string{"id": "x"}}, nil
		}, "", http.StatusCreated, `{"id":"x"}`},
		{"nil result → 204", func(http.ResponseWriter, *http.Request) (any, error) { return nil, nil }, "", http.StatusNoContent, ""},
		{"mapped domain error → its status, never 200", func(http.ResponseWriter, *http.Request) (any, error) {
			return nil, errors.Join(errors.New("ctx"), errNoSuchThing)
		}, "", http.StatusNotFound, `"code":"NOT_FOUND"`},
		{"kit error passes through", func(http.ResponseWriter, *http.Request) (any, error) {
			return nil, &Error{Status: http.StatusConflict, Code: CodeConflict, Message: "taken"}
		}, "", http.StatusConflict, `"message":"taken"`},
		{"unexpected error → masked 500 with request id", func(http.ResponseWriter, *http.Request) (any, error) {
			return nil, errors.New("db password is hunter2")
		}, "", http.StatusInternalServerError, `"request_id":"req-9"`},
		{"DecodeJSON rejects unknown fields with 400", func(_ http.ResponseWriter, r *http.Request) (any, error) {
			var dst struct{ A int }
			return nil, DecodeJSON(r, &dst)
		}, `{"A":1,"B":2}`, http.StatusBadRequest, `"code":"BAD_USER_INPUT"`},
		{"DecodeJSON rejects malformed JSON with 400", func(_ http.ResponseWriter, r *http.Request) (any, error) {
			var dst struct{ A int }
			return nil, DecodeJSON(r, &dst)
		}, `{"A":`, http.StatusBadRequest, `"code":"BAD_USER_INPUT"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			h := JSON(slog.New(slog.NewJSONHandler(&logs, nil)), mapTestErrors, tc.handler)
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body))
			r = r.WithContext(WithErrorFormat(WithRequestID(r.Context(), "req-9"), RESTErrors))
			rec := httptest.NewRecorder()
			h(rec, r)
			if rec.Code != tc.wantStatus || !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Fatalf("got %d %s, want %d containing %s", rec.Code, rec.Body, tc.wantStatus, tc.wantBody)
			}
			if tc.wantStatus == http.StatusInternalServerError {
				if strings.Contains(rec.Body.String(), "hunter2") || !strings.Contains(logs.String(), "hunter2") {
					t.Fatalf("a 500 must hide the cause from the client and log it: body %s, log %s", rec.Body, logs.String())
				}
			}
			if tc.wantStatus < 300 && tc.wantStatus != http.StatusNoContent {
				var v any
				if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil || rec.Header().Get("Content-Type") != "application/json" {
					t.Fatalf("success must be a JSON body: %v %q", err, rec.Header().Get("Content-Type"))
				}
			}
		})
	}
}

func TestErrorUnwraps(t *testing.T) {
	e := &Error{Status: http.StatusBadRequest, Code: CodeBadUserInput, Message: "bad", Err: errNoSuchThing}
	if !errors.Is(e, errNoSuchThing) || e.Error() != "bad" {
		t.Fatalf("Error must unwrap its cause and print its message: %v", e)
	}
}
