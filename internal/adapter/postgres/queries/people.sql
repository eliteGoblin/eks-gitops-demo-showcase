-- name: GetParticipant :one
SELECT * FROM participants WHERE id = $1;

-- name: GetParticipants :many
SELECT * FROM participants WHERE id = ANY(sqlc.arg(ids)::text[]);

-- name: GetProvider :one
SELECT * FROM providers WHERE id = $1;

-- name: GetProviders :many
SELECT * FROM providers WHERE id = ANY(sqlc.arg(ids)::text[]);

-- name: IsActiveDelegate :one
SELECT EXISTS (
    SELECT 1 FROM delegations WHERE nominee_id = $1 AND participant_id = $2 AND active
);

-- name: DelegatedParticipants :many
SELECT participant_id FROM delegations WHERE nominee_id = $1 AND active ORDER BY participant_id;

-- name: SearchParticipants :many
-- pattern is a LIKE pattern whose wildcards the store escaped (literal match).
SELECT * FROM participants
WHERE (sqlc.arg(all_scope)::boolean OR id = ANY(sqlc.arg(ids)::text[]))
  AND ((given_name || ' ' || family_name) ILIKE sqlc.arg(pattern)::text ESCAPE '\'
       OR scheme_number ILIKE sqlc.arg(pattern)::text ESCAPE '\')
ORDER BY family_name COLLATE "C", given_name COLLATE "C", id COLLATE "C"
LIMIT sqlc.arg(row_limit);

-- name: SearchProviders :many
SELECT * FROM providers
WHERE name ILIKE sqlc.arg(pattern)::text ESCAPE '\' OR abn ILIKE sqlc.arg(pattern)::text ESCAPE '\'
ORDER BY name COLLATE "C", id COLLATE "C"
LIMIT sqlc.arg(row_limit);
