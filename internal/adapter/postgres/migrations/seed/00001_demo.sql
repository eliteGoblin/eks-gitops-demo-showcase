-- Demo data set (applied only with `api migrate --seed`); mirrors internal/adapter/memory/seed.go.
-- Idempotent (ON CONFLICT DO NOTHING), so re-seeding a database that already has it is a no-op. Identifiers match the
-- Keycloak user attributes: zhaolin.sun -> P1, demo.guest -> P3,
-- sam.nominee -> N1 (acts for P1), riley.provider -> PR1.

-- +goose Up
INSERT INTO participants (id, scheme_number, given_name, family_name, date_of_birth) VALUES
    ('P1', 'SN100001', 'Zhaolin', 'Sun',   '1990-05-04'),
    ('P2', 'SN100002', 'Peter',   'Quinn', '1985-11-20'),
    ('P3', 'SN100003', 'Jordan',  'Lee',   '1996-08-12')
ON CONFLICT DO NOTHING;

INSERT INTO plans (id, participant_id, status, start_date, end_date) VALUES
    ('PL1', 'P1', 'ACTIVE', '2026-01-01', '2026-12-31'),
    ('PL2', 'P2', 'ACTIVE', '2026-03-01', '2027-02-28'),
    ('PL3', 'P3', 'ACTIVE', '2026-06-01', '2027-05-31')
ON CONFLICT DO NOTHING;

INSERT INTO plan_allocations (plan_id, category, allocated_cents) VALUES
    ('PL1', 'DAILY_LIVING', 1000000), ('PL1', 'THERAPY',   500000),
    ('PL2', 'DAILY_LIVING',  800000), ('PL2', 'EQUIPMENT', 250000),
    ('PL3', 'DAILY_LIVING',  600000), ('PL3', 'THERAPY',   300000)
ON CONFLICT DO NOTHING;

INSERT INTO providers (id, name, abn) VALUES
    ('PR1', 'Sunrise Care',    '12345678901'),
    ('PR2', 'Harbour Therapy', '98765432109')
ON CONFLICT DO NOTHING;

INSERT INTO service_bookings (provider_id, plan_id, category, reserved_cents) VALUES
    ('PR1', 'PL1', 'DAILY_LIVING', 100000)
ON CONFLICT DO NOTHING;

INSERT INTO delegations (nominee_id, participant_id, active) VALUES ('N1', 'P1', true)
ON CONFLICT DO NOTHING;

INSERT INTO claims (id, plan_id, provider_id, category, amount_cents, status, service_date, submitted_at) VALUES
    ('C1', 'PL1', 'PR1', 'DAILY_LIVING', 25000, 'APPROVED',  '2026-02-10', '2026-02-11T09:00:00Z'),
    ('C2', 'PL1', 'PR1', 'DAILY_LIVING', 18050, 'SUBMITTED', '2026-03-03', '2026-03-04T10:30:00Z'),
    ('C3', 'PL2', 'PR2', 'DAILY_LIVING', 40000, 'SUBMITTED', '2026-04-01', '2026-04-02T08:00:00Z')
ON CONFLICT DO NOTHING;

-- +goose Down
-- Scoped to the seeded plans, including anything raised or claimed on them since.
DELETE FROM support_requests WHERE plan_id IN ('PL1', 'PL2', 'PL3');
DELETE FROM claims WHERE plan_id IN ('PL1', 'PL2', 'PL3');
DELETE FROM service_bookings WHERE plan_id IN ('PL1', 'PL2', 'PL3');
DELETE FROM delegations WHERE nominee_id = 'N1' AND participant_id = 'P1';
DELETE FROM plan_allocations WHERE plan_id IN ('PL1', 'PL2', 'PL3');
DELETE FROM plans WHERE id IN ('PL1', 'PL2', 'PL3');
DELETE FROM providers WHERE id IN ('PR1', 'PR2');
DELETE FROM participants WHERE id IN ('P1', 'P2', 'P3');
