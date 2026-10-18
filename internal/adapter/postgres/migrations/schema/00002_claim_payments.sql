-- Payment of an approved claim (payments-svc). Additive and nullable, so pods
-- still running the previous release keep working during a rolling update.

-- +goose Up
ALTER TABLE claims
    ADD COLUMN payment_status text CHECK (payment_status IN ('PENDING', 'PAID', 'FAILED')),
    ADD COLUMN payment_ref    text;

-- +goose Down
ALTER TABLE claims DROP COLUMN payment_ref, DROP COLUMN payment_status;
