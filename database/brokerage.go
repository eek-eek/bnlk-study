/*
Copyright 2024 Blnk Finance Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Brokerage data access: position balances keyed by
// (ledger, identity, account_ref, instrument, currency, settle_code),
// purchase lots, venue holidays, atomic multi-key delta application and
// hold recomputation. This is the persistence half of the TradeControl
// balance subsystem port (BalanceMutationDaoJdbc, HolidayService storage,
// BalanceDetail).
package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math/big"
	"time"

	"github.com/blnkfinance/blnk/internal/apierror"
	"github.com/blnkfinance/blnk/model"
	"github.com/lib/pq"
)

// positionSelectColumns is the column list shared by all position reads.
// Numeric columns are cast to text so values are parsed through big.Int,
// keeping the reads correct even if the column types are widened later.
const positionSelectColumns = `
    b.balance_id, b.balance::text, b.credit_balance::text, b.debit_balance::text,
    b.inflight_balance::text, b.inflight_credit_balance::text, b.inflight_debit_balance::text,
    b.currency, b.ledger_id, COALESCE(b.identity_id, '') as identity_id, b.created_at, b.version,
    COALESCE(b.account_ref, '') as account_ref, COALESCE(b.instrument, '') as instrument,
    b.settle_date, b.settle_code, b.wa_price::text`

// positionKeyPredicate matches a balance row against a model.PositionKey using
// the same COALESCE normalization as the idx_balances_position_key index. The
// settlement dimension is the absolute settle_date (nil/spot -> sentinel),
// making buckets multi-day correct independent of the T+N offset.
const positionKeyPredicate = `
    b.ledger_id = $1 AND COALESCE(b.identity_id, '') = $2 AND b.account_ref = $3
    AND COALESCE(b.instrument, '') = $4 AND b.currency = $5
    AND COALESCE(b.settle_date, DATE '1970-01-01') = COALESCE($6::date, DATE '1970-01-01')`

// rowScanner abstracts *sql.Row and *sql.Rows for the shared scan helper.
type rowScanner interface {
	Scan(dest ...interface{}) error
}

// scanPositionRow scans one position row produced with positionSelectColumns.
func scanPositionRow(row rowScanner) (*model.Balance, error) {
	balance := &model.Balance{}
	var balanceStr, creditStr, debitStr, inflightStr, inflightCreditStr, inflightDebitStr string
	var settleDate sql.NullTime
	var settleCode sql.NullInt64
	var waPrice sql.NullString

	err := row.Scan(
		&balance.BalanceID, &balanceStr, &creditStr, &debitStr,
		&inflightStr, &inflightCreditStr, &inflightDebitStr,
		&balance.Currency, &balance.LedgerID, &balance.IdentityID, &balance.CreatedAt, &balance.Version,
		&balance.AccountRef, &balance.Instrument,
		&settleDate, &settleCode, &waPrice,
	)
	if err != nil {
		return nil, err
	}

	for dst, src := range map[**big.Int]string{
		&balance.Balance:               balanceStr,
		&balance.CreditBalance:         creditStr,
		&balance.DebitBalance:          debitStr,
		&balance.InflightBalance:       inflightStr,
		&balance.InflightCreditBalance: inflightCreditStr,
		&balance.InflightDebitBalance:  inflightDebitStr,
	} {
		value, ok := new(big.Int).SetString(src, 10)
		if !ok {
			return nil, fmt.Errorf("invalid balance amount %q", src)
		}
		*dst = value
	}

	if settleDate.Valid {
		d := settleDate.Time
		balance.SettleDate = &d
	}
	if settleCode.Valid {
		c := int(settleCode.Int64)
		balance.SettleCode = &c
	}
	if waPrice.Valid {
		balance.WAPrice = waPrice.String
	}
	return balance, nil
}

// positionKeyArgs flattens a PositionKey into query arguments matching
// positionKeyPredicate. A nil settle date is passed as NULL so the COALESCE
// resolves it to the spot sentinel.
func positionKeyArgs(key model.PositionKey) []interface{} {
	var settleDate interface{}
	if key.SettleDate != nil {
		settleDate = *key.SettleDate
	}
	return []interface{}{key.LedgerID, key.IdentityID, key.AccountRef, key.Instrument, key.Currency, settleDate}
}

// GetPosition retrieves the position balance exactly matching the key.
func (d Datasource) GetPosition(ctx context.Context, key model.PositionKey) (*model.Balance, error) {
	row := d.Conn.QueryRowContext(ctx, `
        SELECT `+positionSelectColumns+`
        FROM blnk.balances b
        WHERE `+positionKeyPredicate, positionKeyArgs(key)...)

	balance, err := scanPositionRow(row)
	if err == sql.ErrNoRows {
		return nil, apierror.NewAPIError(apierror.ErrNotFound, fmt.Sprintf("Position not found for key '%s'", key.LockKey()), err)
	}
	if err != nil {
		return nil, apierror.NewAPIError(apierror.ErrInternalServer, "Failed to scan position", err)
	}
	return balance, nil
}

// FindOrCreatePosition returns the position balance for the key, creating it
// if it does not exist (TradeControl createClearBalanceForRecalculate /
// findOrCreateAndLock analog). The bucket is identified by the absolute
// settle date carried on the key (nil = spot); the optional settle_code offset
// is stored for reference only.
func (d Datasource) FindOrCreatePosition(ctx context.Context, key model.PositionKey) (*model.Balance, error) {
	if err := key.Validate(); err != nil {
		return nil, apierror.NewAPIError(apierror.ErrBadRequest, err.Error(), err)
	}

	existing, err := d.GetPosition(ctx, key)
	if err == nil {
		return existing, nil
	}
	if apiErr, ok := err.(apierror.APIError); !ok || apiErr.Code != apierror.ErrNotFound {
		return nil, err
	}

	var identityID interface{} = key.IdentityID
	if key.IdentityID == "" {
		identityID = nil
	}
	var instrument interface{} = key.Instrument
	if key.Instrument == "" {
		instrument = nil
	}
	var settleCode interface{}
	if key.SettleCode != nil {
		settleCode = *key.SettleCode
	}
	var settleDateArg interface{}
	if key.SettleDate != nil {
		settleDateArg = *key.SettleDate
	}

	balanceID := model.GenerateUUIDWithSuffix("bln")
	// ON CONFLICT DO NOTHING makes concurrent creators converge on the row
	// guarded by idx_balances_position_key; the loser re-reads below.
	_, err = d.Conn.ExecContext(ctx, `
        INSERT INTO blnk.balances
            (balance_id, balance, credit_balance, debit_balance,
             inflight_balance, inflight_credit_balance, inflight_debit_balance,
             currency, ledger_id, identity_id, created_at, meta_data,
             account_ref, instrument, settle_date, settle_code)
        VALUES ($1, 0, 0, 0, 0, 0, 0, $2, $3, $4, $5, '{}', $6, $7, $8, $9)
        ON CONFLICT DO NOTHING`,
		balanceID, key.Currency, key.LedgerID, identityID, time.Now(),
		key.AccountRef, instrument, settleDateArg, settleCode)
	if err != nil {
		if pqErr, ok := err.(*pq.Error); ok && pqErr.Code.Name() == "foreign_key_violation" {
			return nil, apierror.NewAPIError(apierror.ErrBadRequest, "Invalid ledger ID", err)
		}
		return nil, apierror.NewAPIError(apierror.ErrInternalServer, "Failed to create position", err)
	}

	return d.GetPosition(ctx, key)
}

// GetPositionByID retrieves a balance by ID including the brokerage extension
// columns (account_ref, instrument, settle fields, wa_price).
func (d Datasource) GetPositionByID(ctx context.Context, balanceID string) (*model.Balance, error) {
	row := d.Conn.QueryRowContext(ctx, `
        SELECT `+positionSelectColumns+`
        FROM blnk.balances b
        WHERE b.balance_id = $1`, balanceID)

	balance, err := scanPositionRow(row)
	if err == sql.ErrNoRows {
		return nil, apierror.NewAPIError(apierror.ErrNotFound, fmt.Sprintf("Balance with ID '%s' not found", balanceID), err)
	}
	if err != nil {
		return nil, apierror.NewAPIError(apierror.ErrInternalServer, "Failed to scan position", err)
	}
	return balance, nil
}

// GetActivePosition resolves the active balance for the dimensions using the
// TradeControl settle cascade by absolute date: the most-forward bucket whose
// settle_date is on or before maxSettleDate wins, falling back to the spot
// balance (settle_date IS NULL). maxSettleDate == nil restricts the lookup to
// the spot balance.
func (d Datasource) GetActivePosition(ctx context.Context, ledgerID, identityID, accountRef, instrument, currency string, maxSettleDate *time.Time) (*model.Balance, error) {
	if maxSettleDate == nil {
		row := d.Conn.QueryRowContext(ctx, `
            SELECT `+positionSelectColumns+`
            FROM blnk.balances b
            WHERE b.ledger_id = $1 AND COALESCE(b.identity_id, '') = $2 AND b.account_ref = $3
              AND COALESCE(b.instrument, '') = $4 AND b.currency = $5 AND b.settle_date IS NULL
            LIMIT 1`,
			ledgerID, identityID, accountRef, instrument, currency)
		return scanActivePosition(row)
	}

	row := d.Conn.QueryRowContext(ctx, `
        SELECT `+positionSelectColumns+`
        FROM blnk.balances b
        WHERE b.ledger_id = $1 AND COALESCE(b.identity_id, '') = $2 AND b.account_ref = $3
          AND COALESCE(b.instrument, '') = $4 AND b.currency = $5
          AND (b.settle_date IS NULL OR b.settle_date <= $6)
        ORDER BY b.settle_date DESC NULLS LAST
        LIMIT 1`,
		ledgerID, identityID, accountRef, instrument, currency, *maxSettleDate)
	return scanActivePosition(row)
}

// scanActivePosition scans a cascade row, mapping no-rows to NotFound.
func scanActivePosition(row rowScanner) (*model.Balance, error) {
	balance, err := scanPositionRow(row)
	if err == sql.ErrNoRows {
		return nil, apierror.NewAPIError(apierror.ErrNotFound, "No active position found for the requested dimensions", err)
	}
	if err != nil {
		return nil, apierror.NewAPIError(apierror.ErrInternalServer, "Failed to scan active position", err)
	}
	return balance, nil
}

// GetMaturedPositions lists future (T+N) brokerage balances whose settlement
// date has been reached, candidates for the settlement roll.
func (d Datasource) GetMaturedPositions(ctx context.Context, asOf time.Time, limit int) ([]*model.Balance, error) {
	rows, err := d.Conn.QueryContext(ctx, `
        SELECT `+positionSelectColumns+`
        FROM blnk.balances b
        WHERE b.account_ref IS NOT NULL AND b.settle_date IS NOT NULL AND b.settle_date <= $1
          -- Skip already-settled (zeroed) buckets so re-running settlement is
          -- idempotent and closed buckets do not crowd the batch limit.
          AND (b.balance <> 0 OR b.inflight_credit_balance <> 0 OR b.inflight_debit_balance <> 0)
        ORDER BY b.settle_date ASC
        LIMIT $2`, asOf, limit)
	if err != nil {
		return nil, apierror.NewAPIError(apierror.ErrInternalServer, "Failed to query matured positions", err)
	}
	defer func() { _ = rows.Close() }()

	var balances []*model.Balance
	for rows.Next() {
		balance, err := scanPositionRow(rows)
		if err != nil {
			return nil, apierror.NewAPIError(apierror.ErrInternalServer, "Failed to scan matured position", err)
		}
		balances = append(balances, balance)
	}
	if err := rows.Err(); err != nil {
		return nil, apierror.NewAPIError(apierror.ErrInternalServer, "Failed to iterate matured positions", err)
	}
	return balances, nil
}

// ApplyBalanceDeltas applies a normalized mutation plan in one database
// transaction (TradeControl BalanceMutationServiceImpl.apply / DaoJdbc analog):
// rows are locked with SELECT ... FOR UPDATE in the deterministic plan order,
// missing positions are created, deltas preserve the blnk invariants
// (balance = credit - debit, inflight = inflight_credit - inflight_debit) and
// versions are bumped for optimistic readers. Distributed locking is the
// caller's responsibility (Blnk.ApplyMutationPlan).
func (d Datasource) ApplyBalanceDeltas(ctx context.Context, deltas []model.BalanceDelta) error {
	if len(deltas) == 0 {
		return nil
	}

	tx, err := d.Conn.BeginTx(ctx, nil)
	if err != nil {
		return apierror.NewAPIError(apierror.ErrInternalServer, "Failed to begin mutation transaction", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, delta := range deltas {
		if err := d.applyOneDelta(ctx, tx, delta); err != nil {
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		return apierror.NewAPIError(apierror.ErrInternalServer, "Failed to commit mutation transaction", err)
	}
	return nil
}

// lockPositionRow reads the position's mutable fields under FOR UPDATE.
func lockPositionRow(ctx context.Context, tx *sql.Tx, key model.PositionKey) (balanceID string, credit, debit, inflightCredit, inflightDebit *big.Int, err error) {
	row := tx.QueryRowContext(ctx, `
        SELECT b.balance_id, b.credit_balance::text, b.debit_balance::text,
               b.inflight_credit_balance::text, b.inflight_debit_balance::text
        FROM blnk.balances b
        WHERE `+positionKeyPredicate+`
        FOR UPDATE`, positionKeyArgs(key)...)

	var creditStr, debitStr, inflightCreditStr, inflightDebitStr string
	if err = row.Scan(&balanceID, &creditStr, &debitStr, &inflightCreditStr, &inflightDebitStr); err != nil {
		return "", nil, nil, nil, nil, err
	}
	parse := func(s string) (*big.Int, error) {
		v, ok := new(big.Int).SetString(s, 10)
		if !ok {
			return nil, fmt.Errorf("invalid balance amount %q", s)
		}
		return v, nil
	}
	if credit, err = parse(creditStr); err == nil {
		if debit, err = parse(debitStr); err == nil {
			if inflightCredit, err = parse(inflightCreditStr); err == nil {
				inflightDebit, err = parse(inflightDebitStr)
			}
		}
	}
	return balanceID, credit, debit, inflightCredit, inflightDebit, err
}

// applyOneDelta locks (creating when missing) one position row and applies a
// single delta to it.
func (d Datasource) applyOneDelta(ctx context.Context, tx *sql.Tx, delta model.BalanceDelta) error {
	balanceID, credit, debit, inflightCredit, inflightDebit, err := lockPositionRow(ctx, tx, delta.Key)
	if err == sql.ErrNoRows {
		// Create the missing position inside the same transaction, then lock it.
		var identityID interface{} = delta.Key.IdentityID
		if delta.Key.IdentityID == "" {
			identityID = nil
		}
		var instrument interface{} = delta.Key.Instrument
		if delta.Key.Instrument == "" {
			instrument = nil
		}
		var settleCode interface{}
		if delta.Key.SettleCode != nil {
			settleCode = *delta.Key.SettleCode
		}
		var settleDate interface{}
		if delta.Key.SettleDate != nil {
			settleDate = *delta.Key.SettleDate
		}
		_, err = tx.ExecContext(ctx, `
            INSERT INTO blnk.balances
                (balance_id, balance, credit_balance, debit_balance,
                 inflight_balance, inflight_credit_balance, inflight_debit_balance,
                 currency, ledger_id, identity_id, created_at, meta_data,
                 account_ref, instrument, settle_date, settle_code)
            VALUES ($1, 0, 0, 0, 0, 0, 0, $2, $3, $4, $5, '{}', $6, $7, $8, $9)
            ON CONFLICT DO NOTHING`,
			model.GenerateUUIDWithSuffix("bln"), delta.Key.Currency, delta.Key.LedgerID,
			identityID, time.Now(), delta.Key.AccountRef, instrument, settleDate, settleCode)
		if err != nil {
			return apierror.NewAPIError(apierror.ErrInternalServer, "Failed to create position for mutation", err)
		}
		balanceID, credit, debit, inflightCredit, inflightDebit, err = lockPositionRow(ctx, tx, delta.Key)
	}
	if err != nil {
		return apierror.NewAPIError(apierror.ErrInternalServer, fmt.Sprintf("Failed to lock position '%s'", delta.Key.LockKey()), err)
	}

	// Apply the amount delta through credit/debit so the double-entry
	// decomposition stays intact.
	if delta.AmountDelta != nil {
		switch delta.AmountDelta.Sign() {
		case 1:
			credit = new(big.Int).Add(credit, delta.AmountDelta)
		case -1:
			debit = new(big.Int).Sub(debit, delta.AmountDelta) // subtracting a negative adds
		}
	}
	if delta.BlockedDelta != nil {
		inflightDebit = new(big.Int).Add(inflightDebit, delta.BlockedDelta)
	}
	if delta.WaitingDelta != nil {
		inflightCredit = new(big.Int).Add(inflightCredit, delta.WaitingDelta)
	}
	if inflightDebit.Sign() < 0 || inflightCredit.Sign() < 0 {
		return apierror.NewAPIError(apierror.ErrBadRequest,
			fmt.Sprintf("Mutation would drive holds negative for position '%s'", delta.Key.LockKey()), nil)
	}

	balance := new(big.Int).Sub(credit, debit)
	inflight := new(big.Int).Sub(inflightCredit, inflightDebit)

	_, err = tx.ExecContext(ctx, `
        UPDATE blnk.balances
        SET balance = $2, credit_balance = $3, debit_balance = $4,
            inflight_balance = $5, inflight_credit_balance = $6, inflight_debit_balance = $7,
            version = version + 1
        WHERE balance_id = $1`,
		balanceID, balance.String(), credit.String(), debit.String(),
		inflight.String(), inflightCredit.String(), inflightDebit.String())
	if err != nil {
		return apierror.NewAPIError(apierror.ErrInternalServer, fmt.Sprintf("Failed to update position '%s'", delta.Key.LockKey()), err)
	}
	return nil
}

// RecomputeHolds rebuilds the inflight (blocked/waiting) fields of one balance
// from its live INFLIGHT transactions, accounting for partial commits and
// voids (TradeControl recalculateBlockedWaitingAmount* analog). Returns the
// recomputed (debit, credit) holds.
func (d Datasource) RecomputeHolds(ctx context.Context, balanceID string) (*big.Int, *big.Int, error) {
	tx, err := d.Conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, apierror.NewAPIError(apierror.ErrInternalServer, "Failed to begin holds transaction", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Lock the balance row so concurrent transaction processing cannot
	// interleave between the recompute and the write-back.
	var current string
	if err := tx.QueryRowContext(ctx,
		`SELECT balance_id FROM blnk.balances WHERE balance_id = $1 FOR UPDATE`, balanceID,
	).Scan(&current); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil, apierror.NewAPIError(apierror.ErrNotFound, fmt.Sprintf("Balance with ID '%s' not found", balanceID), err)
		}
		return nil, nil, apierror.NewAPIError(apierror.ErrInternalServer, "Failed to lock balance", err)
	}

	// Remaining hold per INFLIGHT parent = precise_amount minus committed
	// children; voided parents hold nothing.
	row := tx.QueryRowContext(ctx, `
        WITH live AS (
            SELECT t.source, t.destination,
                   GREATEST(t.precise_amount - COALESCE((
                       SELECT SUM(c.precise_amount)
                       FROM blnk.transactions c
                       WHERE c.parent_transaction = t.transaction_id
                         AND c.status = 'APPLIED'), 0), 0) AS remaining
            FROM blnk.transactions t
            WHERE t.status = 'INFLIGHT'
              AND (t.source = $1 OR t.destination = $1)
              AND NOT EXISTS (
                  SELECT 1 FROM blnk.transactions v
                  WHERE v.parent_transaction = t.transaction_id AND v.status = 'VOID')
        )
        SELECT COALESCE(SUM(CASE WHEN source = $1 THEN remaining ELSE 0 END), 0)::text,
               COALESCE(SUM(CASE WHEN destination = $1 THEN remaining ELSE 0 END), 0)::text
        FROM live`, balanceID)

	var debitStr, creditStr string
	if err := row.Scan(&debitStr, &creditStr); err != nil {
		return nil, nil, apierror.NewAPIError(apierror.ErrInternalServer, "Failed to recompute holds", err)
	}
	inflightDebit, ok := new(big.Int).SetString(debitStr, 10)
	if !ok {
		return nil, nil, apierror.NewAPIError(apierror.ErrInternalServer, fmt.Sprintf("Invalid recomputed debit hold %q", debitStr), nil)
	}
	inflightCredit, ok := new(big.Int).SetString(creditStr, 10)
	if !ok {
		return nil, nil, apierror.NewAPIError(apierror.ErrInternalServer, fmt.Sprintf("Invalid recomputed credit hold %q", creditStr), nil)
	}

	inflight := new(big.Int).Sub(inflightCredit, inflightDebit)
	if _, err := tx.ExecContext(ctx, `
        UPDATE blnk.balances
        SET inflight_balance = $2, inflight_credit_balance = $3, inflight_debit_balance = $4,
            version = version + 1
        WHERE balance_id = $1`,
		balanceID, inflight.String(), inflightCredit.String(), inflightDebit.String()); err != nil {
		return nil, nil, apierror.NewAPIError(apierror.ErrInternalServer, "Failed to write recomputed holds", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, nil, apierror.NewAPIError(apierror.ErrInternalServer, "Failed to commit recomputed holds", err)
	}
	return inflightDebit, inflightCredit, nil
}

// GetPendingInflightByDestination lists INFLIGHT transactions destined to the
// balance that still hold a remaining amount (no void, not fully committed).
// Used by the settlement runner to find unsettled trade legs.
func (d Datasource) GetPendingInflightByDestination(ctx context.Context, balanceID string) ([]*model.Transaction, error) {
	rows, err := d.Conn.QueryContext(ctx, `
        SELECT t.transaction_id, t.parent_transaction, t.source, t.reference, t.amount,
               t.precise_amount::text, t.precision, t.currency, t.destination, t.description,
               t.status, t.created_at, t.meta_data
        FROM blnk.transactions t
        WHERE t.destination = $1
          AND t.status = 'INFLIGHT'
          AND NOT EXISTS (
              SELECT 1 FROM blnk.transactions v
              WHERE v.parent_transaction = t.transaction_id AND v.status = 'VOID')
          AND t.precise_amount > COALESCE((
              SELECT SUM(c.precise_amount)
              FROM blnk.transactions c
              WHERE c.parent_transaction = t.transaction_id AND c.status = 'APPLIED'), 0)
        ORDER BY t.created_at ASC`, balanceID)
	if err != nil {
		return nil, apierror.NewAPIError(apierror.ErrInternalServer, "Failed to query pending inflight transactions", err)
	}
	defer func() { _ = rows.Close() }()

	var transactions []*model.Transaction
	for rows.Next() {
		transaction := model.Transaction{}
		var metaDataJSON []byte
		var preciseAmount string
		err := rows.Scan(
			&transaction.TransactionID, &transaction.ParentTransaction, &transaction.Source,
			&transaction.Reference, &transaction.Amount, &preciseAmount, &transaction.Precision,
			&transaction.Currency, &transaction.Destination, &transaction.Description,
			&transaction.Status, &transaction.CreatedAt, &metaDataJSON,
		)
		if err != nil {
			return nil, apierror.NewAPIError(apierror.ErrInternalServer, "Failed to scan pending inflight transaction", err)
		}
		precise, ok := new(big.Int).SetString(preciseAmount, 10)
		if !ok {
			return nil, apierror.NewAPIError(apierror.ErrInternalServer, fmt.Sprintf("Invalid precise_amount %q", preciseAmount), nil)
		}
		transaction.PreciseAmount = precise
		if len(metaDataJSON) > 0 {
			if err := json.Unmarshal(metaDataJSON, &transaction.MetaData); err != nil {
				return nil, apierror.NewAPIError(apierror.ErrInternalServer, "Failed to unmarshal transaction metadata", err)
			}
		}
		transactions = append(transactions, &transaction)
	}
	if err := rows.Err(); err != nil {
		return nil, apierror.NewAPIError(apierror.ErrInternalServer, "Failed to iterate pending inflight transactions", err)
	}
	return transactions, nil
}

// UpdateWAPrice stores the recomputed weighted-average price on the balance.
func (d Datasource) UpdateWAPrice(ctx context.Context, balanceID, waPrice string) error {
	result, err := d.Conn.ExecContext(ctx, `
        UPDATE blnk.balances SET wa_price = $2::numeric, version = version + 1
        WHERE balance_id = $1`, balanceID, waPrice)
	if err != nil {
		return apierror.NewAPIError(apierror.ErrInternalServer, "Failed to update wa price", err)
	}
	affected, err := result.RowsAffected()
	if err == nil && affected == 0 {
		return apierror.NewAPIError(apierror.ErrNotFound, fmt.Sprintf("Balance with ID '%s' not found", balanceID), nil)
	}
	return nil
}

// CreateLot records a purchase lot (TradeControl BalanceDetail analog).
func (d Datasource) CreateLot(ctx context.Context, lot model.BalanceLot) (model.BalanceLot, error) {
	if lot.Quantity == nil || lot.Quantity.Sign() <= 0 {
		return model.BalanceLot{}, apierror.NewAPIError(apierror.ErrBadRequest, "Lot quantity must be positive", nil)
	}
	if lot.Precision < 1 {
		lot.Precision = 1
	}
	lot.LotID = model.GenerateUUIDWithSuffix("lot")
	lot.CreatedAt = time.Now()
	if lot.PurchasedAt.IsZero() {
		lot.PurchasedAt = lot.CreatedAt
	}

	err := d.Conn.QueryRowContext(ctx, `
        INSERT INTO blnk.balance_lots
            (lot_id, balance_id, instrument, quantity, precision, price, currency, reference, purchased_at, created_at)
        VALUES ($1, $2, $3, $4, $5, $6::numeric, $7, $8, $9, $10)
        RETURNING id`,
		lot.LotID, lot.BalanceID, lot.Instrument, lot.Quantity.String(), lot.Precision,
		lot.Price, lot.Currency, lot.Reference, lot.PurchasedAt, lot.CreatedAt,
	).Scan(&lot.ID)
	if err != nil {
		if pqErr, ok := err.(*pq.Error); ok && pqErr.Code.Name() == "foreign_key_violation" {
			return model.BalanceLot{}, apierror.NewAPIError(apierror.ErrBadRequest, "Invalid balance ID", err)
		}
		return model.BalanceLot{}, apierror.NewAPIError(apierror.ErrInternalServer, "Failed to create lot", err)
	}
	return lot, nil
}

// GetLots lists the purchase lots of a balance, oldest first.
func (d Datasource) GetLots(ctx context.Context, balanceID string) ([]model.BalanceLot, error) {
	rows, err := d.Conn.QueryContext(ctx, `
        SELECT id, lot_id, balance_id, instrument, quantity::text, precision, price::text,
               currency, COALESCE(reference, ''), purchased_at, created_at
        FROM blnk.balance_lots
        WHERE balance_id = $1
        ORDER BY purchased_at ASC, id ASC`, balanceID)
	if err != nil {
		return nil, apierror.NewAPIError(apierror.ErrInternalServer, "Failed to query lots", err)
	}
	defer func() { _ = rows.Close() }()

	var lots []model.BalanceLot
	for rows.Next() {
		var lot model.BalanceLot
		var quantityStr string
		if err := rows.Scan(&lot.ID, &lot.LotID, &lot.BalanceID, &lot.Instrument, &quantityStr,
			&lot.Precision, &lot.Price, &lot.Currency, &lot.Reference, &lot.PurchasedAt, &lot.CreatedAt); err != nil {
			return nil, apierror.NewAPIError(apierror.ErrInternalServer, "Failed to scan lot", err)
		}
		quantity, ok := new(big.Int).SetString(quantityStr, 10)
		if !ok {
			return nil, apierror.NewAPIError(apierror.ErrInternalServer, fmt.Sprintf("Invalid lot quantity %q", quantityStr), nil)
		}
		lot.Quantity = quantity
		lots = append(lots, lot)
	}
	if err := rows.Err(); err != nil {
		return nil, apierror.NewAPIError(apierror.ErrInternalServer, "Failed to iterate lots", err)
	}
	return lots, nil
}

// UpsertInstrumentSettings stores (or updates) the trading mode of an
// instrument.
func (d Datasource) UpsertInstrumentSettings(ctx context.Context, settings model.InstrumentSettings) (model.InstrumentSettings, error) {
	if settings.Instrument == "" {
		return model.InstrumentSettings{}, apierror.NewAPIError(apierror.ErrBadRequest, "Instrument is required", nil)
	}
	if settings.SettleOffset < 0 {
		return model.InstrumentSettings{}, apierror.NewAPIError(apierror.ErrBadRequest, "settle_offset must be >= 0", nil)
	}
	now := time.Now()
	var venue interface{} = settings.Venue
	if settings.Venue == "" {
		venue = nil
	}
	err := d.Conn.QueryRowContext(ctx, `
        INSERT INTO blnk.instrument_settings (instrument, venue, trades_on_the_way, settle_offset, created_at, updated_at)
        VALUES ($1, $2, $3, $4, $5, $5)
        ON CONFLICT (instrument) DO UPDATE
            SET venue = EXCLUDED.venue, trades_on_the_way = EXCLUDED.trades_on_the_way,
                settle_offset = EXCLUDED.settle_offset, updated_at = EXCLUDED.updated_at
        RETURNING id, created_at, updated_at`,
		settings.Instrument, venue, settings.TradesOnTheWay, settings.SettleOffset, now,
	).Scan(&settings.ID, &settings.CreatedAt, &settings.UpdatedAt)
	if err != nil {
		return model.InstrumentSettings{}, apierror.NewAPIError(apierror.ErrInternalServer, "Failed to upsert instrument settings", err)
	}
	return settings, nil
}

// GetInstrumentSettings returns the trading mode of an instrument, or a
// NotFound error when none is configured.
func (d Datasource) GetInstrumentSettings(ctx context.Context, instrument string) (*model.InstrumentSettings, error) {
	settings := &model.InstrumentSettings{}
	var venue sql.NullString
	err := d.Conn.QueryRowContext(ctx, `
        SELECT id, instrument, COALESCE(venue, ''), trades_on_the_way, settle_offset, created_at, updated_at
        FROM blnk.instrument_settings WHERE instrument = $1`, instrument,
	).Scan(&settings.ID, &settings.Instrument, &venue, &settings.TradesOnTheWay,
		&settings.SettleOffset, &settings.CreatedAt, &settings.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, apierror.NewAPIError(apierror.ErrNotFound, fmt.Sprintf("No settings configured for instrument '%s'", instrument), err)
	}
	if err != nil {
		return nil, apierror.NewAPIError(apierror.ErrInternalServer, "Failed to scan instrument settings", err)
	}
	settings.Venue = venue.String
	return settings, nil
}

// SumFutureHolds aggregates the in-transit incoming (inflight credit) and
// outgoing (inflight debit) quantity across the future balances of a position
// that settle on or before asOfSettleDate. It is the in-transit term of the
// tradable arithmetic for on-the-way instruments.
func (d Datasource) SumFutureHolds(ctx context.Context, ledgerID, identityID, accountRef, instrument, currency string, asOfSettleDate time.Time) (*big.Int, *big.Int, error) {
	row := d.Conn.QueryRowContext(ctx, `
        SELECT COALESCE(SUM(b.inflight_credit_balance), 0)::text,
               COALESCE(SUM(b.inflight_debit_balance), 0)::text
        FROM blnk.balances b
        WHERE b.ledger_id = $1 AND COALESCE(b.identity_id, '') = $2 AND b.account_ref = $3
          AND COALESCE(b.instrument, '') = $4 AND b.currency = $5
          AND b.settle_date IS NOT NULL AND b.settle_date <= $6`,
		ledgerID, identityID, accountRef, instrument, currency, asOfSettleDate)

	var incomingStr, outgoingStr string
	if err := row.Scan(&incomingStr, &outgoingStr); err != nil {
		return nil, nil, apierror.NewAPIError(apierror.ErrInternalServer, "Failed to sum future holds", err)
	}
	incoming, ok := new(big.Int).SetString(incomingStr, 10)
	if !ok {
		return nil, nil, apierror.NewAPIError(apierror.ErrInternalServer, fmt.Sprintf("Invalid incoming hold sum %q", incomingStr), nil)
	}
	outgoing, ok := new(big.Int).SetString(outgoingStr, 10)
	if !ok {
		return nil, nil, apierror.NewAPIError(apierror.ErrInternalServer, fmt.Sprintf("Invalid outgoing hold sum %q", outgoingStr), nil)
	}
	return incoming, outgoing, nil
}

// GetPendingInflightByBalance lists INFLIGHT transactions touching the balance
// on either side (source or destination) that still hold a remaining amount.
// Used by the settlement roll to find both incoming and outgoing trade legs of
// a matured future balance.
func (d Datasource) GetPendingInflightByBalance(ctx context.Context, balanceID string) ([]*model.Transaction, error) {
	rows, err := d.Conn.QueryContext(ctx, `
        SELECT t.transaction_id, t.parent_transaction, t.source, t.reference, t.amount,
               t.precise_amount::text, t.precision, t.currency, t.destination, t.description,
               t.status, t.created_at, t.meta_data
        FROM blnk.transactions t
        WHERE (t.source = $1 OR t.destination = $1)
          AND t.status = 'INFLIGHT'
          AND NOT EXISTS (
              SELECT 1 FROM blnk.transactions v
              WHERE v.parent_transaction = t.transaction_id AND v.status = 'VOID')
          AND t.precise_amount > COALESCE((
              SELECT SUM(c.precise_amount)
              FROM blnk.transactions c
              WHERE c.parent_transaction = t.transaction_id AND c.status = 'APPLIED'), 0)
        ORDER BY t.created_at ASC`, balanceID)
	if err != nil {
		return nil, apierror.NewAPIError(apierror.ErrInternalServer, "Failed to query pending inflight transactions", err)
	}
	defer func() { _ = rows.Close() }()

	var transactions []*model.Transaction
	for rows.Next() {
		transaction := model.Transaction{}
		var metaDataJSON []byte
		var preciseAmount string
		err := rows.Scan(
			&transaction.TransactionID, &transaction.ParentTransaction, &transaction.Source,
			&transaction.Reference, &transaction.Amount, &preciseAmount, &transaction.Precision,
			&transaction.Currency, &transaction.Destination, &transaction.Description,
			&transaction.Status, &transaction.CreatedAt, &metaDataJSON,
		)
		if err != nil {
			return nil, apierror.NewAPIError(apierror.ErrInternalServer, "Failed to scan pending inflight transaction", err)
		}
		precise, ok := new(big.Int).SetString(preciseAmount, 10)
		if !ok {
			return nil, apierror.NewAPIError(apierror.ErrInternalServer, fmt.Sprintf("Invalid precise_amount %q", preciseAmount), nil)
		}
		transaction.PreciseAmount = precise
		if len(metaDataJSON) > 0 {
			if err := json.Unmarshal(metaDataJSON, &transaction.MetaData); err != nil {
				return nil, apierror.NewAPIError(apierror.ErrInternalServer, "Failed to unmarshal transaction metadata", err)
			}
		}
		transactions = append(transactions, &transaction)
	}
	if err := rows.Err(); err != nil {
		return nil, apierror.NewAPIError(apierror.ErrInternalServer, "Failed to iterate pending inflight transactions", err)
	}
	return transactions, nil
}

// CreateHoliday registers a non-settlement day for a venue.
func (d Datasource) CreateHoliday(ctx context.Context, holiday model.MarketHoliday) (model.MarketHoliday, error) {
	if holiday.Venue == "" {
		return model.MarketHoliday{}, apierror.NewAPIError(apierror.ErrBadRequest, "Holiday venue is required", nil)
	}
	holiday.CreatedAt = time.Now()
	err := d.Conn.QueryRowContext(ctx, `
        INSERT INTO blnk.market_holidays (venue, holiday_date, description, created_at)
        VALUES ($1, $2, $3, $4)
        RETURNING id`,
		holiday.Venue, holiday.HolidayDate, holiday.Description, holiday.CreatedAt,
	).Scan(&holiday.ID)
	if err != nil {
		if pqErr, ok := err.(*pq.Error); ok && pqErr.Code.Name() == "unique_violation" {
			return model.MarketHoliday{}, apierror.NewAPIError(apierror.ErrConflict,
				fmt.Sprintf("Holiday already exists for venue '%s' on %s", holiday.Venue, holiday.HolidayDate.Format(model.HolidayKeyFormat)), err)
		}
		return model.MarketHoliday{}, apierror.NewAPIError(apierror.ErrInternalServer, "Failed to create holiday", err)
	}
	return holiday, nil
}

// GetHolidays lists the holidays of a venue inside [from, to].
func (d Datasource) GetHolidays(ctx context.Context, venue string, from, to time.Time) ([]model.MarketHoliday, error) {
	rows, err := d.Conn.QueryContext(ctx, `
        SELECT id, venue, holiday_date, COALESCE(description, ''), created_at
        FROM blnk.market_holidays
        WHERE venue = $1 AND holiday_date >= $2 AND holiday_date <= $3
        ORDER BY holiday_date ASC`, venue, from, to)
	if err != nil {
		return nil, apierror.NewAPIError(apierror.ErrInternalServer, "Failed to query holidays", err)
	}
	defer func() { _ = rows.Close() }()

	var holidays []model.MarketHoliday
	for rows.Next() {
		var holiday model.MarketHoliday
		if err := rows.Scan(&holiday.ID, &holiday.Venue, &holiday.HolidayDate, &holiday.Description, &holiday.CreatedAt); err != nil {
			return nil, apierror.NewAPIError(apierror.ErrInternalServer, "Failed to scan holiday", err)
		}
		holidays = append(holidays, holiday)
	}
	if err := rows.Err(); err != nil {
		return nil, apierror.NewAPIError(apierror.ErrInternalServer, "Failed to iterate holidays", err)
	}
	return holidays, nil
}

// --- Settlement journal (two-phase, idempotent side effects) ---

// journalColumns is the shared SELECT list for settlement journal reads.
const journalColumns = `
    security_txn_id, COALESCE(trade_ref,''), spot_balance_id, COALESCE(instrument,''),
    COALESCE(side,''), COALESCE(wa_before::text,''), COALESCE(qty_before::text,''),
    COALESCE(price::text,''), COALESCE(quantity::text,''), COALESCE(precision,0),
    COALESCE(currency,''), COALESCE(wa_after::text,''), COALESCE(lot_id,''),
    status, created_at, updated_at`

func scanJournal(row rowScanner) (*model.SettlementJournalEntry, error) {
	e := &model.SettlementJournalEntry{}
	err := row.Scan(
		&e.SecurityTxnID, &e.TradeRef, &e.SpotBalanceID, &e.Instrument, &e.Side,
		&e.WABefore, &e.QtyBefore, &e.Price, &e.Quantity, &e.Precision,
		&e.Currency, &e.WAAfter, &e.LotID, &e.Status, &e.CreatedAt, &e.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return e, nil
}

// BeginSettlementJournal inserts a 'pending' journal row for a security leg, or
// returns the existing row if one is already present (idempotency). The bool is
// true when a new row was created.
func (d Datasource) BeginSettlementJournal(ctx context.Context, e model.SettlementJournalEntry) (*model.SettlementJournalEntry, bool, error) {
	now := time.Now()
	res, err := d.Conn.ExecContext(ctx, `
        INSERT INTO blnk.brokerage_settlement_journal
            (security_txn_id, trade_ref, spot_balance_id, instrument, side,
             wa_before, qty_before, price, quantity, precision, currency, status, created_at, updated_at)
        VALUES ($1,$2,$3,$4,$5, NULLIF($6,'')::numeric, NULLIF($7,'')::numeric,
                NULLIF($8,'')::numeric, NULLIF($9,'')::numeric, $10, $11, 'pending', $12, $12)
        ON CONFLICT (security_txn_id) DO NOTHING`,
		e.SecurityTxnID, e.TradeRef, e.SpotBalanceID, e.Instrument, e.Side,
		e.WABefore, e.QtyBefore, e.Price, e.Quantity, e.Precision, e.Currency, now)
	if err != nil {
		return nil, false, apierror.NewAPIError(apierror.ErrInternalServer, "Failed to begin settlement journal", err)
	}
	existing, err := d.GetSettlementJournal(ctx, e.SecurityTxnID)
	if err != nil {
		return nil, false, err
	}
	created := false
	if n, e2 := res.RowsAffected(); e2 == nil && n == 1 {
		created = true
	}
	return existing, created, nil
}

// GetSettlementJournal fetches a journal row, or NotFound.
func (d Datasource) GetSettlementJournal(ctx context.Context, securityTxnID string) (*model.SettlementJournalEntry, error) {
	row := d.Conn.QueryRowContext(ctx, `SELECT `+journalColumns+`
        FROM blnk.brokerage_settlement_journal WHERE security_txn_id = $1`, securityTxnID)
	e, err := scanJournal(row)
	if err == sql.ErrNoRows {
		return nil, apierror.NewAPIError(apierror.ErrNotFound, fmt.Sprintf("No settlement journal for '%s'", securityTxnID), err)
	}
	if err != nil {
		return nil, apierror.NewAPIError(apierror.ErrInternalServer, "Failed to scan settlement journal", err)
	}
	return e, nil
}

// CompleteSettlementJournal applies the side effects of a settled buy leg in a
// single DB transaction: an idempotent purchase lot, the new wa_price on the
// spot balance, and the journal flipped to 'applied'. Safe to call repeatedly.
func (d Datasource) CompleteSettlementJournal(ctx context.Context, securityTxnID, waAfter string, lot model.BalanceLot) (string, error) {
	tx, err := d.Conn.BeginTx(ctx, nil)
	if err != nil {
		return "", apierror.NewAPIError(apierror.ErrInternalServer, "Failed to begin journal completion", err)
	}
	defer func() { _ = tx.Rollback() }()

	lotID := ""
	if lot.Quantity != nil && lot.Quantity.Sign() > 0 {
		if lot.Precision < 1 {
			lot.Precision = 1
		}
		newLotID := model.GenerateUUIDWithSuffix("lot")
		if lot.PurchasedAt.IsZero() {
			lot.PurchasedAt = time.Now()
		}
		// Idempotent on (balance_id, reference): a retry returns the existing lot id.
		err = tx.QueryRowContext(ctx, `
            INSERT INTO blnk.balance_lots
                (lot_id, balance_id, instrument, quantity, precision, price, currency, reference, purchased_at, created_at)
            VALUES ($1,$2,$3,$4,$5,$6::numeric,$7,$8,$9,$10)
            ON CONFLICT (balance_id, reference) WHERE reference IS NOT NULL AND reference <> ''
            DO UPDATE SET lot_id = blnk.balance_lots.lot_id
            RETURNING lot_id`,
			newLotID, lot.BalanceID, lot.Instrument, lot.Quantity.String(), lot.Precision,
			lot.Price, lot.Currency, lot.Reference, lot.PurchasedAt, time.Now()).Scan(&lotID)
		if err != nil {
			return "", apierror.NewAPIError(apierror.ErrInternalServer, "Failed to upsert lot", err)
		}
	}

	if waAfter != "" {
		if _, err = tx.ExecContext(ctx, `
            UPDATE blnk.balances SET wa_price = $2::numeric, version = version + 1
            WHERE balance_id = $1`, lot.BalanceID, waAfter); err != nil {
			return "", apierror.NewAPIError(apierror.ErrInternalServer, "Failed to update wa price", err)
		}
	}

	if _, err = tx.ExecContext(ctx, `
        UPDATE blnk.brokerage_settlement_journal
        SET status = 'applied', wa_after = NULLIF($2,'')::numeric, lot_id = NULLIF($3,''), updated_at = NOW()
        WHERE security_txn_id = $1`, securityTxnID, waAfter, lotID); err != nil {
		return "", apierror.NewAPIError(apierror.ErrInternalServer, "Failed to mark journal applied", err)
	}

	if err = tx.Commit(); err != nil {
		return "", apierror.NewAPIError(apierror.ErrInternalServer, "Failed to commit journal completion", err)
	}
	return lotID, nil
}

// ListPendingSettlementJournals returns journal rows still awaiting their side
// effects (recovery candidates).
func (d Datasource) ListPendingSettlementJournals(ctx context.Context, limit int) ([]model.SettlementJournalEntry, error) {
	rows, err := d.Conn.QueryContext(ctx, `SELECT `+journalColumns+`
        FROM blnk.brokerage_settlement_journal WHERE status = 'pending'
        ORDER BY created_at ASC LIMIT $1`, limit)
	if err != nil {
		return nil, apierror.NewAPIError(apierror.ErrInternalServer, "Failed to query pending journals", err)
	}
	defer func() { _ = rows.Close() }()
	var out []model.SettlementJournalEntry
	for rows.Next() {
		e, err := scanJournal(rows)
		if err != nil {
			return nil, apierror.NewAPIError(apierror.ErrInternalServer, "Failed to scan pending journal", err)
		}
		out = append(out, *e)
	}
	if err := rows.Err(); err != nil {
		return nil, apierror.NewAPIError(apierror.ErrInternalServer, "Failed to iterate pending journals", err)
	}
	return out, nil
}
