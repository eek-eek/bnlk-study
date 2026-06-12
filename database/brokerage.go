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
// the same COALESCE normalization as the idx_balances_position_key index.
const positionKeyPredicate = `
    b.ledger_id = $1 AND COALESCE(b.identity_id, '') = $2 AND b.account_ref = $3
    AND COALESCE(b.instrument, '') = $4 AND b.currency = $5 AND COALESCE(b.settle_code, -1) = $6`

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
// positionKeyPredicate.
func positionKeyArgs(key model.PositionKey) []interface{} {
	settleCode := model.SpotSettleCode
	if key.SettleCode != nil {
		settleCode = *key.SettleCode
	}
	return []interface{}{key.LedgerID, key.IdentityID, key.AccountRef, key.Instrument, key.Currency, settleCode}
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
// findOrCreateAndLock analog). settleDate must be provided when the key has a
// settle code, so future balances always carry their settlement date.
func (d Datasource) FindOrCreatePosition(ctx context.Context, key model.PositionKey, settleDate *time.Time) (*model.Balance, error) {
	if err := key.Validate(); err != nil {
		return nil, apierror.NewAPIError(apierror.ErrBadRequest, err.Error(), err)
	}
	if key.SettleCode != nil && settleDate == nil {
		return nil, apierror.NewAPIError(apierror.ErrBadRequest, "settle_date is required for a future (T+N) position", nil)
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
	var settleDateArg interface{}
	if key.SettleCode != nil {
		settleCode = *key.SettleCode
		settleDateArg = *settleDate
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
// TradeControl settle cascade: the highest settle code <= maxSettleCode wins,
// falling back through T+1, T+0 down to the spot balance (settle_code IS NULL).
// maxSettleCode == nil restricts the lookup to the spot balance.
func (d Datasource) GetActivePosition(ctx context.Context, ledgerID, identityID, accountRef, instrument, currency string, maxSettleCode *int) (*model.Balance, error) {
	maxCode := model.SpotSettleCode
	if maxSettleCode != nil {
		maxCode = *maxSettleCode
	}

	row := d.Conn.QueryRowContext(ctx, `
        SELECT `+positionSelectColumns+`
        FROM blnk.balances b
        WHERE b.ledger_id = $1 AND COALESCE(b.identity_id, '') = $2 AND b.account_ref = $3
          AND COALESCE(b.instrument, '') = $4 AND b.currency = $5
          AND COALESCE(b.settle_code, -1) <= $6
        ORDER BY COALESCE(b.settle_code, -1) DESC
        LIMIT 1`,
		ledgerID, identityID, accountRef, instrument, currency, maxCode)

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
        WHERE b.account_ref IS NOT NULL AND b.settle_code IS NOT NULL AND b.settle_date <= $1
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
		_, err = tx.ExecContext(ctx, `
            INSERT INTO blnk.balances
                (balance_id, balance, credit_balance, debit_balance,
                 inflight_balance, inflight_credit_balance, inflight_debit_balance,
                 currency, ledger_id, identity_id, created_at, meta_data,
                 account_ref, instrument, settle_code)
            VALUES ($1, 0, 0, 0, 0, 0, 0, $2, $3, $4, $5, '{}', $6, $7, $8)
            ON CONFLICT DO NOTHING`,
			model.GenerateUUIDWithSuffix("bln"), delta.Key.Currency, delta.Key.LedgerID,
			identityID, time.Now(), delta.Key.AccountRef, instrument, settleCode)
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
               t.precise_amount, t.precision, t.currency, t.destination, t.description,
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
		var preciseAmount int64
		err := rows.Scan(
			&transaction.TransactionID, &transaction.ParentTransaction, &transaction.Source,
			&transaction.Reference, &transaction.Amount, &preciseAmount, &transaction.Precision,
			&transaction.Currency, &transaction.Destination, &transaction.Description,
			&transaction.Status, &transaction.CreatedAt, &metaDataJSON,
		)
		if err != nil {
			return nil, apierror.NewAPIError(apierror.ErrInternalServer, "Failed to scan pending inflight transaction", err)
		}
		transaction.PreciseAmount = big.NewInt(preciseAmount)
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
