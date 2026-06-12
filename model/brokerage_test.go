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
package model

import (
	"math/big"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
)

func date(value string) time.Time {
	parsed, err := time.Parse(HolidayKeyFormat, value)
	if err != nil {
		panic(err)
	}
	return parsed
}

func TestComputeSettleDate(t *testing.T) {
	noHolidays := map[string]struct{}{}

	tests := []struct {
		name      string
		tradeDate string
		offset    int
		holidays  map[string]struct{}
		want      string
		wantErr   bool
	}{
		{
			name: "T+2 across a weekend", tradeDate: "2026-06-12", offset: 2, // Friday
			holidays: noHolidays, want: "2026-06-16", // Mon + Tue
		},
		{
			name: "T+2 with a holiday on Monday", tradeDate: "2026-06-12", offset: 2,
			holidays: map[string]struct{}{"2026-06-15": {}}, want: "2026-06-17", // Tue + Wed
		},
		{
			name: "T+0 on a Saturday rolls to Monday", tradeDate: "2026-06-13", offset: 0,
			holidays: noHolidays, want: "2026-06-15",
		},
		{
			name: "T+0 on a business day stays", tradeDate: "2026-06-10", offset: 0, // Wednesday
			holidays: noHolidays, want: "2026-06-10",
		},
		{
			name: "T+1 mid-week", tradeDate: "2026-06-10", offset: 1,
			holidays: noHolidays, want: "2026-06-11",
		},
		{
			name: "negative offset rejected", tradeDate: "2026-06-10", offset: -1,
			holidays: noHolidays, wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ComputeSettleDate(date(tt.tradeDate), tt.offset, tt.holidays)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tt.want, got.Format(HolidayKeyFormat))
		})
	}
}

func TestIsSettlementDay(t *testing.T) {
	holidays := map[string]struct{}{"2026-06-15": {}}
	assert.False(t, IsSettlementDay(date("2026-06-13"), holidays)) // Saturday
	assert.False(t, IsSettlementDay(date("2026-06-14"), holidays)) // Sunday
	assert.False(t, IsSettlementDay(date("2026-06-15"), holidays)) // holiday Monday
	assert.True(t, IsSettlementDay(date("2026-06-16"), holidays))  // Tuesday
}

func TestRecalculateWAPrice(t *testing.T) {
	dec := func(s string) decimal.Decimal {
		d, err := decimal.NewFromString(s)
		assert.NoError(t, err)
		return d
	}

	t.Run("fresh position equals trade price", func(t *testing.T) {
		// 100 qty for 1900000: WA = 19000.00
		wa, err := RecalculateWAPrice("", decimal.Zero, dec("100"), dec("1900000"))
		assert.NoError(t, err)
		assert.Equal(t, "19000.00", wa.StringFixed(2))
	})

	t.Run("blends with the existing position", func(t *testing.T) {
		// TradeControl formula: (tradeMoney + WA_old*qty_old) / (qty_trade + qty_old)
		// (3000 + 10.00*100) / (200 + 100) = 4000/300 = 13.333... -> 13.33
		wa, err := RecalculateWAPrice("10.00", dec("100"), dec("200"), dec("3000"))
		assert.NoError(t, err)
		assert.Equal(t, "13.33", wa.StringFixed(2))
	})

	t.Run("HALF_EVEN rounds half to even", func(t *testing.T) {
		// 2125 / 1000 = 2.125 -> banker's rounding at scale 2 -> 2.12
		wa, err := RecalculateWAPrice("", decimal.Zero, dec("1000"), dec("2125"))
		assert.NoError(t, err)
		assert.Equal(t, "2.12", wa.StringFixed(2))

		// 2135 / 1000 = 2.135 -> 2.14 (4 is even)
		wa, err = RecalculateWAPrice("", decimal.Zero, dec("1000"), dec("2135"))
		assert.NoError(t, err)
		assert.Equal(t, "2.14", wa.StringFixed(2))
	})

	t.Run("rejects non-positive trade quantity", func(t *testing.T) {
		_, err := RecalculateWAPrice("", decimal.Zero, decimal.Zero, dec("100"))
		assert.Error(t, err)
	})

	t.Run("rejects invalid stored wa price", func(t *testing.T) {
		_, err := RecalculateWAPrice("not-a-number", decimal.Zero, dec("1"), dec("100"))
		assert.Error(t, err)
	})
}

func TestFreeBalance(t *testing.T) {
	tests := []struct {
		name             string
		sum0, sum1, sum2 *big.Int
		want             int64
	}{
		{"obligations exceed available -> 0", nil, big.NewInt(100), big.NewInt(150), 0},
		{"normal difference", nil, big.NewInt(1000), big.NewInt(300), 700},
		{"cap reached -> cap", big.NewInt(500), big.NewInt(1000), big.NewInt(300), 500},
		{"cap above difference -> difference", big.NewInt(900), big.NewInt(1000), big.NewInt(300), 700},
		{"cap equal to difference -> cap", big.NewInt(700), big.NewInt(1000), big.NewInt(300), 700},
		{"nil sums treated as zero", nil, nil, nil, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FreeBalance(tt.sum0, tt.sum1, tt.sum2)
			assert.Equal(t, tt.want, got.Int64())
		})
	}
}

func TestPositionKeyLockKey(t *testing.T) {
	two := 2
	spot := PositionKey{LedgerID: "ldg", IdentityID: "idn", AccountRef: "acc", Instrument: "KZAP", Currency: "KZT"}
	future := PositionKey{LedgerID: "ldg", IdentityID: "idn", AccountRef: "acc", Instrument: "KZAP", Currency: "KZT", SettleCode: &two}

	assert.Equal(t, "brokerage|ldg|idn|acc|KZAP|KZT|-1", spot.LockKey())
	assert.Equal(t, "brokerage|ldg|idn|acc|KZAP|KZT|2", future.LockKey())
	assert.NotEqual(t, spot.LockKey(), future.LockKey())
}

func TestPositionKeyValidate(t *testing.T) {
	valid := PositionKey{LedgerID: "ldg", AccountRef: "acc", Currency: "KZT"}
	assert.NoError(t, valid.Validate())

	assert.Error(t, PositionKey{AccountRef: "acc", Currency: "KZT"}.Validate())
	assert.Error(t, PositionKey{LedgerID: "ldg", Currency: "KZT"}.Validate())
	assert.Error(t, PositionKey{LedgerID: "ldg", AccountRef: "acc"}.Validate())

	negative := -1
	invalid := PositionKey{LedgerID: "ldg", AccountRef: "acc", Currency: "KZT", SettleCode: &negative}
	assert.Error(t, invalid.Validate())
}

func TestMutationPlanNormalized(t *testing.T) {
	key := PositionKey{LedgerID: "ldg", AccountRef: "acc", Currency: "KZT"}
	otherKey := PositionKey{LedgerID: "ldg", AccountRef: "acc", Currency: "USD"}

	t.Run("merges deltas sharing a key", func(t *testing.T) {
		plan := MutationPlan{Deltas: []BalanceDelta{
			{Key: key, AmountDelta: big.NewInt(100), BlockedDelta: big.NewInt(50)},
			{Key: key, AmountDelta: big.NewInt(-30), WaitingDelta: big.NewInt(20)},
		}}
		normalized := plan.Normalized()
		assert.Len(t, normalized, 1)
		assert.Equal(t, int64(70), normalized[0].AmountDelta.Int64())
		assert.Equal(t, int64(50), normalized[0].BlockedDelta.Int64())
		assert.Equal(t, int64(20), normalized[0].WaitingDelta.Int64())
	})

	t.Run("drops no-op deltas", func(t *testing.T) {
		plan := MutationPlan{Deltas: []BalanceDelta{
			{Key: key, AmountDelta: big.NewInt(100)},
			{Key: key, AmountDelta: big.NewInt(-100)},
			{Key: otherKey, AmountDelta: big.NewInt(5)},
		}}
		normalized := plan.Normalized()
		assert.Len(t, normalized, 1)
		assert.Equal(t, otherKey.LockKey(), normalized[0].Key.LockKey())
	})

	t.Run("orders deterministically by lock key", func(t *testing.T) {
		plan := MutationPlan{Deltas: []BalanceDelta{
			{Key: otherKey, AmountDelta: big.NewInt(1)},
			{Key: key, AmountDelta: big.NewInt(1)},
		}}
		first := plan.Normalized()
		second := MutationPlan{Deltas: []BalanceDelta{
			{Key: key, AmountDelta: big.NewInt(1)},
			{Key: otherKey, AmountDelta: big.NewInt(1)},
		}}.Normalized()

		assert.Equal(t, first[0].Key.LockKey(), second[0].Key.LockKey())
		assert.Equal(t, first[1].Key.LockKey(), second[1].Key.LockKey())
		assert.True(t, first[0].Key.LockKey() < first[1].Key.LockKey())
	})

	t.Run("empty plan fails validation", func(t *testing.T) {
		assert.Error(t, MutationPlan{}.Validate())
	})

	t.Run("nil components merge safely", func(t *testing.T) {
		plan := MutationPlan{Deltas: []BalanceDelta{
			{Key: key},
			{Key: key, BlockedDelta: big.NewInt(10)},
		}}
		normalized := plan.Normalized()
		assert.Len(t, normalized, 1)
		assert.Equal(t, int64(0), normalized[0].AmountDelta.Int64())
		assert.Equal(t, int64(10), normalized[0].BlockedDelta.Int64())
	})
}

func TestBalanceDeltaIsNoOp(t *testing.T) {
	key := PositionKey{LedgerID: "ldg", AccountRef: "acc", Currency: "KZT"}
	assert.True(t, BalanceDelta{Key: key}.IsNoOp())
	assert.True(t, BalanceDelta{Key: key, AmountDelta: big.NewInt(0)}.IsNoOp())
	assert.False(t, BalanceDelta{Key: key, WaitingDelta: big.NewInt(1)}.IsNoOp())
}

func TestTradeBookingValidate(t *testing.T) {
	valid := TradeBooking{
		LedgerID: "ldg", IdentityID: "idn", AccountRef: "acc",
		Instrument: "KZAP", Venue: "KASE", Currency: "KZT",
		Quantity: 100, QuantityPrecision: 1, Price: "19000.00", MoneyPrecision: 100,
		SettleOffset: 2, SettlementBalanceID: "bln_settle", MarketBalanceID: "bln_market",
		Reference: "trade_001",
	}
	assert.NoError(t, valid.Validate())

	invalid := valid
	invalid.Quantity = 0
	assert.Error(t, invalid.Validate())

	invalid = valid
	invalid.Price = "abc"
	assert.Error(t, invalid.Validate())

	invalid = valid
	invalid.Instrument = ""
	assert.Error(t, invalid.Validate())

	invalid = valid
	invalid.SettleOffset = -1
	assert.Error(t, invalid.Validate())

	invalid = valid
	invalid.Reference = ""
	assert.Error(t, invalid.Validate())
}
