-- name: GetPlan :one
SELECT * FROM plans WHERE id = $1;

-- name: LockPlan :one
-- Serializes budget-changing writes on one plan for the rest of the transaction.
-- NO KEY UPDATE: rows that merely reference the plan (claims, support requests)
-- can still be inserted meanwhile; only other budget writers wait.
SELECT id FROM plans WHERE id = $1 FOR NO KEY UPDATE;

-- name: GetPlans :many
SELECT * FROM plans WHERE id = ANY(sqlc.arg(ids)::text[]);

-- name: ListPlansByParticipant :many
SELECT * FROM plans WHERE participant_id = $1 ORDER BY id;

-- name: AllocationsOfPlans :many
SELECT * FROM plan_allocations WHERE plan_id = ANY(sqlc.arg(plan_ids)::text[]) ORDER BY plan_id, category;

-- name: BookingsOfPlan :many
SELECT * FROM service_bookings WHERE plan_id = $1 ORDER BY provider_id, category;

-- name: BookedCategories :many
SELECT category FROM service_bookings WHERE provider_id = $1 AND plan_id = $2 ORDER BY category;

-- name: HasBookingForParticipant :one
SELECT EXISTS (
    SELECT 1 FROM service_bookings b JOIN plans p ON p.id = b.plan_id
    WHERE b.provider_id = $1 AND p.participant_id = $2
);

-- name: BookedParticipants :many
SELECT DISTINCT p.participant_id FROM service_bookings b JOIN plans p ON p.id = b.plan_id
WHERE b.provider_id = $1
ORDER BY p.participant_id;

-- name: SetBooking :exec
-- Stores a provider's cumulative reservation as computed by the app under the
-- plan lock (domain.Plan.Reserve), so the value is absolute, not an increment.
INSERT INTO service_bookings (provider_id, plan_id, category, reserved_cents)
VALUES ($1, $2, $3, $4)
ON CONFLICT (provider_id, plan_id, category)
DO UPDATE SET reserved_cents = EXCLUDED.reserved_cents;
