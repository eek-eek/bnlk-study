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
package blnk

import (
	"context"
	"testing"
	"time"

	"github.com/blnkfinance/blnk/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOrderLifecycle_BuyStraightThrough: a buy order submitted straight-through
// is validated, approved, executed into a trade, and settles into a spot
// position. Also checks the discrete steps leave the right statuses.
func TestOrderLifecycle_BuyStraightThrough(t *testing.T) {
	service := newIntegrationBlnk(t)
	ctx := context.Background()
	ledgerID := mustCreateLedger(t, service)
	const (
		accountRef = "ord-buy"
		instrument = "ORDAAPL"
		venue      = "NASDAQ"
		currency   = "USD"
	)
	_, err := service.SetInstrumentSettings(ctx, model.InstrumentSettings{
		Instrument: instrument, Venue: venue, TradesOnTheWay: true, SettleOffset: 2,
	})
	require.NoError(t, err)
	market, settlement := fundAndBalances(t, service, ctx, ledgerID, accountRef, currency)

	executed, err := service.SubmitOrder(ctx, model.Order{
		Reference: "ord-buy-1", LedgerID: ledgerID, AccountRef: accountRef,
		Instrument: instrument, Venue: venue, Currency: currency, Side: model.TradeSideBuy,
		Quantity: "10", QuantityPrecision: 1, Price: "150.00", MoneyPrecision: 100,
		SettleOffset: 2, SettlementBalanceID: settlement, MarketBalanceID: market,
	})
	require.NoError(t, err)
	assert.Equal(t, model.OrderStatusExecuted, executed.Status)
	assert.NotEmpty(t, executed.SecurityTxnID)
	assert.NotEmpty(t, executed.MoneyTxnID)
	assert.Equal(t, model.AMLStatusPassed, executed.AMLStatus)

	// Settle and verify the spot position is 10.
	_, err = service.RunSettlement(ctx, time.Now().AddDate(0, 0, 5), 100)
	require.NoError(t, err)
	spot, err := service.GetActivePosition(ctx, ledgerID, "", accountRef, instrument, currency, nil)
	require.NoError(t, err)
	assert.Equal(t, int64(10), spot.Balance.Int64())
}

// TestOrderLifecycle_DiscreteSteps walks DRAFT -> CHECKED -> APPROVED -> EXECUTED.
func TestOrderLifecycle_DiscreteSteps(t *testing.T) {
	service := newIntegrationBlnk(t)
	ctx := context.Background()
	ledgerID := mustCreateLedger(t, service)
	const (
		accountRef = "ord-steps"
		instrument = "ORDSTEP"
		venue      = "NASDAQ"
		currency   = "USD"
	)
	_, err := service.SetInstrumentSettings(ctx, model.InstrumentSettings{
		Instrument: instrument, Venue: venue, TradesOnTheWay: true, SettleOffset: 2,
	})
	require.NoError(t, err)
	market, settlement := fundAndBalances(t, service, ctx, ledgerID, accountRef, currency)

	created, err := service.CreateOrder(ctx, model.Order{
		Reference: "ord-steps-1", LedgerID: ledgerID, AccountRef: accountRef,
		Instrument: instrument, Venue: venue, Currency: currency, Side: model.TradeSideBuy,
		Quantity: "5", QuantityPrecision: 1, Price: "100.00", MoneyPrecision: 100,
		SettleOffset: 2, SettlementBalanceID: settlement, MarketBalanceID: market,
	})
	require.NoError(t, err)
	assert.Equal(t, model.OrderStatusDraft, created.Status)

	checked, err := service.CheckOrder(ctx, created.OrderID)
	require.NoError(t, err)
	assert.Equal(t, model.OrderStatusChecked, checked.Status)

	approved, err := service.ApproveOrder(ctx, created.OrderID)
	require.NoError(t, err)
	assert.Equal(t, model.OrderStatusApproved, approved.Status)

	executed, err := service.ExecuteOrder(ctx, created.OrderID)
	require.NoError(t, err)
	assert.Equal(t, model.OrderStatusExecuted, executed.Status)

	// Re-executing a terminal order is rejected.
	_, err = service.ExecuteOrder(ctx, created.OrderID)
	assert.Error(t, err)
}

// TestOrderLifecycle_SellOrder: seed 100, submit a sell of 40, settle -> 60 left.
func TestOrderLifecycle_SellOrder(t *testing.T) {
	service := newIntegrationBlnk(t)
	ctx := context.Background()
	ledgerID := mustCreateLedger(t, service)
	const (
		accountRef = "ord-sell"
		instrument = "ORDSELL"
		venue      = "NASDAQ"
		currency   = "USD"
	)
	_, err := service.SetInstrumentSettings(ctx, model.InstrumentSettings{
		Instrument: instrument, Venue: venue, TradesOnTheWay: true, SettleOffset: 2,
	})
	require.NoError(t, err)
	market, settlement := fundAndBalances(t, service, ctx, ledgerID, accountRef, currency)
	seedSettledViaBuy(t, service, ctx, ledgerID, accountRef, instrument, venue, currency, "100", "150.00", market, settlement)

	executed, err := service.SubmitOrder(ctx, model.Order{
		Reference: "ord-sell-1", LedgerID: ledgerID, AccountRef: accountRef,
		Instrument: instrument, Venue: venue, Currency: currency, Side: model.TradeSideSell,
		Quantity: "40", QuantityPrecision: 1, Price: "185.00", MoneyPrecision: 100,
		SettleOffset: 2, SettlementBalanceID: settlement, MarketBalanceID: market,
	})
	require.NoError(t, err)
	assert.Equal(t, model.OrderStatusExecuted, executed.Status)

	_, err = service.RunSettlement(ctx, time.Now().AddDate(0, 0, 5), 100)
	require.NoError(t, err)
	spot, err := service.GetActivePosition(ctx, ledgerID, "", accountRef, instrument, currency, nil)
	require.NoError(t, err)
	assert.Equal(t, int64(60), spot.Balance.Int64())
}

// TestOrderLifecycle_RejectInsufficientFunds: a buy with an unfunded money
// balance is rejected at the funds check.
func TestOrderLifecycle_RejectInsufficientFunds(t *testing.T) {
	service := newIntegrationBlnk(t)
	ctx := context.Background()
	ledgerID := mustCreateLedger(t, service)
	const (
		accountRef = "ord-nofunds"
		instrument = "ORDNF"
		currency   = "USD"
	)
	market, err := service.CreateBalance(ctx, model.Balance{LedgerID: ledgerID, Currency: currency})
	require.NoError(t, err)
	settlement, err := service.CreateBalance(ctx, model.Balance{LedgerID: ledgerID, Currency: currency})
	require.NoError(t, err)

	rejected, err := service.SubmitOrder(ctx, model.Order{
		Reference: "ord-nf-1", LedgerID: ledgerID, AccountRef: accountRef,
		Instrument: instrument, Currency: currency, Side: model.TradeSideBuy,
		Quantity: "10", QuantityPrecision: 1, Price: "150.00", MoneyPrecision: 100,
		SettlementBalanceID: settlement.BalanceID, MarketBalanceID: market.BalanceID,
	})
	require.NoError(t, err)
	assert.Equal(t, model.OrderStatusRejected, rejected.Status)
	assert.Equal(t, "insufficient funds", rejected.RejectReason)
}

// TestOrderLifecycle_RejectStopListed: a stop-listed identity is rejected.
func TestOrderLifecycle_RejectStopListed(t *testing.T) {
	service := newIntegrationBlnk(t)
	ctx := context.Background()
	ledgerID := mustCreateLedger(t, service)
	const (
		accountRef = "ord-stop"
		instrument = "ORDSTOP"
		currency   = "USD"
		identityID = "idn-blocked"
	)
	market, err := service.CreateBalance(ctx, model.Balance{LedgerID: ledgerID, Currency: currency})
	require.NoError(t, err)
	settlement, err := service.CreateBalance(ctx, model.Balance{LedgerID: ledgerID, Currency: currency})
	require.NoError(t, err)
	_, err = service.AddStopList(ctx, model.StopListEntry{IdentityID: identityID, Reason: "sanctions"})
	require.NoError(t, err)

	rejected, err := service.SubmitOrder(ctx, model.Order{
		Reference: "ord-stop-1", LedgerID: ledgerID, IdentityID: identityID, AccountRef: accountRef,
		Instrument: instrument, Currency: currency, Side: model.TradeSideBuy,
		Quantity: "10", QuantityPrecision: 1, Price: "150.00", MoneyPrecision: 100,
		SettlementBalanceID: settlement.BalanceID, MarketBalanceID: market.BalanceID,
	})
	require.NoError(t, err)
	assert.Equal(t, model.OrderStatusRejected, rejected.Status)
	assert.Equal(t, "stop list", rejected.RejectReason)
	assert.Equal(t, model.AMLStatusBlocked, rejected.AMLStatus)
}

// TestOrderLifecycle_RejectSizeLimit: an order above the instrument's max size
// is rejected.
func TestOrderLifecycle_RejectSizeLimit(t *testing.T) {
	service := newIntegrationBlnk(t)
	ctx := context.Background()
	ledgerID := mustCreateLedger(t, service)
	const (
		accountRef = "ord-size"
		instrument = "ORDSIZE"
		currency   = "USD"
	)
	_, err := service.SetInstrumentSettings(ctx, model.InstrumentSettings{
		Instrument: instrument, MaxOrderQuantity: "5",
	})
	require.NoError(t, err)
	market, err := service.CreateBalance(ctx, model.Balance{LedgerID: ledgerID, Currency: currency})
	require.NoError(t, err)
	settlement, err := service.CreateBalance(ctx, model.Balance{LedgerID: ledgerID, Currency: currency})
	require.NoError(t, err)

	rejected, err := service.SubmitOrder(ctx, model.Order{
		Reference: "ord-size-1", LedgerID: ledgerID, AccountRef: accountRef,
		Instrument: instrument, Currency: currency, Side: model.TradeSideBuy,
		Quantity: "10", QuantityPrecision: 1, Price: "150.00", MoneyPrecision: 100,
		SettlementBalanceID: settlement.BalanceID, MarketBalanceID: market.BalanceID,
	})
	require.NoError(t, err)
	assert.Equal(t, model.OrderStatusRejected, rejected.Status)
	assert.Equal(t, "above maximum order quantity", rejected.RejectReason)
}

// TestOrderLifecycle_RejectOutsideTradingHours: a venue whose only window is on
// a different weekday is closed today, so the order is rejected.
func TestOrderLifecycle_RejectOutsideTradingHours(t *testing.T) {
	service := newIntegrationBlnk(t)
	ctx := context.Background()
	ledgerID := mustCreateLedger(t, service)
	const (
		accountRef = "ord-hours"
		instrument = "ORDHRS"
		venue      = "CLOSEDVENUE"
		currency   = "USD"
	)
	// Configure a window only for a weekday that is NOT today -> today is closed.
	otherWeekday := (int(time.Now().UTC().Weekday()) + 1) % 7
	_, err := service.SetTradingTime(ctx, model.TradingTime{
		Venue: venue, Weekday: otherWeekday, OpenTime: "00:00", CloseTime: "23:59",
	})
	require.NoError(t, err)
	market, err := service.CreateBalance(ctx, model.Balance{LedgerID: ledgerID, Currency: currency})
	require.NoError(t, err)
	settlement, err := service.CreateBalance(ctx, model.Balance{LedgerID: ledgerID, Currency: currency})
	require.NoError(t, err)

	rejected, err := service.SubmitOrder(ctx, model.Order{
		Reference: "ord-hrs-1", LedgerID: ledgerID, AccountRef: accountRef,
		Instrument: instrument, Venue: venue, Currency: currency, Side: model.TradeSideBuy,
		Quantity: "10", QuantityPrecision: 1, Price: "150.00", MoneyPrecision: 100,
		SettlementBalanceID: settlement.BalanceID, MarketBalanceID: market.BalanceID,
	})
	require.NoError(t, err)
	assert.Equal(t, model.OrderStatusRejected, rejected.Status)
	assert.Equal(t, "outside trading hours", rejected.RejectReason)
}
