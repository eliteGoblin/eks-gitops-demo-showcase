-- name: InsertSupportRequest :exec
INSERT INTO support_requests (
    id, plan_id, participant_id, requested_by, requested_by_name, category, description,
    estimated_cents, preferred_provider_id, status, raised_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11);

-- name: GetSupportRequest :one
SELECT * FROM support_requests WHERE id = $1;

-- name: GetSupportRequests :many
SELECT * FROM support_requests WHERE id = ANY(sqlc.arg(ids)::text[]);

-- name: ListSupportRequests :many
SELECT * FROM support_requests
WHERE (sqlc.arg(all_scope)::boolean
       OR requested_by = sqlc.arg(requested_by)::text
       OR participant_id = ANY(sqlc.arg(participant_ids)::text[]))
  AND (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status)::text)
ORDER BY raised_at, id;

-- name: DecideSupportRequest :execrows
-- Only a PENDING request can be decided; 0 rows means someone decided it first.
UPDATE support_requests
SET status = $2, decided_by = $3, decision_reason = $4, booked_provider_id = $5, decided_at = $6
WHERE id = $1 AND status = 'PENDING';

-- name: SupportRequestStatus :one
SELECT status FROM support_requests WHERE id = $1;
