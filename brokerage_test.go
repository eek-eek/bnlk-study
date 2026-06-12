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
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/blnkfinance/blnk/config"
	"github.com/blnkfinance/blnk/database/mocks"
	"github.com/blnkfinance/blnk/internal/apierror"
	"github.com/blnkfinance/blnk/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// newBrokerageTestBlnk wires a Blnk instance with miniredis and a mock
// datasource, mirroring the setup used by the webhook tests.
func newBrokerageTestBlnk(t *testing.T) (*Blnk, *mocks.MockDataSource, func()) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("an error '%s' occurred when starting miniredis", err)
	}

	cnf := &config.Configuration{
		Redis: config.RedisConfig{
			Dns: mr.Addr(),
		},
		Queue: config.QueueConfig{
			WebhookQueue:   "webhook_queue",
			NumberOfQueues: 1,
		},
		Transaction: config.TransactionConfig{
			LockDuration:    30 * time.Second,
			LockWaitTimeout: 2 * time.Second,
		},
	}
	config.ConfigStore.Store(cnf)

	datasource := new(mocks.MockDataSource)
	service, err := NewBlnk(datasource)
	assert.NoError(t, err)

	return service, datasource, mr.Close
}

func TestApplyMutationPlan_NormalizesAndDelegates(t *testing.T) {
	service, datasource, cleanup := newBrokerageTestBlnk(t)
	defer cleanup()

	key := model.PositionKey{LedgerID: "ldg1", AccountRef: "acc1", Currency: "KZT"}
	plan := model.MutationPlan{Deltas: []model.BalanceDelta{
		{Key: key, AmountDelta: big.NewInt(100)},
		{Key: key, AmountDelta: big.NewInt(-40), BlockedDelta: big.NewInt(10)},
	}}

	datasource.On("ApplyBalanceDeltas", mock.Anything, mock.MatchedBy(func(deltas []model.BalanceDelta) bool {
		return len(deltas) == 1 &&
			deltas[0].AmountDelta.Int64() == 60 &&
			deltas[0].BlockedDelta.Int64() == 10
	})).Return(nil)

	err := service.ApplyMutationPlan(context.Background(), plan)
	assert.NoError(t, err)
	datasource.AssertExpectations(t)
}

func TestApplyMutationPlan_NoOpPlanSkipsDatasource(t *testing.T) {
	service, datasource, cleanup := newBrokerageTestBlnk(t)
	defer cleanup()

	key := model.PositionKey{LedgerID: "ldg1", AccountRef: "acc1", Currency: "KZT"}
	plan := model.MutationPlan{Deltas: []model.BalanceDelta{
		{Key: key, AmountDelta: big.NewInt(70)},
		{Key: key, AmountDelta: big.NewInt(-70)},
	}}

	err := service.ApplyMutationPlan(context.Background(), plan)
	assert.NoError(t, err)
	datasource.AssertNotCalled(t, "ApplyBalanceDeltas", mock.Anything, mock.Anything)
}

func TestApplyMutationPlan_RejectsInvalidPlan(t *testing.T) {
	service, _, cleanup := newBrokerageTestBlnk(t)
	defer cleanup()

	err := service.ApplyMutationPlan(context.Background(), model.MutationPlan{})
	assert.Error(t, err)

	missingLedger := model.MutationPlan{Deltas: []model.BalanceDelta{
		{Key: model.PositionKey{AccountRef: "acc1", Currency: "KZT"}, AmountDelta: big.NewInt(1)},
	}}
	err = service.ApplyMutationPlan(context.Background(), missingLedger)
	assert.Error(t, err)
}

func TestGetFreeBalance_AppliesThresholds(t *testing.T) {
	service, datasource, cleanup := newBrokerageTestBlnk(t)
	defer cleanup()

	balance := &model.Balance{
		BalanceID:            "bln_1",
		Currency:             "KZT",
		Balance:              big.NewInt(10000),
		InflightDebitBalance: big.NewInt(2000), // blocked
		QueuedDebitBalance:   big.NewInt(3000), // queued obligations
	}
	datasource.On("GetBalanceByID", "bln_1", []string(nil), true).Return(balance, nil)

	// sum1 = 10000 - 2000 = 8000; sum2 = 3000; available = 5000
	result, err := service.GetFreeBalance(context.Background(), "bln_1", nil)
	assert.NoError(t, err)
	assert.True(t, result.IsSuccess)
	assert.Equal(t, int64(5000), result.AvailableAmount.Int64())
	assert.Equal(t, "KZT", result.Currency)

	// With a cap below the difference the cap wins (Sum0 semantics).
	result, err = service.GetFreeBalance(context.Background(), "bln_1", big.NewInt(4000))
	assert.NoError(t, err)
	assert.Equal(t, int64(4000), result.AvailableAmount.Int64())
}

func TestGetFreeBalance_PropagatesLookupErrors(t *testing.T) {
	service, datasource, cleanup := newBrokerageTestBlnk(t)
	defer cleanup()

	datasource.On("GetBalanceByID", "missing", []string(nil), true).
		Return((*model.Balance)(nil), fmt.Errorf("balance not found"))

	result, err := service.GetFreeBalance(context.Background(), "missing", nil)
	assert.Error(t, err)
	assert.False(t, result.IsSuccess)
	assert.NotEmpty(t, result.ErrorMessage)
}

func TestRecalculateHolds_AggregatesErrorsPerBalance(t *testing.T) {
	service, datasource, cleanup := newBrokerageTestBlnk(t)
	defer cleanup()

	datasource.On("RecomputeHolds", mock.Anything, "bln_ok").
		Return(big.NewInt(100), big.NewInt(0), nil)
	datasource.On("RecomputeHolds", mock.Anything, "bln_bad").
		Return(nil, nil, fmt.Errorf("boom"))

	result, err := service.RecalculateHolds(context.Background(), []string{"bln_ok", "bln_bad"})
	assert.NoError(t, err)
	assert.Equal(t, []string{"bln_ok"}, result.Recalculated)
	assert.Equal(t, "boom", result.Errors["bln_bad"])
}

func TestRecalculateHolds_RequiresBalanceIDs(t *testing.T) {
	service, _, cleanup := newBrokerageTestBlnk(t)
	defer cleanup()

	_, err := service.RecalculateHolds(context.Background(), nil)
	assert.Error(t, err)
}

func TestComputeSettleDate_UsesVenueHolidays(t *testing.T) {
	service, datasource, cleanup := newBrokerageTestBlnk(t)
	defer cleanup()

	tradeDate := time.Date(2026, 6, 12, 10, 0, 0, 0, time.UTC) // Friday
	holidayMonday := model.MarketHoliday{
		Venue:       "KASE",
		HolidayDate: time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC),
	}
	datasource.On("GetHolidays", mock.Anything, "KASE", mock.Anything, mock.Anything).
		Return([]model.MarketHoliday{holidayMonday}, nil)

	// T+2 from Friday: weekend skipped, Monday is a KASE holiday ->
	// Tuesday counts 1, Wednesday counts 2.
	settleDate, err := service.ComputeSettleDate(context.Background(), "KASE", tradeDate, 2)
	assert.NoError(t, err)
	assert.Equal(t, "2026-06-17", settleDate.Format(model.HolidayKeyFormat))
	datasource.AssertExpectations(t)
}

// --- Scenario: 100 AAPL settled, buy 50 T+2, sell 125 today ---
//
// These tests verify the heart of the requirement: the sell availability is
// computed with settle-date arithmetic, and the in-transit 50 are counted only
// when the instrument is flagged as trading on the way.

func TestGetTradablePosition_OnTheWayCountsIncoming(t *testing.T) {
	service, datasource, cleanup := newBrokerageTestBlnk(t)
	defer cleanup()

	// AAPL trades T+2 on the way.
	datasource.On("GetInstrumentSettings", mock.Anything, "AAPL").
		Return(&model.InstrumentSettings{Instrument: "AAPL", TradesOnTheWay: true, SettleOffset: 2}, nil)
	// Settled spot position: 100, nothing blocked.
	datasource.On("GetPosition", mock.Anything, mock.MatchedBy(func(k model.PositionKey) bool {
		return k.Instrument == "AAPL" && k.SettleCode == nil
	})).Return(&model.Balance{
		BalanceID: "bln_spot", Balance: big.NewInt(100), InflightDebitBalance: big.NewInt(0),
	}, nil)
	// In transit by the sell settle date: +50 incoming (the buy), nothing outgoing.
	datasource.On("SumFutureHolds", mock.Anything, "ldg", "idn", "acc", "AAPL", "USD", mock.Anything).
		Return(big.NewInt(50), big.NewInt(0), nil)

	asOf := time.Date(2026, 6, 16, 0, 0, 0, 0, time.UTC)
	tradable, err := service.GetTradablePosition(context.Background(), "ldg", "idn", "acc", "AAPL", "USD", asOf)
	assert.NoError(t, err)
	assert.True(t, tradable.OnTheWay)
	// 100 settled - 0 blocked + 50 incoming - 0 outgoing = 150.
	assert.Equal(t, int64(150), tradable.Tradable.Int64())
}

func TestGetTradablePosition_ImmediateIgnoresIncoming(t *testing.T) {
	service, datasource, cleanup := newBrokerageTestBlnk(t)
	defer cleanup()

	// MSFT has no on-the-way flag.
	datasource.On("GetInstrumentSettings", mock.Anything, "MSFT").
		Return((*model.InstrumentSettings)(nil), apierror.NewAPIError(apierror.ErrNotFound, "none", nil))
	datasource.On("GetPosition", mock.Anything, mock.Anything).Return(&model.Balance{
		BalanceID: "bln_spot", Balance: big.NewInt(100), InflightDebitBalance: big.NewInt(0),
	}, nil)
	// SumFutureHolds must NOT be consulted for an immediate-settlement instrument.

	asOf := time.Date(2026, 6, 16, 0, 0, 0, 0, time.UTC)
	tradable, err := service.GetTradablePosition(context.Background(), "ldg", "idn", "acc", "MSFT", "USD", asOf)
	assert.NoError(t, err)
	assert.False(t, tradable.OnTheWay)
	// Only the settled 100 is tradable; the future 50 is ignored.
	assert.Equal(t, int64(100), tradable.Tradable.Int64())
	datasource.AssertNotCalled(t, "SumFutureHolds", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

func TestSellTrade_RejectsWhenImmediateInstrumentLacksSettled(t *testing.T) {
	service, datasource, cleanup := newBrokerageTestBlnk(t)
	defer cleanup()

	// Immediate-settlement instrument: only the settled 100 can be sold, so 125 must be rejected.
	datasource.On("GetInstrumentSettings", mock.Anything, "MSFT").
		Return((*model.InstrumentSettings)(nil), apierror.NewAPIError(apierror.ErrNotFound, "none", nil))
	datasource.On("GetPosition", mock.Anything, mock.Anything).Return(&model.Balance{
		BalanceID: "bln_spot", Balance: big.NewInt(100), InflightDebitBalance: big.NewInt(0),
	}, nil)

	_, err := service.SellTrade(context.Background(), model.SellBooking{
		LedgerID: "ldg", IdentityID: "idn", AccountRef: "acc",
		Instrument: "MSFT", Venue: "", Currency: "USD",
		Quantity: 125, QuantityPrecision: 1, Price: "200.00", MoneyPrecision: 100,
		SettleOffset: 2, SettlementBalanceID: "bln_settle", MarketBalanceID: "bln_market",
		Reference: "sell_001",
	})
	assert.Error(t, err)
	apiErr, ok := err.(apierror.APIError)
	assert.True(t, ok)
	assert.Equal(t, apierror.ErrBadRequest, apiErr.Code)
	assert.Contains(t, err.Error(), "insufficient tradable")
	// Rejected before any booking.
	datasource.AssertNotCalled(t, "RecordTransaction", mock.Anything, mock.Anything)
}

func TestSellTrade_RejectsWhenExceedingTradableOnTheWay(t *testing.T) {
	service, datasource, cleanup := newBrokerageTestBlnk(t)
	defer cleanup()

	// On-the-way: tradable = 100 + 50 = 150; selling 200 must be rejected.
	datasource.On("GetInstrumentSettings", mock.Anything, "AAPL").
		Return(&model.InstrumentSettings{Instrument: "AAPL", TradesOnTheWay: true, SettleOffset: 2}, nil)
	datasource.On("GetHolidays", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return([]model.MarketHoliday{}, nil)
	datasource.On("GetPosition", mock.Anything, mock.Anything).Return(&model.Balance{
		BalanceID: "bln_spot", Balance: big.NewInt(100), InflightDebitBalance: big.NewInt(0),
	}, nil)
	datasource.On("SumFutureHolds", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(big.NewInt(50), big.NewInt(0), nil)

	_, err := service.SellTrade(context.Background(), model.SellBooking{
		LedgerID: "ldg", IdentityID: "idn", AccountRef: "acc",
		Instrument: "AAPL", Venue: "KASE", Currency: "USD",
		Quantity: 200, QuantityPrecision: 1, Price: "190.00", MoneyPrecision: 100,
		SettleOffset: 2, SettlementBalanceID: "bln_settle", MarketBalanceID: "bln_market",
		Reference: "sell_002",
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "insufficient tradable")
	datasource.AssertNotCalled(t, "RecordTransaction", mock.Anything, mock.Anything)
}

func TestReconcileSettlement_RecomputesAndCompletes(t *testing.T) {
	service, datasource, cleanup := newBrokerageTestBlnk(t)
	defer cleanup()

	// One pending journal: spot had 100 @ 150 (precise, precision 1), buy 50 @ 180.
	// wa_after = (50*180 + 150*100) / 150 = 24000/150 = 160.00.
	pending := model.SettlementJournalEntry{
		SecurityTxnID: "txn_sec",
		TradeRef:      "buy-50",
		SpotBalanceID: "bln_spot",
		Instrument:    "AAPL",
		Side:          model.TradeSideBuy,
		WABefore:      "150.00",
		QtyBefore:     "100",
		Price:         "180",
		Quantity:      "50",
		Precision:     1,
		Currency:      "USD",
		Status:        model.SettlementPending,
	}
	datasource.On("ListPendingSettlementJournals", mock.Anything, mock.Anything).
		Return([]model.SettlementJournalEntry{pending}, nil)
	datasource.On("CompleteSettlementJournal", mock.Anything, "txn_sec", "160.00",
		mock.MatchedBy(func(lot model.BalanceLot) bool {
			return lot.BalanceID == "bln_spot" && lot.Reference == "buy-50" &&
				lot.Quantity.Int64() == 50 && lot.Price == "180"
		})).Return("lot_1", nil)

	n, err := service.ReconcileSettlement(context.Background(), 10)
	assert.NoError(t, err)
	assert.Equal(t, 1, n)
	datasource.AssertExpectations(t)
}

func TestComputeSettleDate_EmptyVenueSkipsCalendarLookup(t *testing.T) {
	service, datasource, cleanup := newBrokerageTestBlnk(t)
	defer cleanup()

	tradeDate := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC) // Wednesday
	settleDate, err := service.ComputeSettleDate(context.Background(), "", tradeDate, 1)
	assert.NoError(t, err)
	assert.Equal(t, "2026-06-11", settleDate.Format(model.HolidayKeyFormat))
	datasource.AssertNotCalled(t, "GetHolidays", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}
