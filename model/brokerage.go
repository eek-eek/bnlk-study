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

// Brokerage domain model: position keys, mutation plans, purchase lots,
// venue holidays, settle-date (T+N) arithmetic, weighted-average price and
// free-balance thresholds. This is the pure-logic counterpart of the
// TradeControl balance subsystem (BalanceKey/BalanceDelta/BalanceMutationPlan,
// HolidayService, recalculateWawPrice and FreeBalanceAccountService).
package model

import (
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// SpotSettleDateSentinel is the date used in COALESCE expressions to represent
// the spot (settle_date IS NULL) balance, so spot and future buckets share one
// comparison domain.
const SpotSettleDateSentinel = "1970-01-01"

// PositionKey uniquely identifies a brokerage-managed balance
// (TradeControl BalanceKey analog). Instrument == "" denotes a money balance.
//
// The settlement dimension of the identity is the ABSOLUTE settle date, not the
// T+N offset: SettleDate == nil denotes the spot balance, a concrete date
// denotes the future bucket settling that day. This makes multi-day accounting
// correct — trades with the same offset on different trade dates land in
// distinct buckets — and supports cases where the offset N is not controlled
// (only the settlement date is known). SettleCode is an optional informational
// attribute (the offset, when known); it is not part of the identity.
type PositionKey struct {
	LedgerID   string     `json:"ledger_id"`
	IdentityID string     `json:"identity_id"`
	AccountRef string     `json:"account_ref"`
	Instrument string     `json:"instrument,omitempty"`
	Currency   string     `json:"currency"`
	SettleDate *time.Time `json:"settle_date,omitempty"`
	SettleCode *int       `json:"settle_code,omitempty"`
}

// settleDateKey normalizes the nullable settle date for lock keys and queries.
func (k PositionKey) settleDateKey() string {
	if k.SettleDate == nil {
		return SpotSettleDateSentinel
	}
	return k.SettleDate.Format(HolidayKeyFormat)
}

// IsSpot reports whether the key addresses the settled (spot) balance.
func (k PositionKey) IsSpot() bool {
	return k.SettleDate == nil
}

// LockKey returns the deterministic pipe-joined lock key for this position
// (TradeControl BalanceKey.toLockKey analog).
func (k PositionKey) LockKey() string {
	return strings.Join([]string{
		"brokerage", k.LedgerID, k.IdentityID, k.AccountRef,
		k.Instrument, k.Currency, k.settleDateKey(),
	}, "|")
}

// Validate checks that the mandatory key dimensions are present.
func (k PositionKey) Validate() error {
	if k.LedgerID == "" {
		return fmt.Errorf("position key: ledger_id is required")
	}
	if k.AccountRef == "" {
		return fmt.Errorf("position key: account_ref is required")
	}
	if k.Currency == "" {
		return fmt.Errorf("position key: currency is required")
	}
	return nil
}

// BalanceDelta is a signed change applied to a single position
// (TradeControl BalanceDelta analog).
//
//   - AmountDelta mutates the settled balance: positive values are applied as
//     credits, negative values as debits, preserving the blnk invariant
//     balance = credit_balance - debit_balance.
//   - BlockedDelta mutates inflight_debit_balance (TradeControl blockedAmount).
//   - WaitingDelta mutates inflight_credit_balance (TradeControl waitingAmount).
type BalanceDelta struct {
	Key          PositionKey `json:"key"`
	AmountDelta  *big.Int    `json:"amount_delta,omitempty"`
	BlockedDelta *big.Int    `json:"blocked_delta,omitempty"`
	WaitingDelta *big.Int    `json:"waiting_delta,omitempty"`
}

// normalizedFields returns the delta with nil components replaced by zeros.
func (d BalanceDelta) normalizedFields() BalanceDelta {
	out := d
	if out.AmountDelta == nil {
		out.AmountDelta = big.NewInt(0)
	}
	if out.BlockedDelta == nil {
		out.BlockedDelta = big.NewInt(0)
	}
	if out.WaitingDelta == nil {
		out.WaitingDelta = big.NewInt(0)
	}
	return out
}

// IsNoOp reports whether the delta changes nothing.
func (d BalanceDelta) IsNoOp() bool {
	n := d.normalizedFields()
	return n.AmountDelta.Sign() == 0 && n.BlockedDelta.Sign() == 0 && n.WaitingDelta.Sign() == 0
}

// Merge adds another delta for the same key into this one.
func (d *BalanceDelta) Merge(other BalanceDelta) {
	n := d.normalizedFields()
	o := other.normalizedFields()
	d.AmountDelta = new(big.Int).Add(n.AmountDelta, o.AmountDelta)
	d.BlockedDelta = new(big.Int).Add(n.BlockedDelta, o.BlockedDelta)
	d.WaitingDelta = new(big.Int).Add(n.WaitingDelta, o.WaitingDelta)
}

// MutationPlan is an atomic set of balance deltas
// (TradeControl BalanceMutationPlan analog).
type MutationPlan struct {
	Deltas []BalanceDelta `json:"deltas"`
}

// Normalized merges deltas that share a key, drops no-ops and returns the
// result ordered by lock key. The deterministic ordering is the deadlock
// guard: every writer locks and updates rows in the same global order.
func (p MutationPlan) Normalized() []BalanceDelta {
	merged := make(map[string]*BalanceDelta)
	for _, delta := range p.Deltas {
		key := delta.Key.LockKey()
		if existing, ok := merged[key]; ok {
			existing.Merge(delta)
			continue
		}
		normalized := delta.normalizedFields()
		merged[key] = &normalized
	}

	out := make([]BalanceDelta, 0, len(merged))
	for _, delta := range merged {
		if delta.IsNoOp() {
			continue
		}
		out = append(out, *delta)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Key.LockKey() < out[j].Key.LockKey()
	})
	return out
}

// LockKeys returns the sorted set of lock keys touched by the plan.
func (p MutationPlan) LockKeys() []string {
	normalized := p.Normalized()
	keys := make([]string, 0, len(normalized))
	for _, delta := range normalized {
		keys = append(keys, delta.Key.LockKey())
	}
	return keys
}

// Validate checks every delta key in the plan.
func (p MutationPlan) Validate() error {
	if len(p.Deltas) == 0 {
		return fmt.Errorf("mutation plan: at least one delta is required")
	}
	for i, delta := range p.Deltas {
		if err := delta.Key.Validate(); err != nil {
			return fmt.Errorf("mutation plan delta %d: %w", i, err)
		}
	}
	return nil
}

// BalanceLot is a purchase lot attached to a securities balance
// (TradeControl BalanceDetail analog). Quantity is stored in precise minor
// units together with its precision factor; Price is the per-unit execution
// price as an exact decimal string.
type BalanceLot struct {
	ID          int64     `json:"id"`
	LotID       string    `json:"lot_id"`
	BalanceID   string    `json:"balance_id"`
	Instrument  string    `json:"instrument"`
	Quantity    *big.Int  `json:"quantity"`
	Precision   int64     `json:"precision"`
	Price       string    `json:"price"`
	Currency    string    `json:"currency"`
	Reference   string    `json:"reference,omitempty"`
	PurchasedAt time.Time `json:"purchased_at"`
	CreatedAt   time.Time `json:"created_at"`
}

// MarketHoliday is a non-settlement day for a trading venue.
type MarketHoliday struct {
	ID          int64     `json:"id"`
	Venue       string    `json:"venue"`
	HolidayDate time.Time `json:"holiday_date"`
	Description string    `json:"description,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// HolidayKeyFormat is the map key layout used by ComputeSettleDate.
const HolidayKeyFormat = "2006-01-02"

// InstrumentSettings carries the per-instrument trading mode. TradesOnTheWay
// marks an instrument that settles on a T+SettleOffset cycle and may be traded
// while quantity is still in transit (TradeControl board "trades on the way"
// flag). When it is false (or no settings exist) future incoming/outgoing is
// not counted as tradable: only the settled position can be sold.
type InstrumentSettings struct {
	ID              int64     `json:"id"`
	Instrument      string    `json:"instrument"`
	Venue           string    `json:"venue,omitempty"`
	TradesOnTheWay  bool      `json:"trades_on_the_way"`
	SettleOffset    int       `json:"settle_offset"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// TradablePosition is the settle-date-aware answer to "how much can be sold".
// For an on-the-way instrument it nets the settled position with the
// in-transit incoming/outgoing maturing on or before the trade settle date;
// for an immediate-settlement instrument it reports only the settled, unblocked
// position (TradeControl FreeBalance for securities).
type TradablePosition struct {
	Settled    *big.Int `json:"settled"`     // spot balance
	Blocked    *big.Int `json:"blocked"`     // spot inflight debit (already-committed outflows)
	Incoming   *big.Int `json:"incoming"`    // future inflight credit counted (0 if not on-the-way)
	Outgoing   *big.Int `json:"outgoing"`    // future inflight debit counted (0 if not on-the-way)
	Tradable   *big.Int `json:"tradable"`    // amount available to sell
	OnTheWay   bool     `json:"on_the_way"`  // whether in-transit quantity was considered
	AsOfSettle string   `json:"as_of_settle"`
}

// ComputeTradable applies the TradeControl settle-date arithmetic:
//
//	on-the-way:  tradable = settled - blocked + incoming - outgoing
//	immediate:   tradable = settled - blocked
//
// A negative result is clamped to zero.
func ComputeTradable(settled, blocked, incoming, outgoing *big.Int, onTheWay bool) *big.Int {
	tradable := new(big.Int).Sub(nz(settled), nz(blocked))
	if onTheWay {
		tradable.Add(tradable, nz(incoming))
		tradable.Sub(tradable, nz(outgoing))
	}
	if tradable.Sign() < 0 {
		return big.NewInt(0)
	}
	return tradable
}

// nz returns a zero big.Int for nil inputs.
func nz(v *big.Int) *big.Int {
	if v == nil {
		return big.NewInt(0)
	}
	return v
}

// Trade sides.
const (
	TradeSideBuy  = "buy"
	TradeSideSell = "sell"
)

// TradeMetaSide is the metadata key carrying the trade side.
const TradeMetaSide = "trade_side"

// IsSettlementDay reports whether the given date counts as a settlement day:
// not a weekend and not a venue holiday (TradeControl HolidayService rule:
// weekends and venue holidays are not settlement days).
func IsSettlementDay(date time.Time, holidays map[string]struct{}) bool {
	switch date.Weekday() {
	case time.Saturday, time.Sunday:
		return false
	}
	_, isHoliday := holidays[date.Format(HolidayKeyFormat)]
	return !isHoliday
}

// ComputeSettleDate returns the settlement date for a trade executed on
// tradeDate with a T+settleOffset cycle, skipping weekends and venue holidays
// (TradeControl getSettleCodeAccountingHoliday analog). settleOffset == 0
// returns the next settlement day on/after tradeDate (T+0 still cannot settle
// on a holiday).
func ComputeSettleDate(tradeDate time.Time, settleOffset int, holidays map[string]struct{}) (time.Time, error) {
	if settleOffset < 0 {
		return time.Time{}, fmt.Errorf("settle offset must be >= 0, got %d", settleOffset)
	}

	date := time.Date(tradeDate.Year(), tradeDate.Month(), tradeDate.Day(), 0, 0, 0, 0, time.UTC)
	remaining := settleOffset
	// Guard against degenerate holiday calendars that would never settle.
	const maxLookahead = 366
	for i := 0; i < maxLookahead; i++ {
		if remaining > 0 {
			date = date.AddDate(0, 0, 1)
			if IsSettlementDay(date, holidays) {
				remaining--
			}
			continue
		}
		if IsSettlementDay(date, holidays) {
			return date, nil
		}
		date = date.AddDate(0, 0, 1)
	}
	return time.Time{}, fmt.Errorf("no settlement day found within %d days of %s", maxLookahead, tradeDate.Format(HolidayKeyFormat))
}

// RecalculateWAPrice recalculates the weighted-average purchase price after a
// buy, reproducing the TradeControl formula
//
//	WA_new = (tradeMoney + WA_old * qty_old) / (qty_trade + qty_old)
//
// with banker's rounding (HALF_EVEN) at scale 2. currentWA may be empty for a
// fresh position. Quantities and money are exact decimals.
func RecalculateWAPrice(currentWA string, currentQty, tradeQty, tradeMoney decimal.Decimal) (decimal.Decimal, error) {
	wa := decimal.Zero
	if currentWA != "" {
		parsed, err := decimal.NewFromString(currentWA)
		if err != nil {
			return decimal.Zero, fmt.Errorf("invalid current wa price %q: %w", currentWA, err)
		}
		wa = parsed
	}
	if currentQty.IsNegative() {
		return decimal.Zero, fmt.Errorf("current quantity cannot be negative: %s", currentQty)
	}
	if tradeQty.Sign() <= 0 {
		return decimal.Zero, fmt.Errorf("trade quantity must be positive: %s", tradeQty)
	}

	totalQty := tradeQty.Add(currentQty)
	numerator := tradeMoney.Add(wa.Mul(currentQty))
	return numerator.Div(totalQty).RoundBank(2), nil
}

// FreeBalance applies the TradeControl FreeBalanceAccountService thresholds:
//
//	d = sum1 - sum2            (available minus obligations)
//	d < 0                -> 0
//	sum0 set and d >= sum0 -> sum0
//	otherwise            -> d
//
// sum0 is an optional cap (nil = no cap).
func FreeBalance(sum0, sum1, sum2 *big.Int) *big.Int {
	if sum1 == nil {
		sum1 = big.NewInt(0)
	}
	if sum2 == nil {
		sum2 = big.NewInt(0)
	}
	available := new(big.Int).Sub(sum1, sum2)
	if available.Sign() < 0 {
		return big.NewInt(0)
	}
	if sum0 != nil && available.Cmp(sum0) >= 0 {
		return new(big.Int).Set(sum0)
	}
	return available
}

// FreeBalanceResult mirrors TradeControl FreeBalanceAccountResponse.
type FreeBalanceResult struct {
	IsSuccess       bool     `json:"is_success"`
	AvailableAmount *big.Int `json:"available_amount"`
	Currency        string   `json:"currency"`
	ErrorMessage    string   `json:"error_message,omitempty"`
}

// Metadata keys used to link the two legs of a booked trade.
const (
	TradeMetaRef         = "trade_ref"
	TradeMetaLeg         = "trade_leg"
	TradeMetaLegMoney    = "money"
	TradeMetaLegSecurity = "security"
	TradeMetaMoneyLegID  = "money_leg_id"
	TradeMetaInstrument  = "instrument"
	TradeMetaVenue       = "venue"
	TradeMetaQuantity    = "quantity"
	TradeMetaPrice       = "price"
	TradeMetaSettleDate  = "settle_date"
)

// TradeBooking describes a buy trade to be booked against the ledger
// (money hold + future security position).
type TradeBooking struct {
	LedgerID   string `json:"ledger_id"`
	IdentityID string `json:"identity_id"`
	AccountRef string `json:"account_ref"`

	Instrument string `json:"instrument"`
	Venue      string `json:"venue"`
	Currency   string `json:"currency"`

	// Quantity of securities and its precision (units -> minor units factor).
	Quantity          float64 `json:"quantity"`
	QuantityPrecision float64 `json:"quantity_precision"`
	// Per-unit price as an exact decimal string; money amount = quantity * price.
	Price string `json:"price"`
	// Precision for the money leg (e.g. 100 for cents).
	MoneyPrecision float64 `json:"money_precision"`

	SettleOffset int       `json:"settle_offset"` // T+N (used when SettleDate is zero)
	TradeDate    time.Time `json:"trade_date"`
	// SettleDate, when non-zero, sets the settlement date explicitly. Use this
	// when the offset N is not controlled and only the date is known; it wins
	// over SettleOffset and the venue holiday calendar.
	SettleDate time.Time `json:"settle_date"`

	// SettlementBalanceID receives the money hold (broker settlement balance).
	SettlementBalanceID string `json:"settlement_balance_id"`
	// MarketBalanceID is the counterparty securities balance (debited with overdraft).
	MarketBalanceID string `json:"market_balance_id"`

	Reference string `json:"reference"`
}

// Validate checks the booking request invariants.
func (t TradeBooking) Validate() error {
	if t.LedgerID == "" || t.AccountRef == "" || t.Currency == "" {
		return fmt.Errorf("trade booking: ledger_id, account_ref and currency are required")
	}
	if t.Instrument == "" {
		return fmt.Errorf("trade booking: instrument is required")
	}
	if t.Quantity <= 0 {
		return fmt.Errorf("trade booking: quantity must be positive")
	}
	if t.QuantityPrecision < 1 {
		return fmt.Errorf("trade booking: quantity_precision must be >= 1")
	}
	if t.MoneyPrecision < 1 {
		return fmt.Errorf("trade booking: money_precision must be >= 1")
	}
	if _, err := decimal.NewFromString(t.Price); err != nil {
		return fmt.Errorf("trade booking: invalid price %q: %w", t.Price, err)
	}
	if t.SettleOffset < 0 {
		return fmt.Errorf("trade booking: settle_offset must be >= 0")
	}
	if t.SettlementBalanceID == "" || t.MarketBalanceID == "" {
		return fmt.Errorf("trade booking: settlement_balance_id and market_balance_id are required")
	}
	if t.Reference == "" {
		return fmt.Errorf("trade booking: reference is required")
	}
	return nil
}

// TradeBookingResult reports the artifacts created by BookTrade.
type TradeBookingResult struct {
	TradeRef          string    `json:"trade_ref"`
	MoneyTxnID        string    `json:"money_txn_id"`
	SecurityTxnID     string    `json:"security_txn_id"`
	MoneyBalanceID    string    `json:"money_balance_id"`
	PositionBalanceID string    `json:"position_balance_id"`
	SettleDate        time.Time `json:"settle_date"`
}

// SellBooking describes a sell trade. The legs mirror a buy: securities flow
// out of the client position to the market counterparty (a hold that reduces
// the tradable position) and money flows in from settlement. The available
// quantity is validated with the settle-date-aware tradable arithmetic before
// the holds are placed.
type SellBooking struct {
	LedgerID   string `json:"ledger_id"`
	IdentityID string `json:"identity_id"`
	AccountRef string `json:"account_ref"`

	Instrument string `json:"instrument"`
	Venue      string `json:"venue"`
	Currency   string `json:"currency"`

	Quantity          float64 `json:"quantity"`
	QuantityPrecision float64 `json:"quantity_precision"`
	Price             string  `json:"price"`
	MoneyPrecision    float64 `json:"money_precision"`

	// SettleOffset is the requested T+N; it is overridden by the instrument
	// settings when those exist, and ignored when SettleDate is set.
	SettleOffset int       `json:"settle_offset"`
	TradeDate    time.Time `json:"trade_date"`
	// SettleDate, when non-zero, sets the settlement date explicitly (used when
	// the offset N is not controlled). It wins over SettleOffset.
	SettleDate time.Time `json:"settle_date"`

	// SettlementBalanceID is the broker settlement balance funding the proceeds.
	SettlementBalanceID string `json:"settlement_balance_id"`
	// MarketBalanceID is the counterparty securities balance receiving delivery.
	MarketBalanceID string `json:"market_balance_id"`

	Reference string `json:"reference"`
}

// Validate checks the sell booking invariants.
func (s SellBooking) Validate() error {
	if s.LedgerID == "" || s.AccountRef == "" || s.Currency == "" {
		return fmt.Errorf("sell booking: ledger_id, account_ref and currency are required")
	}
	if s.Instrument == "" {
		return fmt.Errorf("sell booking: instrument is required")
	}
	if s.Quantity <= 0 {
		return fmt.Errorf("sell booking: quantity must be positive")
	}
	if s.QuantityPrecision < 1 {
		return fmt.Errorf("sell booking: quantity_precision must be >= 1")
	}
	if s.MoneyPrecision < 1 {
		return fmt.Errorf("sell booking: money_precision must be >= 1")
	}
	if _, err := decimal.NewFromString(s.Price); err != nil {
		return fmt.Errorf("sell booking: invalid price %q: %w", s.Price, err)
	}
	if s.SettleOffset < 0 {
		return fmt.Errorf("sell booking: settle_offset must be >= 0")
	}
	if s.SettlementBalanceID == "" || s.MarketBalanceID == "" {
		return fmt.Errorf("sell booking: settlement_balance_id and market_balance_id are required")
	}
	if s.Reference == "" {
		return fmt.Errorf("sell booking: reference is required")
	}
	return nil
}

// SellBookingResult reports the artifacts created by SellTrade.
type SellBookingResult struct {
	TradeRef          string           `json:"trade_ref"`
	MoneyTxnID        string           `json:"money_txn_id"`
	SecurityTxnID     string           `json:"security_txn_id"`
	MoneyBalanceID    string           `json:"money_balance_id"`
	PositionBalanceID string           `json:"position_balance_id"`
	SettleDate        time.Time        `json:"settle_date"`
	Tradable          TradablePosition `json:"tradable"`
}

// TradeSettlementResult reports the artifacts of settling one trade.
type TradeSettlementResult struct {
	TradeRef          string `json:"trade_ref"`
	MoneyTxnID        string `json:"money_txn_id,omitempty"`
	SecurityTxnID     string `json:"security_txn_id"`
	RollTxnID         string `json:"roll_txn_id"`
	SpotBalanceID     string `json:"spot_balance_id"`
	FutureBalanceID   string `json:"future_balance_id"`
	WAPrice           string `json:"wa_price,omitempty"`
	LotID             string `json:"lot_id,omitempty"`
}

// SettlementRunResult summarizes a RunSettlement pass.
type SettlementRunResult struct {
	AsOf     time.Time               `json:"as_of"`
	Settled  []TradeSettlementResult `json:"settled"`
	Errors   map[string]string       `json:"errors,omitempty"` // balance/txn id -> error
	Examined int                     `json:"examined"`
}

// HoldsRecalcResult summarizes a RecalculateHolds pass
// (TradeControl recalculateBlockedWaiting* analog: per-id error aggregation).
type HoldsRecalcResult struct {
	Recalculated []string          `json:"recalculated"`
	Errors       map[string]string `json:"errors,omitempty"` // balance id -> error
}
