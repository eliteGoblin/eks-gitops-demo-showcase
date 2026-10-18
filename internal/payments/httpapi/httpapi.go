// Package httpapi is payments-svc's REST adapter: thin handlers (decode →
// use case → return) on the kit's JSON adapter, which encodes results and
// answers errors in the REST envelope.
package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/eliteGoblin/eks-gitops-demo/internal/payments"
	"github.com/eliteGoblin/eks-gitops-demo/internal/platform"
)

// CodeIdempotencyKeyReused is the error code for a key reused with a different body.
const CodeIdempotencyKeyReused = "IDEMPOTENCY_KEY_REUSED"

// Service is the subset of the payments use cases the API exposes.
type Service interface {
	Create(ctx context.Context, idempotencyKey string, req payments.Request) (payments.Payment, bool, error)
	Get(ctx context.Context, id string) (payments.Payment, error)
}

// Mount adds the payment routes to r. Authentication runs before them.
func Mount(r chi.Router, svc Service, log *slog.Logger) {
	h := handler{svc: svc, log: log}
	r.Post("/payments", platform.JSON(log, mapError, h.create))
	r.Get("/payments/{id}", platform.JSON(log, mapError, h.get))
}

type handler struct {
	svc Service
	log *slog.Logger
}

type paymentRequest struct {
	Reference   string `json:"reference"`
	Payee       string `json:"payee"`
	AmountCents int64  `json:"amountCents"`
	Currency    string `json:"currency"`
}

type paymentResponse struct {
	ID          string    `json:"id"`
	Reference   string    `json:"reference"`
	Payee       string    `json:"payee"`
	AmountCents int64     `json:"amountCents"`
	Currency    string    `json:"currency"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"createdAt"`
}

func toResponse(p payments.Payment) paymentResponse {
	return paymentResponse{
		ID: p.ID, Reference: p.Reference, Payee: p.Payee, AmountCents: p.AmountCents,
		Currency: p.Currency, Status: string(p.Status), CreatedAt: p.CreatedAt,
	}
}

// create answers 201 with a Location for a new payment, or 200 with
// Idempotent-Replayed for a retry of one already recorded.
func (h handler) create(w http.ResponseWriter, r *http.Request) (any, error) {
	var body paymentRequest
	if err := platform.DecodeJSON(r, &body); err != nil {
		return nil, err
	}
	req := payments.Request{Reference: body.Reference, Payee: body.Payee, AmountCents: body.AmountCents, Currency: body.Currency}
	key := r.Header.Get("Idempotency-Key")
	p, replayed, err := h.svc.Create(r.Context(), key, req)
	if err != nil {
		return nil, err
	}
	// The client is the token's azp: which service asked for the money.
	h.log.InfoContext(r.Context(), "payment recorded", "id", p.ID, "client", p.Owner, "reference", p.Reference,
		"idempotency_key", key, "amount_cents", p.AmountCents, "replayed", replayed)
	if replayed {
		w.Header().Set("Idempotent-Replayed", "true")
		return toResponse(p), nil
	}
	w.Header().Set("Location", "/payments/"+p.ID)
	return platform.Response{Status: http.StatusCreated, Body: toResponse(p)}, nil
}

func (h handler) get(_ http.ResponseWriter, r *http.Request) (any, error) {
	p, err := h.svc.Get(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		return nil, err
	}
	return toResponse(p), nil
}

// mapError maps the use cases' errors to statuses; anything else is a masked 500.
func mapError(err error) *platform.Error {
	var invalid *payments.ValidationError
	switch {
	case errors.As(err, &invalid):
		return &platform.Error{Status: http.StatusBadRequest, Code: platform.CodeBadUserInput, Message: invalid.Error(), Err: err}
	case errors.Is(err, payments.ErrForbidden):
		return &platform.Error{Status: http.StatusForbidden, Code: platform.CodeForbidden, Message: "forbidden", Err: err}
	case errors.Is(err, payments.ErrNotFound):
		return &platform.Error{Status: http.StatusNotFound, Code: platform.CodeNotFound, Message: "payment not found", Err: err}
	case errors.Is(err, payments.ErrKeyReused):
		return &platform.Error{Status: http.StatusConflict, Code: CodeIdempotencyKeyReused, Message: err.Error(), Err: err}
	case errors.Is(err, payments.ErrStoreFull):
		return &platform.Error{Status: http.StatusServiceUnavailable, Code: platform.CodeUnavailable, Message: "try again later", Err: err}
	}
	return nil
}
