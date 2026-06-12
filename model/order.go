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

// Order domain: the TradeControl decision/lifecycle entity in front of trade
// booking. An order is created as a draft, validated (CheckOrder), approved and
// executed into a brokerage trade (BookTrade/SellTrade). Statuses are
// data-driven via the dict table (TradeControl Dict pattern) but the canonical
// lifecycle is enforced in code by the transition table below.
package model

import (
	"fmt"
	"time"
)

// Order lifecycle statuses (dict category "order_status").
const (
	OrderStatusDraft     = "DRAFT"
	OrderStatusChecked   = "CHECKED"
	OrderStatusApproved  = "APPROVED"
	OrderStatusExecuted  = "EXECUTED"
	OrderStatusRejected  = "REJECTED"
	OrderStatusCancelled = "CANCELLED"
)

// AML statuses.
const (
	AMLStatusPassed  = "PASSED"
	AMLStatusBlocked = "BLOCKED"
)

// orderTransitions is the allowed lifecycle: from -> set of reachable statuses.
// REJECTED/CANCELLED/EXECUTED are terminal.
var orderTransitions = map[string]map[string]bool{
	OrderStatusDraft:    {OrderStatusChecked: true, OrderStatusRejected: true, OrderStatusCancelled: true},
	OrderStatusChecked:  {OrderStatusApproved: true, OrderStatusRejected: true, OrderStatusCancelled: true},
	OrderStatusApproved: {OrderStatusExecuted: true, OrderStatusRejected: true, OrderStatusCancelled: true},
}

// CanTransitionOrder reports whether an order may move from -> to.
func CanTransitionOrder(from, to string) bool {
	return orderTransitions[from][to]
}

// IsTerminalOrderStatus reports whether the status admits no further transitions.
func IsTerminalOrderStatus(status string) bool {
	_, ongoing := orderTransitions[status]
	return !ongoing
}

// Order is a buy/sell order against a brokerage account.
type Order struct {
	OrderID    string `json:"order_id"`
	Reference  string `json:"reference"`
	LedgerID   string `json:"ledger_id"`
	IdentityID string `json:"identity_id,omitempty"`
	AccountRef string `json:"account_ref"`

	Instrument string `json:"instrument"`
	Venue      string `json:"venue,omitempty"`
	Currency   string `json:"currency"`
	Side       string `json:"side"` // buy | sell

	Quantity          string `json:"quantity"`
	QuantityPrecision int    `json:"quantity_precision"`
	Price             string `json:"price"`
	MoneyPrecision    int    `json:"money_precision"`

	SettleOffset int        `json:"settle_offset"`
	SettleDate   *time.Time `json:"settle_date,omitempty"`

	Status        string `json:"status"`
	StatusMessage string `json:"status_message,omitempty"`
	RejectReason  string `json:"reject_reason,omitempty"`
	AMLStatus     string `json:"aml_status,omitempty"`

	SettlementBalanceID string `json:"settlement_balance_id"`
	MarketBalanceID     string `json:"market_balance_id"`

	// Filled once the order is executed into a trade.
	TradeRef      string `json:"trade_ref,omitempty"`
	SecurityTxnID string `json:"security_txn_id,omitempty"`
	MoneyTxnID    string `json:"money_txn_id,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Validate checks order creation invariants.
func (o Order) Validate() error {
	if o.Reference == "" {
		return fmt.Errorf("order: reference is required")
	}
	if o.LedgerID == "" || o.AccountRef == "" || o.Currency == "" {
		return fmt.Errorf("order: ledger_id, account_ref and currency are required")
	}
	if o.Instrument == "" {
		return fmt.Errorf("order: instrument is required")
	}
	if o.Side != TradeSideBuy && o.Side != TradeSideSell {
		return fmt.Errorf("order: side must be buy or sell")
	}
	if o.QuantityPrecision < 1 || o.MoneyPrecision < 1 {
		return fmt.Errorf("order: quantity_precision and money_precision must be >= 1")
	}
	if _, err := PreciseQuantity(o.Quantity, o.QuantityPrecision); err != nil {
		return fmt.Errorf("order: %w", err)
	}
	if _, err := PreciseMoney(o.Quantity, o.Price, o.MoneyPrecision); err != nil {
		return fmt.Errorf("order: %w", err)
	}
	if o.SettlementBalanceID == "" || o.MarketBalanceID == "" {
		return fmt.Errorf("order: settlement_balance_id and market_balance_id are required")
	}
	return nil
}

// DictEntry is one configurable reference value (TradeControl Dict).
type DictEntry struct {
	Category  string    `json:"category"`
	Code      string    `json:"code"`
	Name      string    `json:"name"`
	Sort      int       `json:"sort"`
	Active    bool      `json:"active"`
	CreatedAt time.Time `json:"created_at"`
}

// StopListEntry blocks an identity from trading, optionally for one instrument
// (Instrument == "" blocks all instruments).
type StopListEntry struct {
	ID         int64     `json:"id"`
	IdentityID string    `json:"identity_id"`
	Instrument string    `json:"instrument"`
	Reason     string    `json:"reason,omitempty"`
	Active     bool      `json:"active"`
	CreatedAt  time.Time `json:"created_at"`
}

// TradingTime is a venue trading window for a weekday (0=Sunday..6=Saturday),
// open/close as "HH:MM".
type TradingTime struct {
	ID        int64     `json:"id"`
	Venue     string    `json:"venue"`
	Weekday   int       `json:"weekday"`
	OpenTime  string    `json:"open_time"`
	CloseTime string    `json:"close_time"`
	CreatedAt time.Time `json:"created_at"`
}

// IsOpenAt reports whether the window contains the given clock time "HH:MM".
func (w TradingTime) IsOpenAt(hhmm string) bool {
	return hhmm >= w.OpenTime && hhmm <= w.CloseTime
}

// OrderCheckResult is the outcome of validating an order.
type OrderCheckResult struct {
	Passed       bool     `json:"passed"`
	RejectReason string   `json:"reject_reason,omitempty"`
	Checks       []string `json:"checks"` // names of checks that ran
}
