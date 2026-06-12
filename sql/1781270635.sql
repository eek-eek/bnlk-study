-- +migrate Up
-- Re-key the future-bucket index from the T+N offset (settle_code) to the
-- absolute settle_date. Trades booked with an explicit settle_date carry
-- settle_code = NULL, so the old `WHERE settle_code IS NOT NULL` predicate
-- silently excluded them from in-transit aggregation and settlement scans.
DROP INDEX IF EXISTS blnk.idx_balances_settle_date;

-- +migrate Up
CREATE INDEX IF NOT EXISTS idx_balances_settle_date
    ON blnk.balances (settle_date)
    WHERE settle_date IS NOT NULL;

-- +migrate Down
DROP INDEX IF EXISTS blnk.idx_balances_settle_date;

-- +migrate Down
CREATE INDEX IF NOT EXISTS idx_balances_settle_date
    ON blnk.balances (settle_date)
    WHERE settle_code IS NOT NULL;
