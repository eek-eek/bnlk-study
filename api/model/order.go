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
	"time"

	"github.com/blnkfinance/blnk/model"
)

// CreateOrderRequest creates a buy/sell order.
type CreateOrderRequest struct {
	Reference  string `json:"reference"`
	LedgerID   string `json:"ledger_id"`
	IdentityID string `json:"identity_id"`
	AccountRef string `json:"account_ref"`

	Instrument string `json:"instrument"`
	Venue      string `json:"venue"`
	Currency   string `json:"currency"`
	Side       string `json:"side"`

	Quantity          string `json:"quantity"`
	QuantityPrecision int    `json:"quantity_precision"`
	Price             string `json:"price"`
	MoneyPrecision    int    `json:"money_precision"`

	SettleOffset int    `json:"settle_offset"`
	SettleDate   string `json:"settle_date"`

	SettlementBalanceID string `json:"settlement_balance_id"`
	MarketBalanceID     string `json:"market_balance_id"`
}

// ToOrder converts and validates the request into a domain order.
func (r CreateOrderRequest) ToOrder() (model.Order, error) {
	o := model.Order{
		Reference:           r.Reference,
		LedgerID:            r.LedgerID,
		IdentityID:          r.IdentityID,
		AccountRef:          r.AccountRef,
		Instrument:          r.Instrument,
		Venue:               r.Venue,
		Currency:            r.Currency,
		Side:                r.Side,
		Quantity:            r.Quantity,
		QuantityPrecision:   r.QuantityPrecision,
		Price:               r.Price,
		MoneyPrecision:      r.MoneyPrecision,
		SettleOffset:        r.SettleOffset,
		SettlementBalanceID: r.SettlementBalanceID,
		MarketBalanceID:     r.MarketBalanceID,
	}
	if r.SettleDate != "" {
		parsed, err := time.Parse(model.HolidayKeyFormat, r.SettleDate)
		if err != nil {
			return model.Order{}, fmt.Errorf("invalid settle_date %q, expected YYYY-MM-DD", r.SettleDate)
		}
		o.SettleDate = &parsed
	}
	return o, o.Validate()
}

// DictRequest upserts a configurable reference value.
type DictRequest struct {
	Category string `json:"category"`
	Code     string `json:"code"`
	Name     string `json:"name"`
	Sort     int    `json:"sort"`
	Active   *bool  `json:"active"`
}

// ToDictEntry converts the request (active defaults to true).
func (r DictRequest) ToDictEntry() model.DictEntry {
	active := true
	if r.Active != nil {
		active = *r.Active
	}
	return model.DictEntry{Category: r.Category, Code: r.Code, Name: r.Name, Sort: r.Sort, Active: active}
}

// StopListRequest blocks an identity (optionally per instrument).
type StopListRequest struct {
	IdentityID string `json:"identity_id"`
	Instrument string `json:"instrument"`
	Reason     string `json:"reason"`
}

// ToStopListEntry converts the request.
func (r StopListRequest) ToStopListEntry() model.StopListEntry {
	return model.StopListEntry{IdentityID: r.IdentityID, Instrument: r.Instrument, Reason: r.Reason}
}

// TradingTimeRequest upserts a venue trading window.
type TradingTimeRequest struct {
	Venue     string `json:"venue"`
	Weekday   int    `json:"weekday"`
	OpenTime  string `json:"open_time"`
	CloseTime string `json:"close_time"`
}

// ToTradingTime converts the request.
func (r TradingTimeRequest) ToTradingTime() model.TradingTime {
	return model.TradingTime{Venue: r.Venue, Weekday: r.Weekday, OpenTime: r.OpenTime, CloseTime: r.CloseTime}
}
