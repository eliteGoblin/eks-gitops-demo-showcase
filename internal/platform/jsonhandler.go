package platform

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
)

// JSONHandler is a REST handler that returns its result instead of writing
// it: thin handlers decode, call the use case and return. JSON does the rest.
type JSONHandler func(w http.ResponseWriter, r *http.Request) (any, error)

// Response gives a JSONHandler result a status other than 200 (e.g. 201).
type Response struct {
	Status int
	Body   any
}

// Error is an error with the HTTP status and envelope code to answer with.
// Services map their domain errors to it; the kit's helpers return it directly.
type Error struct {
	Status  int
	Code    string
	Message string // safe to show the client
	Err     error  // the cause, for errors.Is/As and logs
}

func (e *Error) Error() string { return e.Message }
func (e *Error) Unwrap() error { return e.Err }

// ErrorMapper turns a service's domain error into an *Error; nil means the
// error is unexpected and becomes a masked 500.
type ErrorMapper func(error) *Error

// JSON adapts h to an http.HandlerFunc. A result is written as JSON (200, the
// Response's status, or 204 for nil). An error is answered in the error
// envelope: an *Error as is, a domain error via mapErr, anything else as a 500
// whose cause is logged with the request id but never shown to the client.
func JSON(log *slog.Logger, mapErr ErrorMapper, h JSONHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		result, err := h(w, r)
		if err != nil {
			writeHandlerError(w, r, log, mapErr, err)
			return
		}
		status, body := http.StatusOK, result
		if resp, ok := result.(Response); ok {
			status, body = resp.Status, resp.Body
		}
		if body == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}
}

func writeHandlerError(w http.ResponseWriter, r *http.Request, log *slog.Logger, mapErr ErrorMapper, err error) {
	var kitErr *Error
	if !errors.As(err, &kitErr) && mapErr != nil {
		kitErr = mapErr(err)
	}
	if kitErr == nil {
		log.ErrorContext(r.Context(), "request failed", "err", err)
		WriteError(w, r, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}
	WriteError(w, r, kitErr.Status, kitErr.Code, kitErr.Message)
}

// DecodeJSON decodes the request body into dst, rejecting unknown fields and
// malformed JSON with a 400 *Error.
func DecodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return &Error{Status: http.StatusBadRequest, Code: CodeBadUserInput, Message: "request body is not valid JSON for this endpoint", Err: err}
	}
	return nil
}
