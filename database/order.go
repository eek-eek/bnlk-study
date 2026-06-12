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

// Order data access: orders, the configurable status/reference dictionary,
// stop-list and venue trading windows.
package database

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/blnkfinance/blnk/internal/apierror"
	"github.com/blnkfinance/blnk/model"
	"github.com/lib/pq"
)

const orderColumns = `
    order_id, reference, ledger_id, COALESCE(identity_id,''), account_ref, instrument,
    COALESCE(venue,''), currency, side, quantity::text, quantity_precision, price::text,
    money_precision, settle_offset, settle_date, status, COALESCE(status_message,''),
    COALESCE(reject_reason,''), COALESCE(aml_status,''), COALESCE(settlement_balance_id,''),
    COALESCE(market_balance_id,''), COALESCE(trade_ref,''), COALESCE(security_txn_id,''),
    COALESCE(money_txn_id,''), created_at, updated_at`

func scanOrder(row rowScanner) (*model.Order, error) {
	o := &model.Order{}
	var settleDate sql.NullTime
	err := row.Scan(
		&o.OrderID, &o.Reference, &o.LedgerID, &o.IdentityID, &o.AccountRef, &o.Instrument,
		&o.Venue, &o.Currency, &o.Side, &o.Quantity, &o.QuantityPrecision, &o.Price,
		&o.MoneyPrecision, &o.SettleOffset, &settleDate, &o.Status, &o.StatusMessage,
		&o.RejectReason, &o.AMLStatus, &o.SettlementBalanceID, &o.MarketBalanceID,
		&o.TradeRef, &o.SecurityTxnID, &o.MoneyTxnID, &o.CreatedAt, &o.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	if settleDate.Valid {
		d := settleDate.Time
		o.SettleDate = &d
	}
	return o, nil
}

// CreateOrder inserts a new order.
func (d Datasource) CreateOrder(ctx context.Context, o model.Order) (*model.Order, error) {
	o.OrderID = model.GenerateUUIDWithSuffix("ord")
	now := time.Now()
	o.CreatedAt, o.UpdatedAt = now, now
	if o.Status == "" {
		o.Status = model.OrderStatusDraft
	}
	var identity, venue, settleDate interface{}
	if o.IdentityID != "" {
		identity = o.IdentityID
	}
	if o.Venue != "" {
		venue = o.Venue
	}
	if o.SettleDate != nil {
		settleDate = *o.SettleDate
	}
	_, err := d.Conn.ExecContext(ctx, `
        INSERT INTO blnk.orders
            (order_id, reference, ledger_id, identity_id, account_ref, instrument, venue, currency,
             side, quantity, quantity_precision, price, money_precision, settle_offset, settle_date,
             status, aml_status, settlement_balance_id, market_balance_id, created_at, updated_at)
        VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::numeric,$11,$12::numeric,$13,$14,$15,$16,$17,$18,$19,$20,$20)`,
		o.OrderID, o.Reference, o.LedgerID, identity, o.AccountRef, o.Instrument, venue, o.Currency,
		o.Side, o.Quantity, o.QuantityPrecision, o.Price, o.MoneyPrecision, o.SettleOffset, settleDate,
		o.Status, o.AMLStatus, o.SettlementBalanceID, o.MarketBalanceID, now)
	if err != nil {
		if pqErr, ok := err.(*pq.Error); ok && pqErr.Code.Name() == "unique_violation" {
			return nil, apierror.NewAPIError(apierror.ErrConflict, fmt.Sprintf("Order with reference '%s' already exists", o.Reference), err)
		}
		return nil, apierror.NewAPIError(apierror.ErrInternalServer, "Failed to create order", err)
	}
	return &o, nil
}

// GetOrder retrieves an order by ID.
func (d Datasource) GetOrder(ctx context.Context, orderID string) (*model.Order, error) {
	row := d.Conn.QueryRowContext(ctx, `SELECT `+orderColumns+` FROM blnk.orders WHERE order_id = $1`, orderID)
	o, err := scanOrder(row)
	if err == sql.ErrNoRows {
		return nil, apierror.NewAPIError(apierror.ErrNotFound, fmt.Sprintf("Order '%s' not found", orderID), err)
	}
	if err != nil {
		return nil, apierror.NewAPIError(apierror.ErrInternalServer, "Failed to scan order", err)
	}
	return o, nil
}

// UpdateOrder persists the mutable fields of an order (status and its outcome).
func (d Datasource) UpdateOrder(ctx context.Context, o *model.Order) error {
	res, err := d.Conn.ExecContext(ctx, `
        UPDATE blnk.orders
        SET status = $2, status_message = NULLIF($3,''), reject_reason = NULLIF($4,''),
            aml_status = NULLIF($5,''), trade_ref = NULLIF($6,''), security_txn_id = NULLIF($7,''),
            money_txn_id = NULLIF($8,''), updated_at = NOW()
        WHERE order_id = $1`,
		o.OrderID, o.Status, o.StatusMessage, o.RejectReason, o.AMLStatus,
		o.TradeRef, o.SecurityTxnID, o.MoneyTxnID)
	if err != nil {
		return apierror.NewAPIError(apierror.ErrInternalServer, "Failed to update order", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return apierror.NewAPIError(apierror.ErrNotFound, fmt.Sprintf("Order '%s' not found", o.OrderID), nil)
	}
	return nil
}

// ListOrdersByStatus lists orders in a given status (for batch processing/UI).
func (d Datasource) ListOrdersByStatus(ctx context.Context, status string, limit, offset int) ([]model.Order, error) {
	rows, err := d.Conn.QueryContext(ctx, `SELECT `+orderColumns+`
        FROM blnk.orders WHERE status = $1 ORDER BY created_at ASC LIMIT $2 OFFSET $3`, status, limit, offset)
	if err != nil {
		return nil, apierror.NewAPIError(apierror.ErrInternalServer, "Failed to query orders", err)
	}
	defer func() { _ = rows.Close() }()
	var out []model.Order
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, apierror.NewAPIError(apierror.ErrInternalServer, "Failed to scan order", err)
		}
		out = append(out, *o)
	}
	return out, rows.Err()
}

// --- Dict (configurable reference) ---

// UpsertDict stores a reference value.
func (d Datasource) UpsertDict(ctx context.Context, e model.DictEntry) (model.DictEntry, error) {
	if e.Category == "" || e.Code == "" {
		return model.DictEntry{}, apierror.NewAPIError(apierror.ErrBadRequest, "dict category and code are required", nil)
	}
	err := d.Conn.QueryRowContext(ctx, `
        INSERT INTO blnk.dict (category, code, name, sort, active)
        VALUES ($1,$2,$3,$4,$5)
        ON CONFLICT (category, code) DO UPDATE
            SET name = EXCLUDED.name, sort = EXCLUDED.sort, active = EXCLUDED.active
        RETURNING created_at`, e.Category, e.Code, e.Name, e.Sort, e.Active,
	).Scan(&e.CreatedAt)
	if err != nil {
		return model.DictEntry{}, apierror.NewAPIError(apierror.ErrInternalServer, "Failed to upsert dict", err)
	}
	return e, nil
}

// GetDict lists active reference values of a category, ordered by sort.
func (d Datasource) GetDict(ctx context.Context, category string) ([]model.DictEntry, error) {
	rows, err := d.Conn.QueryContext(ctx, `
        SELECT category, code, name, sort, active, created_at
        FROM blnk.dict WHERE category = $1 AND active = TRUE ORDER BY sort ASC, code ASC`, category)
	if err != nil {
		return nil, apierror.NewAPIError(apierror.ErrInternalServer, "Failed to query dict", err)
	}
	defer func() { _ = rows.Close() }()
	var out []model.DictEntry
	for rows.Next() {
		var e model.DictEntry
		if err := rows.Scan(&e.Category, &e.Code, &e.Name, &e.Sort, &e.Active, &e.CreatedAt); err != nil {
			return nil, apierror.NewAPIError(apierror.ErrInternalServer, "Failed to scan dict", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// --- Stop list ---

// AddStopList blocks an identity (optionally for one instrument).
func (d Datasource) AddStopList(ctx context.Context, e model.StopListEntry) (model.StopListEntry, error) {
	if e.IdentityID == "" {
		return model.StopListEntry{}, apierror.NewAPIError(apierror.ErrBadRequest, "identity_id is required", nil)
	}
	err := d.Conn.QueryRowContext(ctx, `
        INSERT INTO blnk.stop_list (identity_id, instrument, reason, active)
        VALUES ($1,$2,$3,TRUE)
        ON CONFLICT (identity_id, instrument) DO UPDATE SET reason = EXCLUDED.reason, active = TRUE
        RETURNING id, created_at`, e.IdentityID, e.Instrument, e.Reason,
	).Scan(&e.ID, &e.CreatedAt)
	if err != nil {
		return model.StopListEntry{}, apierror.NewAPIError(apierror.ErrInternalServer, "Failed to add stop list entry", err)
	}
	e.Active = true
	return e, nil
}

// IsStopListed reports whether the identity is blocked for the instrument
// (an instrument-specific entry or a global '' entry).
func (d Datasource) IsStopListed(ctx context.Context, identityID, instrument string) (bool, string, error) {
	var reason sql.NullString
	err := d.Conn.QueryRowContext(ctx, `
        SELECT COALESCE(reason,'') FROM blnk.stop_list
        WHERE identity_id = $1 AND active = TRUE AND (instrument = $2 OR instrument = '')
        ORDER BY instrument DESC LIMIT 1`, identityID, instrument,
	).Scan(&reason)
	if err == sql.ErrNoRows {
		return false, "", nil
	}
	if err != nil {
		return false, "", apierror.NewAPIError(apierror.ErrInternalServer, "Failed to query stop list", err)
	}
	return true, reason.String, nil
}

// --- Trading times ---

// SetTradingTime upserts a venue trading window for a weekday.
func (d Datasource) SetTradingTime(ctx context.Context, w model.TradingTime) (model.TradingTime, error) {
	if w.Venue == "" || w.Weekday < 0 || w.Weekday > 6 {
		return model.TradingTime{}, apierror.NewAPIError(apierror.ErrBadRequest, "venue and weekday 0..6 are required", nil)
	}
	err := d.Conn.QueryRowContext(ctx, `
        INSERT INTO blnk.trading_times (venue, weekday, open_time, close_time)
        VALUES ($1,$2,$3,$4)
        ON CONFLICT (venue, weekday) DO UPDATE SET open_time = EXCLUDED.open_time, close_time = EXCLUDED.close_time
        RETURNING id, created_at`, w.Venue, w.Weekday, w.OpenTime, w.CloseTime,
	).Scan(&w.ID, &w.CreatedAt)
	if err != nil {
		return model.TradingTime{}, apierror.NewAPIError(apierror.ErrInternalServer, "Failed to set trading time", err)
	}
	return w, nil
}

// GetTradingTimes lists a venue's trading windows.
func (d Datasource) GetTradingTimes(ctx context.Context, venue string) ([]model.TradingTime, error) {
	rows, err := d.Conn.QueryContext(ctx, `
        SELECT id, venue, weekday, open_time, close_time, created_at
        FROM blnk.trading_times WHERE venue = $1 ORDER BY weekday ASC`, venue)
	if err != nil {
		return nil, apierror.NewAPIError(apierror.ErrInternalServer, "Failed to query trading times", err)
	}
	defer func() { _ = rows.Close() }()
	var out []model.TradingTime
	for rows.Next() {
		var w model.TradingTime
		if err := rows.Scan(&w.ID, &w.Venue, &w.Weekday, &w.OpenTime, &w.CloseTime, &w.CreatedAt); err != nil {
			return nil, apierror.NewAPIError(apierror.ErrInternalServer, "Failed to scan trading time", err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}
