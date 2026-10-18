-- Idempotent claim submission and claim listings sorted by amount.
-- Additive, so pods still running the previous release keep working during a
-- rolling update.

-- +goose Up
-- A client's idempotency key, scoped to the caller (the token subject), and
-- the claim its first request created. Rows are kept (at least the 24 hours
-- the API promises).
CREATE TABLE idempotency_keys (
    subject     text        NOT NULL,
    key         text        NOT NULL,
    fingerprint text        NOT NULL,
    claim_id    text        NOT NULL REFERENCES claims (id),
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (subject, key)
);

-- Keyset pagination in both directions; ids compare byte-wise (COLLATE "C"),
-- the same order the memory adapter uses.
CREATE INDEX claims_submitted_keyset ON claims (submitted_at, id COLLATE "C");
CREATE INDEX claims_amount_keyset ON claims (amount_cents, id COLLATE "C");

-- +goose Down
DROP INDEX claims_amount_keyset;
DROP INDEX claims_submitted_keyset;
DROP TABLE idempotency_keys;
