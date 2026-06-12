-- +migrate Up
-- Per-instrument trading mode. trades_on_the_way marks instruments that
-- settle on a T+N cycle and may be traded while incoming/outgoing quantity is
-- still in transit (TradeControl board/section "trades on the way" flag).
-- Instruments without a row, or with trades_on_the_way = false, are treated as
-- immediate-settlement: future incoming/outgoing is NOT counted as tradable.
CREATE TABLE IF NOT EXISTS blnk.instrument_settings (
    id                BIGSERIAL PRIMARY KEY,
    instrument        TEXT      NOT NULL UNIQUE,
    venue             TEXT,
    trades_on_the_way BOOLEAN   NOT NULL DEFAULT FALSE,
    settle_offset     INT       NOT NULL DEFAULT 0,
    created_at        TIMESTAMP NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMP NOT NULL DEFAULT NOW()
);

-- +migrate Down
DROP TABLE IF EXISTS blnk.instrument_settings;
