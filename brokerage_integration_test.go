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
	"os"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/blnkfinance/blnk/config"
	"github.com/blnkfinance/blnk/database"
	"github.com/blnkfinance/blnk/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newIntegrationBlnk builds a Blnk wired to a real PostgreSQL (from
// BLNK_TEST_DATA_SOURCE_DNS) and miniredis. The whole brokerage suite is
// skipped when no database is configured, so it never fails in environments
// without a database while still verifying the full chain when one is present.
func newIntegrationBlnk(t *testing.T) *Blnk {
	service, _ := newIntegrationBlnkDS(t)
	return service
}

// newIntegrationBlnkDS also returns the datasource, for tests that seed/inspect
// rows directly (e.g. settlement journal recovery).
func newIntegrationBlnkDS(t *testing.T) (*Blnk, database.IDataSource) {
	t.Helper()
	dns := os.Getenv("BLNK_TEST_DATA_SOURCE_DNS")
	if dns == "" {
		t.Skip("BLNK_TEST_DATA_SOURCE_DNS not set; skipping brokerage integration test")
	}

	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)

	cnf := &config.Configuration{
		Redis:      config.RedisConfig{Dns: mr.Addr()},
		DataSource: config.DataSourceConfig{Dns: dns},
		Queue:      config.QueueConfig{NumberOfQueues: 1},
		Transaction: config.TransactionConfig{
			LockDuration:    30 * time.Second,
			LockWaitTimeout: 2 * time.Second,
		},
	}
	config.ConfigStore.Store(cnf)

	ds, err := database.NewDataSource(cnf)
	require.NoError(t, err)
	service, err := NewBlnk(ds)
	require.NoError(t, err)
	return service, ds
}

// seedSpotPosition creates a settled spot position of `qty` units by booking
// and immediately settling a T+0 buy, returning the ledger and account refs.
func mustCreateLedger(t *testing.T, service *Blnk) string {
	t.Helper()
	ledger, err := service.CreateLedger(model.Ledger{Name: "brokerage-test"})
	require.NoError(t, err)
	return ledger.LedgerID
}

// TestBrokerageChain_BuyT2ThenSellExceedingSettled drives the exact scenario:
// 100 AAPL settled, buy 50 (T+2), then today sell 125 (T+2). Because AAPL is
// flagged as trading on the way, the 50 in-transit count toward the available
// quantity (150), so the sell of 125 is accepted. After settlement the spot
// position is 25 at the blended weighted-average price.
func TestBrokerageChain_BuyT2ThenSellExceedingSettled(t *testing.T) {
	service := newIntegrationBlnk(t)
	ctx := context.Background()
	ledgerID := mustCreateLedger(t, service)
	const (
		identityID = ""
		accountRef = "acct-001"
		instrument = "AAPL"
		venue      = "NASDAQ"
		currency   = "USD"
	)

	// AAPL trades T+2 on the way.
	_, err := service.SetInstrumentSettings(ctx, model.InstrumentSettings{
		Instrument: instrument, Venue: venue, TradesOnTheWay: true, SettleOffset: 2,
	})
	require.NoError(t, err)

	// Counterparty/funding balances.
	market, err := service.CreateBalance(ctx, model.Balance{LedgerID: ledgerID, Currency: currency})
	require.NoError(t, err)
	settlement, err := service.CreateBalance(ctx, model.Balance{LedgerID: ledgerID, Currency: currency})
	require.NoError(t, err)

	// Fund the client money balance so the buy money legs (which debit it) clear.
	moneyPos, err := service.GetOrCreatePosition(ctx, model.PositionKey{
		LedgerID: ledgerID, IdentityID: identityID, AccountRef: accountRef, Currency: currency,
	})
	require.NoError(t, err)
	_, err = service.RecordTransaction(ctx, &model.Transaction{
		TransactionID: model.GenerateUUIDWithSuffix("txn"),
		Source:        market.BalanceID, Destination: moneyPos.BalanceID,
		Amount: 100000000, AmountString: "100000000", Precision: 100, Currency: currency,
		Reference: "fund-money", AllowOverdraft: true, SkipQueue: true,
	})
	require.NoError(t, err)

	// Seed 100 settled AAPL: book a buy of 100 and settle it now.
	seedRef := "seed-100"
	seed, err := service.BookTrade(ctx, model.TradeBooking{
		LedgerID: ledgerID, IdentityID: identityID, AccountRef: accountRef,
		Instrument: instrument, Venue: venue, Currency: currency,
		Quantity: 100, QuantityPrecision: 1, Price: "150.00", MoneyPrecision: 100,
		SettleOffset: 2, SettlementBalanceID: settlement.BalanceID, MarketBalanceID: market.BalanceID,
		Reference: seedRef, TradeDate: time.Now().AddDate(0, 0, -5),
	})
	require.NoError(t, err)
	_, err = service.SettleTrade(ctx, seed.SecurityTxnID)
	require.NoError(t, err)

	spot, err := service.GetActivePosition(ctx, ledgerID, identityID, accountRef, instrument, currency, nil)
	require.NoError(t, err)
	assert.Equal(t, int64(100), spot.Balance.Int64(), "should start with 100 settled AAPL")

	// Buy 50 more AAPL, T+2 (today).
	buy, err := service.BookTrade(ctx, model.TradeBooking{
		LedgerID: ledgerID, IdentityID: identityID, AccountRef: accountRef,
		Instrument: instrument, Venue: venue, Currency: currency,
		Quantity: 50, QuantityPrecision: 1, Price: "180.00", MoneyPrecision: 100,
		SettleOffset: 2, SettlementBalanceID: settlement.BalanceID, MarketBalanceID: market.BalanceID,
		Reference: "buy-50",
	})
	require.NoError(t, err)

	// Tradable today (settles T+2) must be 150: 100 settled + 50 in transit.
	tradable, err := service.GetTradablePosition(ctx, ledgerID, identityID, accountRef, instrument, currency, buy.SettleDate)
	require.NoError(t, err)
	assert.True(t, tradable.OnTheWay)
	assert.Equal(t, int64(150), tradable.Tradable.Int64())

	// Sell 125 today, T+2. Accepted because 125 <= 150.
	sell, err := service.SellTrade(ctx, model.SellBooking{
		LedgerID: ledgerID, IdentityID: identityID, AccountRef: accountRef,
		Instrument: instrument, Venue: venue, Currency: currency,
		Quantity: 125, QuantityPrecision: 1, Price: "185.00", MoneyPrecision: 100,
		SettleOffset: 2, SettlementBalanceID: settlement.BalanceID, MarketBalanceID: market.BalanceID,
		Reference: "sell-125",
	})
	require.NoError(t, err)
	assert.Equal(t, int64(150), sell.Tradable.Tradable.Int64())

	// A further sell of even 26 must now be rejected (150 - 125 = 25 left).
	_, err = service.SellTrade(ctx, model.SellBooking{
		LedgerID: ledgerID, IdentityID: identityID, AccountRef: accountRef,
		Instrument: instrument, Venue: venue, Currency: currency,
		Quantity: 26, QuantityPrecision: 1, Price: "185.00", MoneyPrecision: 100,
		SettleOffset: 2, SettlementBalanceID: settlement.BalanceID, MarketBalanceID: market.BalanceID,
		Reference: "sell-26",
	})
	assert.Error(t, err, "only 25 should remain tradable after selling 125")

	// Settle everything maturing by the buy's settle date (T+2).
	_, err = service.RunSettlement(ctx, buy.SettleDate.AddDate(0, 0, 1), 100)
	require.NoError(t, err)

	// Final settled position: 100 + 50 - 125 = 25 AAPL.
	finalSpot, err := service.GetActivePosition(ctx, ledgerID, identityID, accountRef, instrument, currency, nil)
	require.NoError(t, err)
	assert.Equal(t, int64(25), finalSpot.Balance.Int64(), "final settled position should be 25 AAPL")

	// WA price blended the original 100 @ 150 with 50 @ 180 over 150 shares:
	// (100*150 + 50*180) / 150 = 24000 / 150 = 160.00; the sell at WA leaves it.
	assert.Equal(t, "160.00", finalSpot.WAPrice, "weighted-average price after settlement")
}

// TestBrokerageChain_MultiDaySeparateBuckets proves multi-day accounting: two
// T+2 buys placed on different trade dates form distinct future buckets keyed
// by their absolute settle dates, settle independently on their own dates, and
// an explicit settle date works when the offset N is not controlled.
func TestBrokerageChain_MultiDaySeparateBuckets(t *testing.T) {
	service := newIntegrationBlnk(t)
	ctx := context.Background()
	ledgerID := mustCreateLedger(t, service)
	const (
		accountRef = "acct-multi"
		instrument = "TSLA"
		venue      = "NASDAQ"
		currency   = "USD"
	)

	_, err := service.SetInstrumentSettings(ctx, model.InstrumentSettings{
		Instrument: instrument, Venue: venue, TradesOnTheWay: true, SettleOffset: 2,
	})
	require.NoError(t, err)

	market, err := service.CreateBalance(ctx, model.Balance{LedgerID: ledgerID, Currency: currency})
	require.NoError(t, err)
	settlement, err := service.CreateBalance(ctx, model.Balance{LedgerID: ledgerID, Currency: currency})
	require.NoError(t, err)
	moneyPos, err := service.GetOrCreatePosition(ctx, model.PositionKey{LedgerID: ledgerID, AccountRef: accountRef, Currency: currency})
	require.NoError(t, err)
	_, err = service.RecordTransaction(ctx, &model.Transaction{
		TransactionID: model.GenerateUUIDWithSuffix("txn"),
		Source:        market.BalanceID, Destination: moneyPos.BalanceID,
		Amount: 100000000, AmountString: "100000000", Precision: 100, Currency: currency,
		Reference: "fund-multi", AllowOverdraft: true, SkipQueue: true,
	})
	require.NoError(t, err)

	// Buy 10 traded Monday (T+2 -> Wednesday) and 20 traded Tuesday (T+2 -> Thursday).
	monday := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	tuesday := monday.AddDate(0, 0, 1)
	buyMon, err := service.BookTrade(ctx, model.TradeBooking{
		LedgerID: ledgerID, AccountRef: accountRef, Instrument: instrument, Venue: venue, Currency: currency,
		Quantity: 10, QuantityPrecision: 1, Price: "100.00", MoneyPrecision: 100,
		SettleOffset: 2, TradeDate: monday, SettlementBalanceID: settlement.BalanceID, MarketBalanceID: market.BalanceID,
		Reference: "buy-mon",
	})
	require.NoError(t, err)
	buyTue, err := service.BookTrade(ctx, model.TradeBooking{
		LedgerID: ledgerID, AccountRef: accountRef, Instrument: instrument, Venue: venue, Currency: currency,
		Quantity: 20, QuantityPrecision: 1, Price: "100.00", MoneyPrecision: 100,
		SettleOffset: 2, TradeDate: tuesday, SettlementBalanceID: settlement.BalanceID, MarketBalanceID: market.BalanceID,
		Reference: "buy-tue",
	})
	require.NoError(t, err)

	// Distinct settle dates => distinct buckets (Wed vs Thu).
	assert.NotEqual(t, buyMon.SettleDate, buyTue.SettleDate)
	assert.NotEqual(t, buyMon.PositionBalanceID, buyTue.PositionBalanceID, "different trade dates must form different buckets")

	// Settle only what matured by Wednesday: just the Monday buy (10), not the Tuesday buy.
	_, err = service.RunSettlement(ctx, buyMon.SettleDate, 100)
	require.NoError(t, err)
	spot, err := service.GetActivePosition(ctx, ledgerID, "", accountRef, instrument, currency, nil)
	require.NoError(t, err)
	assert.Equal(t, int64(10), spot.Balance.Int64(), "only the Wednesday bucket should have settled")

	// Settle through Thursday: the Tuesday buy (20) now settles too -> 30 total.
	_, err = service.RunSettlement(ctx, buyTue.SettleDate, 100)
	require.NoError(t, err)
	spot, err = service.GetActivePosition(ctx, ledgerID, "", accountRef, instrument, currency, nil)
	require.NoError(t, err)
	assert.Equal(t, int64(30), spot.Balance.Int64(), "both buckets settled")

	// Buy with an EXPLICIT settle date (offset N uncontrolled): bucket keyed by the date.
	explicit := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	buyExplicit, err := service.BookTrade(ctx, model.TradeBooking{
		LedgerID: ledgerID, AccountRef: accountRef, Instrument: instrument, Venue: venue, Currency: currency,
		Quantity: 5, QuantityPrecision: 1, Price: "100.00", MoneyPrecision: 100,
		SettleDate: explicit, SettlementBalanceID: settlement.BalanceID, MarketBalanceID: market.BalanceID,
		Reference: "buy-explicit",
	})
	require.NoError(t, err)
	assert.Equal(t, "2026-07-01", buyExplicit.SettleDate.Format(model.HolidayKeyFormat), "explicit settle date is honored")

	_, err = service.RunSettlement(ctx, explicit, 100)
	require.NoError(t, err)
	spot, err = service.GetActivePosition(ctx, ledgerID, "", accountRef, instrument, currency, nil)
	require.NoError(t, err)
	assert.Equal(t, int64(35), spot.Balance.Int64(), "explicit-date buy settled into spot")
}

// TestBrokerageChain_ExplicitSettleDateIncoming verifies the item-1 fix: a buy
// booked with an explicit settle_date (settle_code NULL) is counted as
// in-transit incoming for a sale, so it is not silently excluded.
func TestBrokerageChain_ExplicitSettleDateIncoming(t *testing.T) {
	service := newIntegrationBlnk(t)
	ctx := context.Background()
	ledgerID := mustCreateLedger(t, service)
	const (
		accountRef = "acct-explicit"
		instrument = "NVDA"
		venue      = "NASDAQ"
		currency   = "USD"
	)
	_, err := service.SetInstrumentSettings(ctx, model.InstrumentSettings{
		Instrument: instrument, Venue: venue, TradesOnTheWay: true, SettleOffset: 2,
	})
	require.NoError(t, err)

	market, err := service.CreateBalance(ctx, model.Balance{LedgerID: ledgerID, Currency: currency})
	require.NoError(t, err)
	settlement, err := service.CreateBalance(ctx, model.Balance{LedgerID: ledgerID, Currency: currency})
	require.NoError(t, err)
	moneyPos, err := service.GetOrCreatePosition(ctx, model.PositionKey{LedgerID: ledgerID, AccountRef: accountRef, Currency: currency})
	require.NoError(t, err)
	_, err = service.RecordTransaction(ctx, &model.Transaction{
		TransactionID: model.GenerateUUIDWithSuffix("txn"),
		Source:        market.BalanceID, Destination: moneyPos.BalanceID,
		Amount: 100000000, AmountString: "100000000", Precision: 100, Currency: currency,
		Reference: "fund-explicit", AllowOverdraft: true, SkipQueue: true,
	})
	require.NoError(t, err)

	// Buy 10 with an EXPLICIT settle date (offset N uncontrolled -> settle_code NULL).
	explicit := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	_, err = service.BookTrade(ctx, model.TradeBooking{
		LedgerID: ledgerID, AccountRef: accountRef, Instrument: instrument, Venue: venue, Currency: currency,
		Quantity: 10, QuantityPrecision: 1, Price: "500.00", MoneyPrecision: 100,
		SettleDate: explicit, SettlementBalanceID: settlement.BalanceID, MarketBalanceID: market.BalanceID,
		Reference: "buy-explicit-10",
	})
	require.NoError(t, err)

	// Despite settle_code = NULL, the in-transit 10 must be counted as incoming.
	tradable, err := service.GetTradablePosition(ctx, ledgerID, "", accountRef, instrument, currency, explicit)
	require.NoError(t, err)
	assert.True(t, tradable.OnTheWay)
	assert.Equal(t, int64(10), tradable.Incoming.Int64(), "explicit-settle_date buy must count as incoming")
	assert.Equal(t, int64(10), tradable.Tradable.Int64())
}

// TestBrokerageChain_IdempotentResettlement verifies the item-2 fix: a closed
// (zeroed) future bucket is not reprocessed by a later settlement run, and the
// settlement journal has no leftover pending rows.
func TestBrokerageChain_IdempotentResettlement(t *testing.T) {
	service, ds := newIntegrationBlnkDS(t)
	ctx := context.Background()
	ledgerID := mustCreateLedger(t, service)
	const (
		accountRef = "acct-idem"
		instrument = "AMD"
		venue      = "NASDAQ"
		currency   = "USD"
	)
	_, err := service.SetInstrumentSettings(ctx, model.InstrumentSettings{
		Instrument: instrument, Venue: venue, TradesOnTheWay: true, SettleOffset: 2,
	})
	require.NoError(t, err)
	market, err := service.CreateBalance(ctx, model.Balance{LedgerID: ledgerID, Currency: currency})
	require.NoError(t, err)
	settlement, err := service.CreateBalance(ctx, model.Balance{LedgerID: ledgerID, Currency: currency})
	require.NoError(t, err)
	moneyPos, err := service.GetOrCreatePosition(ctx, model.PositionKey{LedgerID: ledgerID, AccountRef: accountRef, Currency: currency})
	require.NoError(t, err)
	_, err = service.RecordTransaction(ctx, &model.Transaction{
		TransactionID: model.GenerateUUIDWithSuffix("txn"),
		Source:        market.BalanceID, Destination: moneyPos.BalanceID,
		Amount: 100000000, AmountString: "100000000", Precision: 100, Currency: currency,
		Reference: "fund-idem", AllowOverdraft: true, SkipQueue: true,
	})
	require.NoError(t, err)

	buy, err := service.BookTrade(ctx, model.TradeBooking{
		LedgerID: ledgerID, AccountRef: accountRef, Instrument: instrument, Venue: venue, Currency: currency,
		Quantity: 40, QuantityPrecision: 1, Price: "100.00", MoneyPrecision: 100,
		SettleOffset: 2, SettlementBalanceID: settlement.BalanceID, MarketBalanceID: market.BalanceID,
		Reference: "buy-idem",
	})
	require.NoError(t, err)

	first, err := service.RunSettlement(ctx, buy.SettleDate, 100)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, first.Examined, 1)

	spot, err := service.GetActivePosition(ctx, ledgerID, "", accountRef, instrument, currency, nil)
	require.NoError(t, err)
	assert.Equal(t, int64(40), spot.Balance.Int64())

	// Re-run: the now-zeroed bucket must be skipped (idempotent), nothing settled.
	second, err := service.RunSettlement(ctx, buy.SettleDate, 100)
	require.NoError(t, err)
	assert.Equal(t, 0, second.Examined, "closed bucket must not be reprocessed")
	assert.Empty(t, second.Settled)

	// No settlement journal rows should remain pending after a clean run.
	pending, err := ds.ListPendingSettlementJournals(ctx, 100)
	require.NoError(t, err)
	assert.Empty(t, pending, "no pending settlement journals after a completed run")
}

// TestBrokerageChain_SettlementRecovery verifies the item-3 fix: a settlement
// whose side effects (wa_price + lot) did not finish (a 'pending' journal row)
// is completed deterministically and idempotently by ReconcileSettlement.
func TestBrokerageChain_SettlementRecovery(t *testing.T) {
	service, ds := newIntegrationBlnkDS(t)
	ctx := context.Background()
	ledgerID := mustCreateLedger(t, service)
	const (
		accountRef = "acct-recover"
		instrument = "INTC"
		currency   = "USD"
	)

	// A spot position to receive the WA + lot.
	spot, err := service.GetOrCreatePosition(ctx, model.PositionKey{
		LedgerID: ledgerID, AccountRef: accountRef, Instrument: instrument, Currency: currency,
	})
	require.NoError(t, err)

	// Simulate a crashed settlement: a 'pending' journal row exists, but its
	// side effects were never applied. wa_before 150 @ 100, buy 50 @ 180.
	_, created, err := ds.BeginSettlementJournal(ctx, model.SettlementJournalEntry{
		SecurityTxnID: "txn_crashed_sec",
		TradeRef:      "buy-crashed",
		SpotBalanceID: spot.BalanceID,
		Instrument:    instrument,
		Side:          model.TradeSideBuy,
		WABefore:      "150.00",
		QtyBefore:     "100",
		Price:         "180",
		Quantity:      "50",
		Precision:     1,
		Currency:      currency,
	})
	require.NoError(t, err)
	assert.True(t, created)

	// Recovery completes the side effects: wa_after = 160.00, one lot.
	n, err := service.ReconcileSettlement(ctx, 100)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	applied, err := ds.GetSettlementJournal(ctx, "txn_crashed_sec")
	require.NoError(t, err)
	assert.Equal(t, model.SettlementApplied, applied.Status)

	spot, err = service.GetOrCreatePosition(ctx, model.PositionKey{
		LedgerID: ledgerID, AccountRef: accountRef, Instrument: instrument, Currency: currency,
	})
	require.NoError(t, err)
	assert.Equal(t, "160.00", spot.WAPrice, "wa recomputed deterministically on recovery")

	lots, err := service.GetBalanceLots(ctx, spot.BalanceID)
	require.NoError(t, err)
	assert.Len(t, lots, 1)

	// Idempotent: a second recovery does nothing new (no pending, no duplicate lot).
	n2, err := service.ReconcileSettlement(ctx, 100)
	require.NoError(t, err)
	assert.Equal(t, 0, n2)
	lots, err = service.GetBalanceLots(ctx, spot.BalanceID)
	require.NoError(t, err)
	assert.Len(t, lots, 1, "recovery must not duplicate the lot")
}
