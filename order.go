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

// Order lifecycle service: the TradeControl "Trade Control" decision core in
// front of trade booking. An order moves DRAFT -> CHECKED -> APPROVED ->
// EXECUTED (or REJECTED/CANCELLED). CheckOrder runs the validation handlers
// (stop-list, trading window, size limits, sufficient funds/position) and
// ExecuteOrder books the brokerage trade (BookTrade/SellTrade) that the
// settlement engine later settles.
package blnk

import (
	"context"
	"fmt"
	"time"

	"github.com/blnkfinance/blnk/internal/apierror"
	"github.com/blnkfinance/blnk/internal/metrics"
	"github.com/blnkfinance/blnk/model"
	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/metric"
)

// CreateOrder validates and persists a new draft order.
func (l *Blnk) CreateOrder(ctx context.Context, o model.Order) (*model.Order, error) {
	if err := o.Validate(); err != nil {
		return nil, apierror.NewAPIError(apierror.ErrBadRequest, err.Error(), err)
	}
	o.Status = model.OrderStatusDraft
	return l.datasource.CreateOrder(ctx, o)
}

// GetOrder retrieves an order by ID.
func (l *Blnk) GetOrder(ctx context.Context, orderID string) (*model.Order, error) {
	return l.datasource.GetOrder(ctx, orderID)
}

// orderSettleDate resolves the settlement date the order will book to, honoring
// instrument settings and any explicit settle date.
func (l *Blnk) orderSettleDate(ctx context.Context, o *model.Order) (time.Time, error) {
	params, err := l.resolveTradeParams(ctx, o.Instrument, o.SettleOffset, o.Venue)
	if err != nil {
		return time.Time{}, err
	}
	explicit := time.Time{}
	if o.SettleDate != nil {
		explicit = *o.SettleDate
	}
	settleDate, _, err := l.resolveSettlement(ctx, params, time.Now(), explicit)
	return settleDate, err
}

// CheckOrder runs the validation handlers and moves the order to CHECKED or
// REJECTED. A rejection is a normal business outcome (no error returned).
func (l *Blnk) CheckOrder(ctx context.Context, orderID string) (*model.Order, error) {
	o, err := l.datasource.GetOrder(ctx, orderID)
	if err != nil {
		return nil, err
	}
	if o.Status != model.OrderStatusDraft {
		return nil, apierror.NewAPIError(apierror.ErrBadRequest, fmt.Sprintf("order %s cannot be checked from status %s", orderID, o.Status), nil)
	}

	result, err := l.runOrderChecks(ctx, o)
	if err != nil {
		return nil, err
	}
	if !result.Passed {
		o.Status = model.OrderStatusRejected
		o.RejectReason = result.RejectReason
		o.StatusMessage = "validation failed: " + result.RejectReason
		if result.RejectReason == "stop list" {
			o.AMLStatus = model.AMLStatusBlocked
		}
		metrics.OrderRejectedTotal.Add(ctx, 1, otelmetric.WithAttributes(attribute.String("stage", "check")))
		if err := l.datasource.UpdateOrder(ctx, o); err != nil {
			return nil, err
		}
		return o, nil
	}

	o.Status = model.OrderStatusChecked
	o.AMLStatus = model.AMLStatusPassed
	o.StatusMessage = "checks passed"
	if err := l.datasource.UpdateOrder(ctx, o); err != nil {
		return nil, err
	}
	return o, nil
}

// runOrderChecks executes the validation handlers in order and returns the
// first failure (TradeControl OrderCheckService analog).
func (l *Blnk) runOrderChecks(ctx context.Context, o *model.Order) (model.OrderCheckResult, error) {
	result := model.OrderCheckResult{Passed: true}

	// 1. Stop list.
	result.Checks = append(result.Checks, "stop_list")
	if o.IdentityID != "" {
		blocked, _, err := l.datasource.IsStopListed(ctx, o.IdentityID, o.Instrument)
		if err != nil {
			return result, err
		}
		if blocked {
			return model.OrderCheckResult{Passed: false, RejectReason: "stop list", Checks: result.Checks}, nil
		}
	}

	// 2. Trading window (only when the venue has configured windows).
	result.Checks = append(result.Checks, "trading_time")
	if open, err := l.isVenueOpen(ctx, o.Venue, time.Now().UTC()); err != nil {
		return result, err
	} else if !open {
		return model.OrderCheckResult{Passed: false, RejectReason: "outside trading hours", Checks: result.Checks}, nil
	}

	// 3. Order size limits (instrument settings, when configured).
	result.Checks = append(result.Checks, "size_limits")
	if ok, reason, err := l.checkSizeLimits(ctx, o); err != nil {
		return result, err
	} else if !ok {
		return model.OrderCheckResult{Passed: false, RejectReason: reason, Checks: result.Checks}, nil
	}

	// 4. Sufficient funds (buy) / tradable position (sell).
	result.Checks = append(result.Checks, "funds")
	if ok, reason, err := l.checkFunds(ctx, o); err != nil {
		return result, err
	} else if !ok {
		return model.OrderCheckResult{Passed: false, RejectReason: reason, Checks: result.Checks}, nil
	}

	return result, nil
}

// isVenueOpen reports whether the venue is open at the given time. A venue with
// no configured windows is always open; otherwise the weekday must have a
// window containing the clock time.
func (l *Blnk) isVenueOpen(ctx context.Context, venue string, at time.Time) (bool, error) {
	if venue == "" {
		return true, nil
	}
	windows, err := l.datasource.GetTradingTimes(ctx, venue)
	if err != nil {
		return false, err
	}
	if len(windows) == 0 {
		return true, nil
	}
	weekday := int(at.Weekday())
	hhmm := at.Format("15:04")
	for _, w := range windows {
		if w.Weekday == weekday && w.IsOpenAt(hhmm) {
			return true, nil
		}
	}
	return false, nil
}

// checkSizeLimits enforces the instrument's optional min/max order quantity.
func (l *Blnk) checkSizeLimits(ctx context.Context, o *model.Order) (bool, string, error) {
	settings, err := l.datasource.GetInstrumentSettings(ctx, o.Instrument)
	if err != nil {
		if apiErr, ok := err.(apierror.APIError); ok && apiErr.Code == apierror.ErrNotFound {
			return true, "", nil
		}
		return false, "", err
	}
	qty, err := model.PreciseQuantity(o.Quantity, o.QuantityPrecision)
	if err != nil {
		return false, "", apierror.NewAPIError(apierror.ErrBadRequest, err.Error(), err)
	}
	if settings.MinOrderQuantity != "" {
		min, e := model.PreciseQuantity(settings.MinOrderQuantity, o.QuantityPrecision)
		if e == nil && qty.Cmp(min) < 0 {
			return false, "below minimum order quantity", nil
		}
	}
	if settings.MaxOrderQuantity != "" {
		max, e := model.PreciseQuantity(settings.MaxOrderQuantity, o.QuantityPrecision)
		if e == nil && qty.Cmp(max) > 0 {
			return false, "above maximum order quantity", nil
		}
	}
	return true, "", nil
}

// checkFunds verifies the client can fund a buy (free money balance) or deliver
// a sell (settle-date-aware tradable position).
func (l *Blnk) checkFunds(ctx context.Context, o *model.Order) (bool, string, error) {
	settleDate, err := l.orderSettleDate(ctx, o)
	if err != nil {
		return false, "", err
	}

	if o.Side == model.TradeSideSell {
		qty, err := model.PreciseQuantity(o.Quantity, o.QuantityPrecision)
		if err != nil {
			return false, "", apierror.NewAPIError(apierror.ErrBadRequest, err.Error(), err)
		}
		tradable, err := l.GetTradablePosition(ctx, o.LedgerID, o.IdentityID, o.AccountRef, o.Instrument, o.Currency, settleDate)
		if err != nil {
			return false, "", err
		}
		if qty.Cmp(tradable.Tradable) > 0 {
			return false, "insufficient tradable position", nil
		}
		return true, "", nil
	}

	// Buy: the client money balance must cover price * quantity.
	money, err := model.PreciseMoney(o.Quantity, o.Price, o.MoneyPrecision)
	if err != nil {
		return false, "", apierror.NewAPIError(apierror.ErrBadRequest, err.Error(), err)
	}
	moneyPos, err := l.GetOrCreatePosition(ctx, model.PositionKey{
		LedgerID: o.LedgerID, IdentityID: o.IdentityID, AccountRef: o.AccountRef, Currency: o.Currency,
	})
	if err != nil {
		return false, "", err
	}
	free, err := l.GetFreeBalance(ctx, moneyPos.BalanceID, nil)
	if err != nil {
		return false, "", err
	}
	if free.AvailableAmount.Cmp(money) < 0 {
		return false, "insufficient funds", nil
	}
	return true, "", nil
}

// ApproveOrder moves a CHECKED order to APPROVED (the approval gate; in a fuller
// build this is where multi-step sign-off lives).
func (l *Blnk) ApproveOrder(ctx context.Context, orderID string) (*model.Order, error) {
	o, err := l.datasource.GetOrder(ctx, orderID)
	if err != nil {
		return nil, err
	}
	if !model.CanTransitionOrder(o.Status, model.OrderStatusApproved) {
		return nil, apierror.NewAPIError(apierror.ErrBadRequest, fmt.Sprintf("order %s cannot be approved from status %s", orderID, o.Status), nil)
	}
	o.Status = model.OrderStatusApproved
	o.StatusMessage = "approved"
	if err := l.datasource.UpdateOrder(ctx, o); err != nil {
		return nil, err
	}
	return o, nil
}

// ExecuteOrder books the trade for an APPROVED order and moves it to EXECUTED.
func (l *Blnk) ExecuteOrder(ctx context.Context, orderID string) (*model.Order, error) {
	o, err := l.datasource.GetOrder(ctx, orderID)
	if err != nil {
		return nil, err
	}
	if !model.CanTransitionOrder(o.Status, model.OrderStatusExecuted) {
		return nil, apierror.NewAPIError(apierror.ErrBadRequest, fmt.Sprintf("order %s cannot be executed from status %s", orderID, o.Status), nil)
	}

	var tradeRef, securityTxn, moneyTxn string
	if o.Side == model.TradeSideSell {
		res, err := l.SellTrade(ctx, model.SellBooking{
			LedgerID: o.LedgerID, IdentityID: o.IdentityID, AccountRef: o.AccountRef,
			Instrument: o.Instrument, Venue: o.Venue, Currency: o.Currency,
			Quantity: o.Quantity, QuantityPrecision: o.QuantityPrecision, Price: o.Price, MoneyPrecision: o.MoneyPrecision,
			SettleOffset: o.SettleOffset, SettleDate: derefTime(o.SettleDate),
			SettlementBalanceID: o.SettlementBalanceID, MarketBalanceID: o.MarketBalanceID,
			Reference: o.Reference,
		})
		if err != nil {
			return l.rejectOrderOnExecute(ctx, o, err)
		}
		tradeRef, securityTxn, moneyTxn = res.TradeRef, res.SecurityTxnID, res.MoneyTxnID
	} else {
		res, err := l.BookTrade(ctx, model.TradeBooking{
			LedgerID: o.LedgerID, IdentityID: o.IdentityID, AccountRef: o.AccountRef,
			Instrument: o.Instrument, Venue: o.Venue, Currency: o.Currency,
			Quantity: o.Quantity, QuantityPrecision: o.QuantityPrecision, Price: o.Price, MoneyPrecision: o.MoneyPrecision,
			SettleOffset: o.SettleOffset, SettleDate: derefTime(o.SettleDate),
			SettlementBalanceID: o.SettlementBalanceID, MarketBalanceID: o.MarketBalanceID,
			Reference: o.Reference,
		})
		if err != nil {
			return l.rejectOrderOnExecute(ctx, o, err)
		}
		tradeRef, securityTxn, moneyTxn = res.TradeRef, res.SecurityTxnID, res.MoneyTxnID
	}

	o.Status = model.OrderStatusExecuted
	o.StatusMessage = "executed"
	o.TradeRef, o.SecurityTxnID, o.MoneyTxnID = tradeRef, securityTxn, moneyTxn
	if err := l.datasource.UpdateOrder(ctx, o); err != nil {
		return nil, err
	}
	metrics.OrderExecutedTotal.Add(ctx, 1, otelmetric.WithAttributes(attribute.String("side", o.Side)))
	return o, nil
}

// rejectOrderOnExecute marks an order REJECTED when its trade booking fails
// (e.g. a race that drained availability between check and execute).
func (l *Blnk) rejectOrderOnExecute(ctx context.Context, o *model.Order, cause error) (*model.Order, error) {
	o.Status = model.OrderStatusRejected
	o.RejectReason = "execution failed"
	o.StatusMessage = cause.Error()
	metrics.OrderRejectedTotal.Add(ctx, 1, otelmetric.WithAttributes(attribute.String("stage", "execute")))
	if err := l.datasource.UpdateOrder(ctx, o); err != nil {
		return nil, err
	}
	return o, nil
}

// CancelOrder cancels a non-terminal order.
func (l *Blnk) CancelOrder(ctx context.Context, orderID string) (*model.Order, error) {
	o, err := l.datasource.GetOrder(ctx, orderID)
	if err != nil {
		return nil, err
	}
	if !model.CanTransitionOrder(o.Status, model.OrderStatusCancelled) {
		return nil, apierror.NewAPIError(apierror.ErrBadRequest, fmt.Sprintf("order %s cannot be cancelled from status %s", orderID, o.Status), nil)
	}
	o.Status = model.OrderStatusCancelled
	o.StatusMessage = "cancelled"
	if err := l.datasource.UpdateOrder(ctx, o); err != nil {
		return nil, err
	}
	return o, nil
}

// SubmitOrder runs the full straight-through lifecycle: create, check, approve,
// execute. It stops (and returns) at the first rejection.
func (l *Blnk) SubmitOrder(ctx context.Context, o model.Order) (*model.Order, error) {
	created, err := l.CreateOrder(ctx, o)
	if err != nil {
		return nil, err
	}
	checked, err := l.CheckOrder(ctx, created.OrderID)
	if err != nil {
		return nil, err
	}
	if checked.Status == model.OrderStatusRejected {
		return checked, nil
	}
	approved, err := l.ApproveOrder(ctx, checked.OrderID)
	if err != nil {
		return nil, err
	}
	return l.ExecuteOrder(ctx, approved.OrderID)
}

// --- Reference data passthroughs ---

func (l *Blnk) UpsertDict(ctx context.Context, e model.DictEntry) (model.DictEntry, error) {
	return l.datasource.UpsertDict(ctx, e)
}
func (l *Blnk) GetDict(ctx context.Context, category string) ([]model.DictEntry, error) {
	return l.datasource.GetDict(ctx, category)
}
func (l *Blnk) AddStopList(ctx context.Context, e model.StopListEntry) (model.StopListEntry, error) {
	return l.datasource.AddStopList(ctx, e)
}
func (l *Blnk) SetTradingTime(ctx context.Context, w model.TradingTime) (model.TradingTime, error) {
	return l.datasource.SetTradingTime(ctx, w)
}
func (l *Blnk) GetTradingTimes(ctx context.Context, venue string) ([]model.TradingTime, error) {
	return l.datasource.GetTradingTimes(ctx, venue)
}

// derefTime returns the zero time for a nil pointer.
func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}
