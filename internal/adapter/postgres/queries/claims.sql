-- name: GetClaim :one
SELECT * FROM claims WHERE id = $1;

-- name: GetClaims :many
SELECT * FROM claims WHERE id = ANY(sqlc.arg(ids)::text[]);

-- name: ClaimsOfPlan :many
SELECT * FROM claims WHERE plan_id = $1 ORDER BY submitted_at, id;

-- name: InsertClaim :exec
INSERT INTO claims (id, plan_id, provider_id, category, amount_cents, status, service_date, submitted_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: UpdateClaim :execrows
-- Status and payment in one write, conditional on the current status, so two
-- concurrent transitions cannot both win.
UPDATE claims
SET status = sqlc.arg(status), payment_status = sqlc.narg(payment_status), payment_ref = sqlc.narg(payment_ref)
WHERE id = sqlc.arg(id) AND status = sqlc.arg(from_status);

-- name: GetIdempotencyKey :one
SELECT claim_id, fingerprint FROM idempotency_keys WHERE subject = $1 AND key = $2;

-- name: InsertIdempotencyKey :exec
INSERT INTO idempotency_keys (subject, key, fingerprint, claim_id) VALUES ($1, $2, $3, $4);
