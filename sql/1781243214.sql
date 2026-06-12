-- +migrate Up
ALTER TABLE blnk.balances ADD COLUMN IF NOT EXISTS account_ref TEXT;

-- +migrate Up
ALTER TABLE blnk.balances ADD COLUMN IF NOT EXISTS instrument TEXT;

-- +migrate Up
ALTER TABLE blnk.balances ADD COLUMN IF NOT EXISTS settle_date DATE;

-- +migrate Up
ALTER TABLE blnk.balances ADD COLUMN IF NOT EXISTS settle_code INT;

-- +migrate Up
ALTER TABLE blnk.balances ADD COLUMN IF NOT EXISTS wa_price NUMERIC(34, 2);

-- +migrate Up
-- One active brokerage balance per position key. Mirrors the TradeControl
-- invariant "too many records in the balances table" but enforced by the
-- schema instead of a runtime check. Only brokerage-managed balances
-- (account_ref IS NOT NULL) are constrained; native blnk balances are not
-- affected.
CREATE UNIQUE INDEX IF NOT EXISTS idx_balances_position_key
    ON blnk.balances (ledger_id, COALESCE(identity_id, ''), account_ref,
                      COALESCE(instrument, ''), currency, COALESCE(settle_code, -1))
    WHERE account_ref IS NOT NULL;

-- +migrate Up
CREATE INDEX IF NOT EXISTS idx_balances_settle_date
    ON blnk.balances (settle_date)
    WHERE settle_code IS NOT NULL;

-- +migrate Up
-- Purchase lots per security balance (TradeControl BalanceDetail analog).
-- Quantity is stored in precise minor units together with its precision
-- factor, price is the per-unit execution price.
CREATE TABLE IF NOT EXISTS blnk.balance_lots (
    id           BIGSERIAL PRIMARY KEY,
    lot_id       TEXT      NOT NULL UNIQUE,
    balance_id   TEXT      NOT NULL REFERENCES blnk.balances (balance_id),
    instrument   TEXT      NOT NULL,
    quantity     BIGINT    NOT NULL,
    precision    BIGINT    NOT NULL DEFAULT 1,
    price        NUMERIC(34, 8) NOT NULL,
    currency     TEXT      NOT NULL,
    reference    TEXT,
    purchased_at TIMESTAMP NOT NULL DEFAULT NOW(),
    created_at   TIMESTAMP NOT NULL DEFAULT NOW()
);

-- +migrate Up
CREATE INDEX IF NOT EXISTS idx_balance_lots_balance_id
    ON blnk.balance_lots (balance_id);

-- +migrate Up
-- Trading venue holidays used for settle date (T+N) computation
-- (TradeControl HolidayService analog).
CREATE TABLE IF NOT EXISTS blnk.market_holidays (
    id           BIGSERIAL PRIMARY KEY,
    venue        TEXT      NOT NULL,
    holiday_date DATE      NOT NULL,
    description  TEXT,
    created_at   TIMESTAMP NOT NULL DEFAULT NOW(),
    UNIQUE (venue, holiday_date)
);

-- +migrate Down
DROP TABLE IF EXISTS blnk.market_holidays;

-- +migrate Down
DROP TABLE IF EXISTS blnk.balance_lots;

-- +migrate Down
DROP INDEX IF EXISTS blnk.idx_balances_settle_date;

-- +migrate Down
DROP INDEX IF EXISTS blnk.idx_balances_position_key;

-- +migrate Down
ALTER TABLE blnk.balances DROP COLUMN IF EXISTS wa_price;

-- +migrate Down
ALTER TABLE blnk.balances DROP COLUMN IF EXISTS settle_code;

-- +migrate Down
ALTER TABLE blnk.balances DROP COLUMN IF EXISTS settle_date;

-- +migrate Down
ALTER TABLE blnk.balances DROP COLUMN IF EXISTS instrument;

-- +migrate Down
ALTER TABLE blnk.balances DROP COLUMN IF EXISTS account_ref;
