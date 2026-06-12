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
	return service
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
	}, nil)
	require.NoError(t, err)
	_, err = service.RecordTransaction(ctx, &model.Transaction{
		Source: market.BalanceID, Destination: moneyPos.BalanceID,
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
