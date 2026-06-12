-- +migrate Up
-- Two-phase settlement journal: makes the post-commit side effects of a trade
-- settlement (weighted-average price update + purchase lot) atomic and
-- recoverable. A 'pending' row is written before the inflight legs are
-- committed (capturing wa_before/qty_before so wa_after is deterministic), and
-- flipped to 'applied' in the same DB transaction that writes the lot and the
-- wa_price. A crash between the commit and the side effects leaves a 'pending'
-- row that ReconcileSettlement completes idempotently.
CREATE TABLE IF NOT EXISTS blnk.brokerage_settlement_journal (
    security_txn_id TEXT PRIMARY KEY,
    trade_ref       TEXT,
    spot_balance_id TEXT NOT NULL,
    instrument      TEXT,
    side            TEXT,
    wa_before       NUMERIC,
    qty_before      NUMERIC,
    price           NUMERIC,
    quantity        NUMERIC,
    precision       BIGINT,
    currency        TEXT,
    wa_after        NUMERIC,
    lot_id          TEXT,
    status          TEXT NOT NULL DEFAULT 'pending',
    created_at      TIMESTAMP NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMP NOT NULL DEFAULT NOW()
);

-- +migrate Up
CREATE INDEX IF NOT EXISTS idx_settlement_journal_pending
    ON blnk.brokerage_settlement_journal (status)
    WHERE status = 'pending';

-- +migrate Down
DROP TABLE IF EXISTS blnk.brokerage_settlement_journal;
