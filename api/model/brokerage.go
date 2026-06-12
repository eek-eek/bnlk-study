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
	"fmt"
	"math/big"
	"time"

	"github.com/blnkfinance/blnk/model"

	validation "github.com/go-ozzo/ozzo-validation/v4"
)

// PositionKeyRequest identifies a brokerage position in API requests.
type PositionKeyRequest struct {
	LedgerID   string `json:"ledger_id"`
	IdentityID string `json:"identity_id"`
	AccountRef string `json:"account_ref"`
	Instrument string `json:"instrument"`
	Currency   string `json:"currency"`
	SettleCode *int   `json:"settle_code"`
}

// ToPositionKey converts the request into the domain key.
func (p PositionKeyRequest) ToPositionKey() model.PositionKey {
	return model.PositionKey{
		LedgerID:   p.LedgerID,
		IdentityID: p.IdentityID,
		AccountRef: p.AccountRef,
		Instrument: p.Instrument,
		Currency:   p.Currency,
		SettleCode: p.SettleCode,
	}
}

// ValidatePositionKey validates the mandatory key dimensions.
func (p PositionKeyRequest) ValidatePositionKey() error {
	return validation.ValidateStruct(&p,
		validation.Field(&p.LedgerID, validation.Required),
		validation.Field(&p.AccountRef, validation.Required),
		validation.Field(&p.Currency, validation.Required),
	)
}

// CreatePositionRequest creates (or returns) a position balance. SettleDate is
// required when SettleCode is set ("2006-01-02").
type CreatePositionRequest struct {
	PositionKeyRequest
	SettleDate string `json:"settle_date"`
}

// ParsedSettleDate parses the optional settle date.
func (c CreatePositionRequest) ParsedSettleDate() (*time.Time, error) {
	if c.SettleDate == "" {
		return nil, nil
	}
	parsed, err := time.Parse(model.HolidayKeyFormat, c.SettleDate)
	if err != nil {
		return nil, fmt.Errorf("invalid settle_date %q, expected YYYY-MM-DD", c.SettleDate)
	}
	return &parsed, nil
}

// ValidateCreatePosition validates the request.
func (c CreatePositionRequest) ValidateCreatePosition() error {
	if err := c.ValidatePositionKey(); err != nil {
		return err
	}
	if c.SettleCode != nil && c.SettleDate == "" {
		return fmt.Errorf("settle_date is required when settle_code is set")
	}
	return nil
}

// BalanceDeltaRequest is one mutation plan entry; amounts are signed integer
// strings in minor units (big.Int).
type BalanceDeltaRequest struct {
	Key          PositionKeyRequest `json:"key"`
	AmountDelta  string             `json:"amount_delta"`
	BlockedDelta string             `json:"blocked_delta"`
	WaitingDelta string             `json:"waiting_delta"`
}

// parseOptionalBigInt parses a signed integer string, "" meaning nil.
func parseOptionalBigInt(field, value string) (*big.Int, error) {
	if value == "" {
		return nil, nil
	}
	parsed, ok := new(big.Int).SetString(value, 10)
	if !ok {
		return nil, fmt.Errorf("invalid %s %q, expected a base-10 integer", field, value)
	}
	return parsed, nil
}

// ToBalanceDelta converts the request entry into the domain delta.
func (b BalanceDeltaRequest) ToBalanceDelta() (model.BalanceDelta, error) {
	amount, err := parseOptionalBigInt("amount_delta", b.AmountDelta)
	if err != nil {
		return model.BalanceDelta{}, err
	}
	blocked, err := parseOptionalBigInt("blocked_delta", b.BlockedDelta)
	if err != nil {
		return model.BalanceDelta{}, err
	}
	waiting, err := parseOptionalBigInt("waiting_delta", b.WaitingDelta)
	if err != nil {
		return model.BalanceDelta{}, err
	}
	return model.BalanceDelta{
		Key:          b.Key.ToPositionKey(),
		AmountDelta:  amount,
		BlockedDelta: blocked,
		WaitingDelta: waiting,
	}, nil
}

// ApplyMutationPlanRequest carries an atomic multi-position mutation plan.
type ApplyMutationPlanRequest struct {
	Deltas []BalanceDeltaRequest `json:"deltas"`
}

// ToMutationPlan converts and validates the request into a domain plan.
func (a ApplyMutationPlanRequest) ToMutationPlan() (model.MutationPlan, error) {
	if len(a.Deltas) == 0 {
		return model.MutationPlan{}, fmt.Errorf("at least one delta is required")
	}
	plan := model.MutationPlan{Deltas: make([]model.BalanceDelta, 0, len(a.Deltas))}
	for i, request := range a.Deltas {
		if err := request.Key.ValidatePositionKey(); err != nil {
			return model.MutationPlan{}, fmt.Errorf("delta %d: %w", i, err)
		}
		delta, err := request.ToBalanceDelta()
		if err != nil {
			return model.MutationPlan{}, fmt.Errorf("delta %d: %w", i, err)
		}
		plan.Deltas = append(plan.Deltas, delta)
	}
	return plan, nil
}

// CreateHolidayRequest registers a venue holiday ("2006-01-02").
type CreateHolidayRequest struct {
	Venue       string `json:"venue"`
	Date        string `json:"date"`
	Description string `json:"description"`
}

// ToMarketHoliday converts and validates the request.
func (c CreateHolidayRequest) ToMarketHoliday() (model.MarketHoliday, error) {
	if err := validation.ValidateStruct(&c,
		validation.Field(&c.Venue, validation.Required),
		validation.Field(&c.Date, validation.Required),
	); err != nil {
		return model.MarketHoliday{}, err
	}
	parsed, err := time.Parse(model.HolidayKeyFormat, c.Date)
	if err != nil {
		return model.MarketHoliday{}, fmt.Errorf("invalid date %q, expected YYYY-MM-DD", c.Date)
	}
	return model.MarketHoliday{Venue: c.Venue, HolidayDate: parsed, Description: c.Description}, nil
}

// ComputeSettleDateRequest computes a T+N settle date for a venue.
type ComputeSettleDateRequest struct {
	Venue        string `json:"venue"`
	TradeDate    string `json:"trade_date"`
	SettleOffset int    `json:"settle_offset"`
}

// ParsedTradeDate parses the optional trade date (default: today).
func (c ComputeSettleDateRequest) ParsedTradeDate() (time.Time, error) {
	if c.TradeDate == "" {
		return time.Now(), nil
	}
	parsed, err := time.Parse(model.HolidayKeyFormat, c.TradeDate)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid trade_date %q, expected YYYY-MM-DD", c.TradeDate)
	}
	return parsed, nil
}

// BookTradeRequest books a buy trade (money hold + future security position).
type BookTradeRequest struct {
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

	SettleOffset int    `json:"settle_offset"`
	TradeDate    string `json:"trade_date"`

	SettlementBalanceID string `json:"settlement_balance_id"`
	MarketBalanceID     string `json:"market_balance_id"`

	Reference string `json:"reference"`
}

// ToTradeBooking converts the request into the domain booking.
func (b BookTradeRequest) ToTradeBooking() (model.TradeBooking, error) {
	booking := model.TradeBooking{
		LedgerID:            b.LedgerID,
		IdentityID:          b.IdentityID,
		AccountRef:          b.AccountRef,
		Instrument:          b.Instrument,
		Venue:               b.Venue,
		Currency:            b.Currency,
		Quantity:            b.Quantity,
		QuantityPrecision:   b.QuantityPrecision,
		Price:               b.Price,
		MoneyPrecision:      b.MoneyPrecision,
		SettleOffset:        b.SettleOffset,
		SettlementBalanceID: b.SettlementBalanceID,
		MarketBalanceID:     b.MarketBalanceID,
		Reference:           b.Reference,
	}
	if b.TradeDate != "" {
		parsed, err := time.Parse(model.HolidayKeyFormat, b.TradeDate)
		if err != nil {
			return model.TradeBooking{}, fmt.Errorf("invalid trade_date %q, expected YYYY-MM-DD", b.TradeDate)
		}
		booking.TradeDate = parsed
	}
	return booking, booking.Validate()
}

// RecalculateHoldsRequest rebuilds blocked/waiting holds for balances.
type RecalculateHoldsRequest struct {
	BalanceIDs []string `json:"balance_ids"`
}

// ValidateRecalculateHolds validates the request.
func (r RecalculateHoldsRequest) ValidateRecalculateHolds() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.BalanceIDs, validation.Required, validation.Length(1, 0)),
	)
}

// InstrumentSettingsRequest configures the trading mode of an instrument.
type InstrumentSettingsRequest struct {
	Instrument     string `json:"instrument"`
	Venue          string `json:"venue"`
	TradesOnTheWay bool   `json:"trades_on_the_way"`
	SettleOffset   int    `json:"settle_offset"`
}

// ToInstrumentSettings converts and validates the request.
func (i InstrumentSettingsRequest) ToInstrumentSettings() (model.InstrumentSettings, error) {
	if err := validation.ValidateStruct(&i,
		validation.Field(&i.Instrument, validation.Required),
		validation.Field(&i.SettleOffset, validation.Min(0)),
	); err != nil {
		return model.InstrumentSettings{}, err
	}
	if i.TradesOnTheWay && i.SettleOffset == 0 {
		return model.InstrumentSettings{}, fmt.Errorf("settle_offset must be > 0 when trades_on_the_way is true")
	}
	return model.InstrumentSettings{
		Instrument:     i.Instrument,
		Venue:          i.Venue,
		TradesOnTheWay: i.TradesOnTheWay,
		SettleOffset:   i.SettleOffset,
	}, nil
}

// SellTradeRequest books a sell trade (delivery hold + money credit).
type SellTradeRequest struct {
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

	SettleOffset int    `json:"settle_offset"`
	TradeDate    string `json:"trade_date"`

	SettlementBalanceID string `json:"settlement_balance_id"`
	MarketBalanceID     string `json:"market_balance_id"`

	Reference string `json:"reference"`
}

// ToSellBooking converts the request into the domain booking.
func (s SellTradeRequest) ToSellBooking() (model.SellBooking, error) {
	booking := model.SellBooking{
		LedgerID:            s.LedgerID,
		IdentityID:          s.IdentityID,
		AccountRef:          s.AccountRef,
		Instrument:          s.Instrument,
		Venue:               s.Venue,
		Currency:            s.Currency,
		Quantity:            s.Quantity,
		QuantityPrecision:   s.QuantityPrecision,
		Price:               s.Price,
		MoneyPrecision:      s.MoneyPrecision,
		SettleOffset:        s.SettleOffset,
		SettlementBalanceID: s.SettlementBalanceID,
		MarketBalanceID:     s.MarketBalanceID,
		Reference:           s.Reference,
	}
	if s.TradeDate != "" {
		parsed, err := time.Parse(model.HolidayKeyFormat, s.TradeDate)
		if err != nil {
			return model.SellBooking{}, fmt.Errorf("invalid trade_date %q, expected YYYY-MM-DD", s.TradeDate)
		}
		booking.TradeDate = parsed
	}
	return booking, booking.Validate()
}

// RunSettlementRequest triggers a settlement pass.
type RunSettlementRequest struct {
	AsOf  string `json:"as_of"`
	Limit int    `json:"limit"`
}

// ParsedAsOf parses the optional as-of date (default: now).
func (r RunSettlementRequest) ParsedAsOf() (time.Time, error) {
	if r.AsOf == "" {
		return time.Now(), nil
	}
	parsed, err := time.Parse(model.HolidayKeyFormat, r.AsOf)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid as_of %q, expected YYYY-MM-DD", r.AsOf)
	}
	return parsed, nil
}
