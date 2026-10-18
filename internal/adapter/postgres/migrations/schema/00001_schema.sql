-- +goose Up
CREATE TABLE participants (
    id            text PRIMARY KEY,
    scheme_number text NOT NULL UNIQUE,
    given_name    text NOT NULL,
    family_name   text NOT NULL,
    date_of_birth date
);

CREATE TABLE providers (
    id   text PRIMARY KEY,
    name text NOT NULL,
    abn  text NOT NULL
);

CREATE TABLE plans (
    id             text PRIMARY KEY,
    participant_id text NOT NULL REFERENCES participants (id),
    status         text NOT NULL CHECK (status IN ('DRAFT', 'ACTIVE', 'EXPIRED')),
    start_date     date NOT NULL,
    end_date       date NOT NULL CHECK (end_date >= start_date)
);
CREATE INDEX plans_participant ON plans (participant_id);

CREATE TABLE plan_allocations (
    plan_id         text   NOT NULL REFERENCES plans (id),
    category        text   NOT NULL,
    allocated_cents bigint NOT NULL CHECK (allocated_cents >= 0),
    PRIMARY KEY (plan_id, category)
);

CREATE TABLE delegations (
    nominee_id     text    NOT NULL,
    participant_id text    NOT NULL REFERENCES participants (id),
    active         boolean NOT NULL,
    PRIMARY KEY (nominee_id, participant_id)
);

-- A booking reserves part of a category budget for a provider.
CREATE TABLE service_bookings (
    provider_id    text   NOT NULL REFERENCES providers (id),
    plan_id        text   NOT NULL REFERENCES plans (id),
    category       text   NOT NULL,
    reserved_cents bigint NOT NULL DEFAULT 0 CHECK (reserved_cents >= 0),
    PRIMARY KEY (provider_id, plan_id, category)
);
CREATE INDEX service_bookings_plan ON service_bookings (plan_id);

CREATE TABLE claims (
    id           text        PRIMARY KEY,
    plan_id      text        NOT NULL REFERENCES plans (id),
    provider_id  text        NOT NULL REFERENCES providers (id),
    category     text        NOT NULL,
    amount_cents bigint      NOT NULL CHECK (amount_cents > 0),
    status       text        NOT NULL CHECK (status IN ('SUBMITTED', 'APPROVED', 'REJECTED', 'PAID')),
    service_date date        NOT NULL,
    submitted_at timestamptz NOT NULL
);
-- Keyset pagination orders by (submitted_at, id).
CREATE INDEX claims_keyset ON claims (submitted_at, id);
CREATE INDEX claims_plan ON claims (plan_id);
CREATE INDEX claims_provider ON claims (provider_id);

CREATE TABLE support_requests (
    id                    text        PRIMARY KEY,
    plan_id               text        NOT NULL REFERENCES plans (id),
    participant_id        text        NOT NULL REFERENCES participants (id),
    requested_by          text        NOT NULL,
    requested_by_name     text        NOT NULL,
    category              text        NOT NULL,
    description           text        NOT NULL,
    estimated_cents       bigint      NOT NULL CHECK (estimated_cents > 0),
    preferred_provider_id text        REFERENCES providers (id),
    status                text        NOT NULL CHECK (status IN ('PENDING', 'APPROVED', 'REJECTED')),
    decided_by            text,
    decision_reason       text,
    booked_provider_id    text        REFERENCES providers (id),
    raised_at             timestamptz NOT NULL,
    decided_at            timestamptz
);
CREATE INDEX support_requests_queue ON support_requests (status, raised_at, id);
CREATE INDEX support_requests_participant ON support_requests (participant_id);
CREATE INDEX support_requests_requested_by ON support_requests (requested_by);

-- +goose Down
DROP TABLE support_requests, claims, service_bookings, delegations, plan_allocations, plans, providers, participants;
