-- +migrate Up
-- Idempotent purchase lots: a settlement retry (or the journal recovery path)
-- must not create duplicate lots for the same trade. The reference carries the
-- trade ref, so one lot per (balance, trade) is enforced.
CREATE UNIQUE INDEX IF NOT EXISTS uq_balance_lots_balance_reference
    ON blnk.balance_lots (balance_id, reference)
    WHERE reference IS NOT NULL AND reference <> '';

-- +migrate Down
DROP INDEX IF EXISTS blnk.uq_balance_lots_balance_reference;
