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
package api

import (
	"math/big"
	"net/http"
	"time"

	model2 "github.com/blnkfinance/blnk/api/model"
	"github.com/blnkfinance/blnk/internal/apierror"
	"github.com/blnkfinance/blnk/model"
	"github.com/gin-gonic/gin"
)

// CreateMarketHoliday registers a non-settlement day for a trading venue.
func (a Api) CreateMarketHoliday(c *gin.Context) {
	var request model2.CreateHolidayRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		respondCode(c, apierror.ErrGenMalformedRequest, err.Error(), nil)
		return
	}
	holiday, err := request.ToMarketHoliday()
	if err != nil {
		respondCode(c, apierror.ErrGenValidation, err.Error(), nil)
		return
	}
	resp, err := a.blnk.AddMarketHoliday(c.Request.Context(), holiday)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusCreated, resp)
}

// GetMarketHolidays lists venue holidays inside an optional date range.
func (a Api) GetMarketHolidays(c *gin.Context) {
	venue, passed := c.Params.Get("venue")
	if !passed {
		respondCode(c, apierror.ErrGenValidation, "venue is required. pass venue in the route /brokerage/holidays/:venue", nil)
		return
	}

	from := time.Now().AddDate(-1, 0, 0)
	to := time.Now().AddDate(1, 0, 0)
	var err error
	if raw := c.Query("from"); raw != "" {
		if from, err = time.Parse(model.HolidayKeyFormat, raw); err != nil {
			respondCode(c, apierror.ErrGenValidation, "invalid from date, expected YYYY-MM-DD", nil)
			return
		}
	}
	if raw := c.Query("to"); raw != "" {
		if to, err = time.Parse(model.HolidayKeyFormat, raw); err != nil {
			respondCode(c, apierror.ErrGenValidation, "invalid to date, expected YYYY-MM-DD", nil)
			return
		}
	}

	holidays, err := a.blnk.GetMarketHolidays(c.Request.Context(), venue, from, to)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, holidays)
}

// ComputeSettleDate computes the holiday-aware T+N settlement date.
func (a Api) ComputeSettleDate(c *gin.Context) {
	var request model2.ComputeSettleDateRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		respondCode(c, apierror.ErrGenMalformedRequest, err.Error(), nil)
		return
	}
	tradeDate, err := request.ParsedTradeDate()
	if err != nil {
		respondCode(c, apierror.ErrGenValidation, err.Error(), nil)
		return
	}
	settleDate, err := a.blnk.ComputeSettleDate(c.Request.Context(), request.Venue, tradeDate, request.SettleOffset)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"venue":         request.Venue,
		"trade_date":    tradeDate.Format(model.HolidayKeyFormat),
		"settle_offset": request.SettleOffset,
		"settle_date":   settleDate.Format(model.HolidayKeyFormat),
	})
}

// CreatePosition finds or creates a position balance for a key.
func (a Api) CreatePosition(c *gin.Context) {
	var request model2.CreatePositionRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		respondCode(c, apierror.ErrGenMalformedRequest, err.Error(), nil)
		return
	}
	if err := request.ValidateCreatePosition(); err != nil {
		respondCode(c, apierror.ErrGenValidation, err.Error(), nil)
		return
	}
	key, err := request.ToPositionKey()
	if err != nil {
		respondCode(c, apierror.ErrGenValidation, err.Error(), nil)
		return
	}
	resp, err := a.blnk.GetOrCreatePosition(c.Request.Context(), key)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusCreated, resp)
}

// GetActivePosition resolves the active balance through the settle-date cascade
// (the latest bucket settling on or before max_settle_date, falling back to
// spot). Omit max_settle_date to read the spot balance.
func (a Api) GetActivePosition(c *gin.Context) {
	ledgerID := c.Query("ledger_id")
	accountRef := c.Query("account_ref")
	currency := c.Query("currency")
	if ledgerID == "" || accountRef == "" || currency == "" {
		respondCode(c, apierror.ErrGenValidation, "ledger_id, account_ref and currency query parameters are required", nil)
		return
	}

	var maxSettleDate *time.Time
	if raw := c.Query("max_settle_date"); raw != "" {
		parsed, err := time.Parse(model.HolidayKeyFormat, raw)
		if err != nil {
			respondCode(c, apierror.ErrGenValidation, "max_settle_date must be a date YYYY-MM-DD", nil)
			return
		}
		maxSettleDate = &parsed
	}

	resp, err := a.blnk.GetActivePosition(c.Request.Context(), ledgerID, c.Query("identity_id"),
		accountRef, c.Query("instrument"), currency, maxSettleDate)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, resp)
}

// SetInstrumentSettings configures the trading mode of an instrument.
func (a Api) SetInstrumentSettings(c *gin.Context) {
	var request model2.InstrumentSettingsRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		respondCode(c, apierror.ErrGenMalformedRequest, err.Error(), nil)
		return
	}
	settings, err := request.ToInstrumentSettings()
	if err != nil {
		respondCode(c, apierror.ErrGenValidation, err.Error(), nil)
		return
	}
	resp, err := a.blnk.SetInstrumentSettings(c.Request.Context(), settings)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusCreated, resp)
}

// GetInstrumentSettings returns the trading mode of an instrument.
func (a Api) GetInstrumentSettings(c *gin.Context) {
	instrument, passed := c.Params.Get("instrument")
	if !passed {
		respondCode(c, apierror.ErrGenValidation, "instrument is required. pass it in the route /brokerage/instruments/:instrument", nil)
		return
	}
	resp, err := a.blnk.GetInstrumentSettings(c.Request.Context(), instrument)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, resp)
}

// GetTradablePosition returns the settle-date-aware quantity available to sell.
// The settle date is resolved from the instrument's configured T+N cycle and
// the venue holiday calendar.
func (a Api) GetTradablePosition(c *gin.Context) {
	ledgerID := c.Query("ledger_id")
	accountRef := c.Query("account_ref")
	instrument := c.Query("instrument")
	currency := c.Query("currency")
	if ledgerID == "" || accountRef == "" || instrument == "" || currency == "" {
		respondCode(c, apierror.ErrGenValidation, "ledger_id, account_ref, instrument and currency query parameters are required", nil)
		return
	}

	tradeDate := time.Now()
	if raw := c.Query("trade_date"); raw != "" {
		parsed, err := time.Parse(model.HolidayKeyFormat, raw)
		if err != nil {
			respondCode(c, apierror.ErrGenValidation, "invalid trade_date, expected YYYY-MM-DD", nil)
			return
		}
		tradeDate = parsed
	}

	settings, err := a.blnk.GetInstrumentSettings(c.Request.Context(), instrument)
	venue := c.Query("venue")
	settleOffset := 0
	if err == nil {
		if settings.TradesOnTheWay {
			settleOffset = settings.SettleOffset
		}
		if settings.Venue != "" {
			venue = settings.Venue
		}
	}
	settleDate, err := a.blnk.ComputeSettleDate(c.Request.Context(), venue, tradeDate, settleOffset)
	if err != nil {
		respondError(c, err)
		return
	}

	resp, err := a.blnk.GetTradablePosition(c.Request.Context(), ledgerID, c.Query("identity_id"),
		accountRef, instrument, currency, settleDate)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, resp)
}

// SellTrade books a sell trade after the settle-date-aware availability check.
func (a Api) SellTrade(c *gin.Context) {
	var request model2.SellTradeRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		respondCode(c, apierror.ErrGenMalformedRequest, err.Error(), nil)
		return
	}
	booking, err := request.ToSellBooking()
	if err != nil {
		respondCode(c, apierror.ErrGenValidation, err.Error(), nil)
		return
	}
	resp, err := a.blnk.SellTrade(c.Request.Context(), booking)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusCreated, resp)
}

// BookTrade books a buy trade: money hold + future security position.
func (a Api) BookTrade(c *gin.Context) {
	var request model2.BookTradeRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		respondCode(c, apierror.ErrGenMalformedRequest, err.Error(), nil)
		return
	}
	booking, err := request.ToTradeBooking()
	if err != nil {
		respondCode(c, apierror.ErrGenValidation, err.Error(), nil)
		return
	}
	resp, err := a.blnk.BookTrade(c.Request.Context(), booking)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusCreated, resp)
}

// SettleTrade settles one booked trade by its security leg transaction ID.
func (a Api) SettleTrade(c *gin.Context) {
	txID, passed := c.Params.Get("txID")
	if !passed {
		respondCode(c, apierror.ErrGenValidation, "transaction id is required. pass id in the route /brokerage/trades/:txID/settle", nil)
		return
	}
	resp, err := a.blnk.SettleTrade(c.Request.Context(), txID)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, resp)
}

// RunSettlement settles everything matured by the as-of date.
func (a Api) RunSettlement(c *gin.Context) {
	var request model2.RunSettlementRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		respondCode(c, apierror.ErrGenMalformedRequest, err.Error(), nil)
		return
	}
	asOf, err := request.ParsedAsOf()
	if err != nil {
		respondCode(c, apierror.ErrGenValidation, err.Error(), nil)
		return
	}
	resp, err := a.blnk.RunSettlement(c.Request.Context(), asOf, request.Limit)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, resp)
}

// ApplyMutationPlan applies an atomic multi-position mutation plan.
func (a Api) ApplyMutationPlan(c *gin.Context) {
	var request model2.ApplyMutationPlanRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		respondCode(c, apierror.ErrGenMalformedRequest, err.Error(), nil)
		return
	}
	plan, err := request.ToMutationPlan()
	if err != nil {
		respondCode(c, apierror.ErrGenValidation, err.Error(), nil)
		return
	}
	if err := a.blnk.ApplyMutationPlan(c.Request.Context(), plan); err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"applied": len(plan.Normalized())})
}

// RecalculateHolds rebuilds blocked/waiting holds from live transactions.
func (a Api) RecalculateHolds(c *gin.Context) {
	var request model2.RecalculateHoldsRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		respondCode(c, apierror.ErrGenMalformedRequest, err.Error(), nil)
		return
	}
	if err := request.ValidateRecalculateHolds(); err != nil {
		respondCode(c, apierror.ErrGenValidation, err.Error(), nil)
		return
	}
	resp, err := a.blnk.RecalculateHolds(c.Request.Context(), request.BalanceIDs)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, resp)
}

// GetFreeBalance returns the operable amount of a balance with the
// Sum0/Sum1/Sum2 thresholds; the optional cap query parameter is Sum0.
func (a Api) GetFreeBalance(c *gin.Context) {
	id, passed := c.Params.Get("id")
	if !passed {
		respondCode(c, apierror.ErrGenValidation, "id is required. pass id in the route /brokerage/balances/:id/free", nil)
		return
	}

	var cap *big.Int
	if raw := c.Query("cap"); raw != "" {
		parsed, ok := new(big.Int).SetString(raw, 10)
		if !ok {
			respondCode(c, apierror.ErrGenValidation, "cap must be a base-10 integer in minor units", nil)
			return
		}
		cap = parsed
	}

	resp, err := a.blnk.GetFreeBalance(c.Request.Context(), id, cap)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, resp)
}

// GetBalanceLots lists the purchase lots (BalanceDetail) of a balance.
func (a Api) GetBalanceLots(c *gin.Context) {
	id, passed := c.Params.Get("id")
	if !passed {
		respondCode(c, apierror.ErrGenValidation, "id is required. pass id in the route /brokerage/balances/:id/lots", nil)
		return
	}
	lots, err := a.blnk.GetBalanceLots(c.Request.Context(), id)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, lots)
}
