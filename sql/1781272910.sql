-- +migrate Up
-- Configurable status / reference dictionary (TradeControl `Dict` analog): lets
-- statuses, sides and types be data-driven instead of hard enums.
CREATE TABLE IF NOT EXISTS blnk.dict (
    category   TEXT    NOT NULL,
    code       TEXT    NOT NULL,
    name       TEXT    NOT NULL,
    sort       INT     NOT NULL DEFAULT 0,
    active     BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMP NOT NULL DEFAULT NOW(),
    PRIMARY KEY (category, code)
);

-- +migrate Up
-- Seed the canonical order lifecycle statuses (operators may add their own).
INSERT INTO blnk.dict (category, code, name, sort) VALUES
    ('order_status', 'DRAFT',     'Draft',     10),
    ('order_status', 'CHECKED',   'Checked',   20),
    ('order_status', 'APPROVED',  'Approved',  30),
    ('order_status', 'EXECUTED',  'Executed',  40),
    ('order_status', 'REJECTED',  'Rejected',  90),
    ('order_status', 'CANCELLED', 'Cancelled', 91),
    ('order_side',   'buy',       'Buy',        1),
    ('order_side',   'sell',      'Sell',       2)
ON CONFLICT (category, code) DO NOTHING;

-- +migrate Up
-- Orders: the decision/lifecycle entity in front of trade booking. Quantity and
-- price are NUMERIC (read as text -> decimal); precisions are integers.
CREATE TABLE IF NOT EXISTS blnk.orders (
    order_id              TEXT    PRIMARY KEY,
    reference             TEXT    NOT NULL UNIQUE,
    ledger_id             TEXT    NOT NULL,
    identity_id           TEXT,
    account_ref           TEXT    NOT NULL,
    instrument            TEXT    NOT NULL,
    venue                 TEXT,
    currency              TEXT    NOT NULL,
    side                  TEXT    NOT NULL,
    quantity              NUMERIC NOT NULL,
    quantity_precision    INT     NOT NULL DEFAULT 1,
    price                 NUMERIC NOT NULL,
    money_precision       INT     NOT NULL DEFAULT 100,
    settle_offset         INT     NOT NULL DEFAULT 0,
    settle_date           DATE,
    status                TEXT    NOT NULL DEFAULT 'DRAFT',
    status_message        TEXT,
    reject_reason         TEXT,
    aml_status            TEXT,
    settlement_balance_id TEXT,
    market_balance_id     TEXT,
    trade_ref             TEXT,
    security_txn_id       TEXT,
    money_txn_id          TEXT,
    created_at            TIMESTAMP NOT NULL DEFAULT NOW(),
    updated_at            TIMESTAMP NOT NULL DEFAULT NOW()
);

-- +migrate Up
CREATE INDEX IF NOT EXISTS idx_orders_status ON blnk.orders (status);

-- +migrate Up
-- Stop-list: blocks an identity from trading, optionally scoped to one
-- instrument (instrument = '' blocks all). TradeControl StopList analog.
CREATE TABLE IF NOT EXISTS blnk.stop_list (
    id          BIGSERIAL PRIMARY KEY,
    identity_id TEXT NOT NULL,
    instrument  TEXT NOT NULL DEFAULT '',
    reason      TEXT,
    active      BOOLEAN NOT NULL DEFAULT TRUE,
    created_at  TIMESTAMP NOT NULL DEFAULT NOW(),
    UNIQUE (identity_id, instrument)
);

-- +migrate Up
-- Trading time windows per venue/weekday (0=Sunday..6=Saturday). When a venue
-- has no rows, the trading-time check is skipped (always open). TradeControl
-- TradingTime analog.
CREATE TABLE IF NOT EXISTS blnk.trading_times (
    id          BIGSERIAL PRIMARY KEY,
    venue       TEXT NOT NULL,
    weekday     INT  NOT NULL,
    open_time   TEXT NOT NULL,  -- 'HH:MM'
    close_time  TEXT NOT NULL,  -- 'HH:MM'
    created_at  TIMESTAMP NOT NULL DEFAULT NOW(),
    UNIQUE (venue, weekday)
);

-- +migrate Up
-- Per-instrument order size limits (optional). NULL = no bound.
ALTER TABLE blnk.instrument_settings ADD COLUMN IF NOT EXISTS min_order_quantity NUMERIC;
-- +migrate Up
ALTER TABLE blnk.instrument_settings ADD COLUMN IF NOT EXISTS max_order_quantity NUMERIC;

-- +migrate Down
ALTER TABLE blnk.instrument_settings DROP COLUMN IF EXISTS max_order_quantity;
-- +migrate Down
ALTER TABLE blnk.instrument_settings DROP COLUMN IF EXISTS min_order_quantity;
-- +migrate Down
DROP TABLE IF EXISTS blnk.trading_times;
-- +migrate Down
DROP TABLE IF EXISTS blnk.stop_list;
-- +migrate Down
DROP TABLE IF EXISTS blnk.orders;
-- +migrate Down
DROP TABLE IF EXISTS blnk.dict;
