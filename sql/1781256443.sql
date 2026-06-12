-- +migrate Up
-- Re-key the brokerage position uniqueness from the T+N offset (settle_code) to
-- the absolute settle_date, so multi-day accounting is correct: trades with the
-- same offset on different trade dates form distinct buckets, and the offset N
-- may be uncontrolled (NULL) as long as the settlement date is known.
DROP INDEX IF EXISTS blnk.idx_balances_position_key;

-- +migrate Up
CREATE UNIQUE INDEX IF NOT EXISTS idx_balances_position_key
    ON blnk.balances (ledger_id, COALESCE(identity_id, ''), account_ref,
                      COALESCE(instrument, ''), currency,
                      COALESCE(settle_date, DATE '1970-01-01'))
    WHERE account_ref IS NOT NULL;

-- +migrate Down
DROP INDEX IF EXISTS blnk.idx_balances_position_key;

-- +migrate Down
CREATE UNIQUE INDEX IF NOT EXISTS idx_balances_position_key
    ON blnk.balances (ledger_id, COALESCE(identity_id, ''), account_ref,
                      COALESCE(instrument, ''), currency, COALESCE(settle_code, -1))
    WHERE account_ref IS NOT NULL;
