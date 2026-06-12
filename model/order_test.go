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
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestOrderTransitions(t *testing.T) {
	// Valid forward transitions.
	assert.True(t, CanTransitionOrder(OrderStatusDraft, OrderStatusChecked))
	assert.True(t, CanTransitionOrder(OrderStatusChecked, OrderStatusApproved))
	assert.True(t, CanTransitionOrder(OrderStatusApproved, OrderStatusExecuted))
	// Reject/cancel allowed from any non-terminal state.
	assert.True(t, CanTransitionOrder(OrderStatusDraft, OrderStatusRejected))
	assert.True(t, CanTransitionOrder(OrderStatusChecked, OrderStatusCancelled))

	// Invalid skips / backward.
	assert.False(t, CanTransitionOrder(OrderStatusDraft, OrderStatusApproved))
	assert.False(t, CanTransitionOrder(OrderStatusDraft, OrderStatusExecuted))
	assert.False(t, CanTransitionOrder(OrderStatusChecked, OrderStatusExecuted))
	assert.False(t, CanTransitionOrder(OrderStatusApproved, OrderStatusChecked))

	// Terminal states admit nothing.
	assert.True(t, IsTerminalOrderStatus(OrderStatusExecuted))
	assert.True(t, IsTerminalOrderStatus(OrderStatusRejected))
	assert.True(t, IsTerminalOrderStatus(OrderStatusCancelled))
	assert.False(t, IsTerminalOrderStatus(OrderStatusDraft))
	assert.False(t, CanTransitionOrder(OrderStatusExecuted, OrderStatusCancelled))
}

func TestOrderValidate(t *testing.T) {
	valid := Order{
		Reference: "ord-1", LedgerID: "ldg", AccountRef: "acc", Currency: "USD",
		Instrument: "AAPL", Side: TradeSideBuy,
		Quantity: "10", QuantityPrecision: 1, Price: "150.00", MoneyPrecision: 100,
		SettlementBalanceID: "bln_s", MarketBalanceID: "bln_m",
	}
	assert.NoError(t, valid.Validate())

	bad := valid
	bad.Side = "hold"
	assert.Error(t, bad.Validate())

	bad = valid
	bad.Quantity = "0"
	assert.Error(t, bad.Validate())

	bad = valid
	bad.Reference = ""
	assert.Error(t, bad.Validate())

	bad = valid
	bad.SettlementBalanceID = ""
	assert.Error(t, bad.Validate())

	bad = valid
	bad.Price = "abc"
	assert.Error(t, bad.Validate())
}

func TestTradingTimeIsOpenAt(t *testing.T) {
	w := TradingTime{OpenTime: "09:30", CloseTime: "16:00"}
	assert.True(t, w.IsOpenAt("09:30"))
	assert.True(t, w.IsOpenAt("12:00"))
	assert.True(t, w.IsOpenAt("16:00"))
	assert.False(t, w.IsOpenAt("09:29"))
	assert.False(t, w.IsOpenAt("16:01"))
}
