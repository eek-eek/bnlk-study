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

// Brokerage service layer: the TradeControl balance subsystem ported onto the
// blnk ledger. Money holds are inflight debits (TradeControl blockedAmount),
// expected deliveries are inflight credits (waitingAmount), T+N positions are
// settle-coded balances created on the fly, the settlement roll is plain
// double-entry between the future and spot balances, and out-of-band
// corrections go through distributed-lock-guarded mutation plans.
package blnk

import (
	"context"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/blnkfinance/blnk/internal/apierror"
	redlock "github.com/blnkfinance/blnk/internal/lock"
	"github.com/blnkfinance/blnk/internal/metrics"
	"github.com/blnkfinance/blnk/model"
	"github.com/shopspring/decimal"
	"github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/metric"
	"golang.org/x/sync/semaphore"
)

const (
	// holdsRecalcConcurrency bounds parallel hold recomputation
	// (TradeControl used a fixed pool of 10 threads).
	holdsRecalcConcurrency = 10
	// settleDateHorizonDays bounds the holiday calendar window loaded for
	// settle-date computation.
	settleDateHorizonDays = 366
	// defaultSettlementBatch bounds one RunSettlement pass.
	defaultSettlementBatch = 100
)

// AddMarketHoliday registers a non-settlement day for a trading venue.
func (l *Blnk) AddMarketHoliday(ctx context.Context, holiday model.MarketHoliday) (model.MarketHoliday, error) {
	return l.datasource.CreateHoliday(ctx, holiday)
}

// GetMarketHolidays lists the holidays of a venue inside [from, to].
func (l *Blnk) GetMarketHolidays(ctx context.Context, venue string, from, to time.Time) ([]model.MarketHoliday, error) {
	return l.datasource.GetHolidays(ctx, venue, from, to)
}

// marketHolidaySet loads the venue holiday calendar as a lookup set keyed by
// model.HolidayKeyFormat dates.
func (l *Blnk) marketHolidaySet(ctx context.Context, venue string, from time.Time) (map[string]struct{}, error) {
	set := make(map[string]struct{})
	if venue == "" {
		return set, nil
	}
	holidays, err := l.datasource.GetHolidays(ctx, venue, from, from.AddDate(0, 0, settleDateHorizonDays))
	if err != nil {
		return nil, err
	}
	for _, holiday := range holidays {
		set[holiday.HolidayDate.Format(model.HolidayKeyFormat)] = struct{}{}
	}
	return set, nil
}

// ComputeSettleDate returns the T+offset settlement date for a venue,
// skipping weekends and venue holidays (TradeControl
// getSettleCodeAccountingHoliday analog).
func (l *Blnk) ComputeSettleDate(ctx context.Context, venue string, tradeDate time.Time, settleOffset int) (time.Time, error) {
	holidays, err := l.marketHolidaySet(ctx, venue, tradeDate)
	if err != nil {
		return time.Time{}, err
	}
	return model.ComputeSettleDate(tradeDate, settleOffset, holidays)
}

// GetOrCreatePosition resolves the position balance for the key, creating it
// when missing. The settlement bucket is identified by key.SettleDate
// (nil = spot).
func (l *Blnk) GetOrCreatePosition(ctx context.Context, key model.PositionKey) (*model.Balance, error) {
	return l.datasource.FindOrCreatePosition(ctx, key)
}

// GetActivePosition resolves the active balance for the dimensions with the
// TradeControl settle cascade by date (the latest bucket settling on or before
// maxSettleDate, falling back to spot).
func (l *Blnk) GetActivePosition(ctx context.Context, ledgerID, identityID, accountRef, instrument, currency string, maxSettleDate *time.Time) (*model.Balance, error) {
	return l.datasource.GetActivePosition(ctx, ledgerID, identityID, accountRef, instrument, currency, maxSettleDate)
}

// GetBalanceLots lists the purchase lots of a balance (BalanceDetail analog).
func (l *Blnk) GetBalanceLots(ctx context.Context, balanceID string) ([]model.BalanceLot, error) {
	return l.datasource.GetLots(ctx, balanceID)
}

// SetInstrumentSettings stores the trading mode of an instrument.
func (l *Blnk) SetInstrumentSettings(ctx context.Context, settings model.InstrumentSettings) (model.InstrumentSettings, error) {
	return l.datasource.UpsertInstrumentSettings(ctx, settings)
}

// GetInstrumentSettings returns the trading mode of an instrument.
func (l *Blnk) GetInstrumentSettings(ctx context.Context, instrument string) (*model.InstrumentSettings, error) {
	return l.datasource.GetInstrumentSettings(ctx, instrument)
}

// tradeParams is the resolved trading mode for one trade.
type tradeParams struct {
	onTheWay     bool
	settleOffset int
	venue        string
}

// resolveTradeParams resolves the trading mode for an instrument.
//
// Semantics:
//   - Configured settings win: an on-the-way instrument keeps its T+N offset; a
//     non-on-the-way one is forced to T+0.
//   - No settings: the instrument is NOT on-the-way (availability never lends
//     against in-transit incoming purchases), but the caller-supplied offset is
//     still honored so the trade settles on its requested/explicit date. This
//     is safe because outgoing holds always reduce availability (see
//     ComputeTradable), so it cannot be oversold regardless of the offset.
func (l *Blnk) resolveTradeParams(ctx context.Context, instrument string, requestedOffset int, requestedVenue string) (tradeParams, error) {
	settings, err := l.datasource.GetInstrumentSettings(ctx, instrument)
	if err != nil {
		if apiErr, ok := err.(apierror.APIError); ok && apiErr.Code == apierror.ErrNotFound {
			return tradeParams{onTheWay: false, settleOffset: requestedOffset, venue: requestedVenue}, nil
		}
		return tradeParams{}, err
	}
	venue := settings.Venue
	if venue == "" {
		venue = requestedVenue
	}
	offset := settings.SettleOffset
	if !settings.TradesOnTheWay {
		// Immediate settlement: never carry future quantity, settle on the spot
		// cycle even if a larger offset was configured.
		offset = 0
	}
	return tradeParams{onTheWay: settings.TradesOnTheWay, settleOffset: offset, venue: venue}, nil
}

// resolveSettlement returns the settlement date and the offset (when known) for
// a trade. An explicit settle date wins over the T+N computation, covering the
// case where the offset N is not controlled and only the date is known; the
// returned offset is then nil. The date is normalized to a UTC midnight so it
// matches a DATE column cleanly.
func (l *Blnk) resolveSettlement(ctx context.Context, params tradeParams, tradeDate, explicit time.Time) (time.Time, *int, error) {
	if !explicit.IsZero() {
		d := time.Date(explicit.Year(), explicit.Month(), explicit.Day(), 0, 0, 0, 0, time.UTC)
		return d, nil, nil
	}
	settleDate, err := l.ComputeSettleDate(ctx, params.venue, tradeDate, params.settleOffset)
	if err != nil {
		return time.Time{}, nil, err
	}
	code := params.settleOffset
	return settleDate, &code, nil
}

// GetTradablePosition computes how much of a security position can be sold by
// the given settle date, applying the TradeControl settle-date arithmetic and
// honoring the instrument's on-the-way flag. For an immediate-settlement
// instrument only the settled, unblocked position is tradable; for an
// on-the-way instrument the in-transit incoming/outgoing maturing by the
// settle date is netted in.
func (l *Blnk) GetTradablePosition(ctx context.Context, ledgerID, identityID, accountRef, instrument, currency string, asOfSettleDate time.Time) (*model.TradablePosition, error) {
	settings, err := l.datasource.GetInstrumentSettings(ctx, instrument)
	onTheWay := false
	if err == nil {
		onTheWay = settings.TradesOnTheWay
	} else if apiErr, ok := err.(apierror.APIError); !ok || apiErr.Code != apierror.ErrNotFound {
		return nil, err
	}

	settled := big.NewInt(0)
	blocked := big.NewInt(0)
	spot, err := l.datasource.GetPosition(ctx, model.PositionKey{
		LedgerID: ledgerID, IdentityID: identityID, AccountRef: accountRef,
		Instrument: instrument, Currency: currency,
	})
	if err == nil {
		spot.InitializeBalanceFields()
		settled = spot.Balance
		blocked = spot.InflightDebitBalance
	} else if apiErr, ok := err.(apierror.APIError); !ok || apiErr.Code != apierror.ErrNotFound {
		return nil, err
	}

	// Always fetch holds: outgoing (already-committed sells) must reduce
	// availability even for immediate-settlement instruments, otherwise a
	// second sell of the same settled position would be allowed. Incoming is
	// only counted (added) for on-the-way instruments — see ComputeTradable.
	incoming, outgoing, err := l.datasource.SumFutureHolds(ctx, ledgerID, identityID, accountRef, instrument, currency, asOfSettleDate)
	if err != nil {
		return nil, err
	}
	reportedIncoming := big.NewInt(0)
	if onTheWay {
		reportedIncoming = incoming
	}

	return &model.TradablePosition{
		Settled:    settled,
		Blocked:    blocked,
		Incoming:   reportedIncoming,
		Outgoing:   outgoing,
		Tradable:   model.ComputeTradable(settled, blocked, incoming, outgoing, onTheWay),
		OnTheWay:   onTheWay,
		AsOfSettle: asOfSettleDate.Format(model.HolidayKeyFormat),
	}, nil
}

// SellTrade books a sell trade after validating the settle-date-aware tradable
// quantity. The security leg places an inflight delivery from the client
// position to the market counterparty (reducing the tradable position) and the
// money leg places an inflight credit of the proceeds. For an on-the-way
// instrument the delivery is booked on the T+N future balance and may exceed
// the settled spot quantity (covered by in-transit incoming); for an
// immediate-settlement instrument only the settled position can be sold.
func (l *Blnk) SellTrade(ctx context.Context, booking model.SellBooking) (*model.SellBookingResult, error) {
	if err := booking.Validate(); err != nil {
		return nil, err
	}
	tradeDate := booking.TradeDate
	if tradeDate.IsZero() {
		tradeDate = time.Now()
	}

	params, err := l.resolveTradeParams(ctx, booking.Instrument, booking.SettleOffset, booking.Venue)
	if err != nil {
		return nil, err
	}
	settleDate, settleCode, err := l.resolveSettlement(ctx, params, tradeDate, booking.SettleDate)
	if err != nil {
		return nil, err
	}

	// Settle-date-aware availability check (the heart of the requirement).
	tradable, err := l.GetTradablePosition(ctx, booking.LedgerID, booking.IdentityID,
		booking.AccountRef, booking.Instrument, booking.Currency, settleDate)
	if err != nil {
		return nil, err
	}
	qtyPrecise, err := model.PreciseQuantity(booking.Quantity, booking.QuantityPrecision)
	if err != nil {
		return nil, apierror.NewAPIError(apierror.ErrBadRequest, err.Error(), err)
	}
	if qtyPrecise.Cmp(tradable.Tradable) > 0 {
		metrics.BrokerageSellRejectedTotal.Add(ctx, 1, otelmetric.WithAttributes(
			attribute.String("reason", "insufficient_tradable"),
			attribute.String("instrument", booking.Instrument)))
		return nil, apierror.NewAPIError(apierror.ErrBadRequest, fmt.Sprintf(
			"insufficient tradable quantity for %s: requested %s, available %s (settled %s, blocked %s, incoming %s, outgoing %s, on_the_way %t)",
			booking.Instrument, booking.Quantity, tradable.Tradable.String(),
			tradable.Settled.String(), tradable.Blocked.String(), tradable.Incoming.String(),
			tradable.Outgoing.String(), tradable.OnTheWay), nil)
	}

	moneyBalance, err := l.GetOrCreatePosition(ctx, model.PositionKey{
		LedgerID:   booking.LedgerID,
		IdentityID: booking.IdentityID,
		AccountRef: booking.AccountRef,
		Currency:   booking.Currency,
	})
	if err != nil {
		return nil, err
	}

	// The delivery is booked on the future bucket settling on settleDate, created
	// on the fly. The outflow hold may exceed the settled spot quantity;
	// AllowOverdraft is safe because the tradable check above already validated
	// the netted availability.
	deliverPosition, err := l.GetOrCreatePosition(ctx, model.PositionKey{
		LedgerID:   booking.LedgerID,
		IdentityID: booking.IdentityID,
		AccountRef: booking.AccountRef,
		Instrument: booking.Instrument,
		Currency:   booking.Currency,
		SettleDate: &settleDate,
		SettleCode: settleCode,
	})
	if err != nil {
		return nil, err
	}

	moneyPrecise, err := model.PreciseMoney(booking.Quantity, booking.Price, booking.MoneyPrecision)
	if err != nil {
		return nil, err
	}

	sharedMeta := func(leg string) map[string]interface{} {
		return map[string]interface{}{
			model.TradeMetaRef:        booking.Reference,
			model.TradeMetaLeg:        leg,
			model.TradeMetaSide:       model.TradeSideSell,
			model.TradeMetaInstrument: booking.Instrument,
			model.TradeMetaVenue:      params.venue,
			model.TradeMetaQuantity:   booking.Quantity,
			model.TradeMetaPrice:      booking.Price,
			model.TradeMetaSettleDate: settleDate.Format(model.HolidayKeyFormat),
		}
	}

	// Money leg: proceeds flow in from the broker settlement balance to the
	// client money balance. The settlement balance funds the payout, so the
	// outflow is allowed to overdraw it. Precise minor units keep float out.
	moneyTxn := &model.Transaction{
		TransactionID:  model.GenerateUUIDWithSuffix("txn"),
		Source:         booking.SettlementBalanceID,
		Destination:    moneyBalance.BalanceID,
		PreciseAmount:  moneyPrecise,
		Precision:      float64(booking.MoneyPrecision),
		Currency:       booking.Currency,
		Reference:      booking.Reference + "_money",
		Description:    fmt.Sprintf("Trade %s money leg: sell %s %s @ %s", booking.Reference, booking.Quantity, booking.Instrument, booking.Price),
		Inflight:       true,
		SkipQueue:      true,
		AllowOverdraft: true,
		MetaData:       sharedMeta(model.TradeMetaLegMoney),
	}
	recordedMoney, err := l.RecordTransaction(ctx, moneyTxn)
	if err != nil {
		return nil, fmt.Errorf("failed to book sell money leg: %w", err)
	}

	securityMeta := sharedMeta(model.TradeMetaLegSecurity)
	securityMeta[model.TradeMetaMoneyLegID] = recordedMoney.TransactionID
	securityTxn := &model.Transaction{
		TransactionID:  model.GenerateUUIDWithSuffix("txn"),
		Source:         deliverPosition.BalanceID,
		Destination:    booking.MarketBalanceID,
		PreciseAmount:  qtyPrecise,
		Precision:      float64(booking.QuantityPrecision),
		Currency:       booking.Currency,
		Reference:      booking.Reference + "_sec",
		Description:    fmt.Sprintf("Trade %s security leg: deliver %s %s (T+%d)", booking.Reference, booking.Quantity, booking.Instrument, params.settleOffset),
		Inflight:       true,
		SkipQueue:      true,
		AllowOverdraft: true, // covered by in-transit incoming, validated above
		MetaData:       securityMeta,
	}
	recordedSecurity, err := l.RecordTransaction(ctx, securityTxn)
	if err != nil {
		if _, voidErr := l.VoidInflightTransaction(ctx, recordedMoney.TransactionID); voidErr != nil {
			logrus.WithError(voidErr).WithField("transaction_id", recordedMoney.TransactionID).
				Error("failed to void sell money leg after security leg failure")
		}
		return nil, fmt.Errorf("failed to book sell security leg: %w", err)
	}

	metrics.BrokerageTradeBookedTotal.Add(ctx, 1, otelmetric.WithAttributes(
		attribute.String("side", model.TradeSideSell),
		attribute.String("instrument", booking.Instrument)))
	return &model.SellBookingResult{
		TradeRef:          booking.Reference,
		MoneyTxnID:        recordedMoney.TransactionID,
		SecurityTxnID:     recordedSecurity.TransactionID,
		MoneyBalanceID:    moneyBalance.BalanceID,
		PositionBalanceID: deliverPosition.BalanceID,
		SettleDate:        settleDate,
		Tradable:          *tradable,
	}, nil
}

// ApplyMutationPlan applies an atomic multi-position mutation plan
// (TradeControl BalanceMutationServiceImpl.apply analog):
//
//  1. deltas are normalized (merged per key, no-ops dropped) and ordered
//     deterministically;
//  2. distributed locks are taken on every key (sorted by MultiLocker —
//     deadlock guard);
//  3. all rows are mutated in one database transaction under
//     SELECT ... FOR UPDATE, creating missing positions on the fly;
//  4. locks are released after the transaction completes.
//
// This is the sanctioned escape hatch for reconciliation-style corrections;
// regular money movement must keep going through blnk transactions.
func (l *Blnk) ApplyMutationPlan(ctx context.Context, plan model.MutationPlan) error {
	if err := plan.Validate(); err != nil {
		return err
	}
	normalized := plan.Normalized()
	if len(normalized) == 0 {
		return nil
	}

	locker := redlock.NewMultiLocker(l.redis, plan.LockKeys(), model.GenerateUUIDWithSuffix("loc"))
	if err := locker.WaitLock(ctx, l.Config().Transaction.LockDuration, l.Config().Transaction.LockWaitTimeout); err != nil {
		return fmt.Errorf("failed to acquire mutation locks: %w", err)
	}
	defer func() {
		if err := locker.Unlock(ctx); err != nil {
			logrus.WithError(err).Error("failed to release mutation locks")
		}
	}()

	return l.datasource.ApplyBalanceDeltas(ctx, normalized)
}

// GetFreeBalance computes the operable amount of a balance with the
// TradeControl FreeBalanceAccountService thresholds: sum1 is the settled
// balance minus blocked holds (inflight debits), sum2 is the queued debit
// obligations, and the optional cap plays the role of Sum0.
func (l *Blnk) GetFreeBalance(ctx context.Context, balanceID string, cap *big.Int) (*model.FreeBalanceResult, error) {
	balance, err := l.datasource.GetBalanceByID(balanceID, nil, true)
	if err != nil {
		return &model.FreeBalanceResult{IsSuccess: false, ErrorMessage: err.Error()}, err
	}
	balance.InitializeBalanceFields()

	sum1 := new(big.Int).Sub(balance.Balance, balance.InflightDebitBalance)
	sum2 := balance.QueuedDebitBalance // nil-safe inside FreeBalance

	return &model.FreeBalanceResult{
		IsSuccess:       true,
		AvailableAmount: model.FreeBalance(cap, sum1, sum2),
		Currency:        balance.Currency,
	}, nil
}

// RecalculateHolds rebuilds the blocked/waiting holds of the given balances
// from their live INFLIGHT transactions (TradeControl
// recalculateBlockedWaitingAmount* analog): bounded parallelism with errors
// aggregated per balance instead of failing the whole pass.
func (l *Blnk) RecalculateHolds(ctx context.Context, balanceIDs []string) (*model.HoldsRecalcResult, error) {
	if len(balanceIDs) == 0 {
		return nil, fmt.Errorf("at least one balance id is required")
	}

	sem := semaphore.NewWeighted(holdsRecalcConcurrency)
	var mu sync.Mutex
	result := &model.HoldsRecalcResult{Errors: make(map[string]string)}

	for _, balanceID := range balanceIDs {
		if err := sem.Acquire(ctx, 1); err != nil {
			return nil, err
		}
		go func(id string) {
			defer sem.Release(1)
			_, _, err := l.datasource.RecomputeHolds(ctx, id)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				result.Errors[id] = err.Error()
				return
			}
			result.Recalculated = append(result.Recalculated, id)
		}(balanceID)
	}
	// Drain the semaphore: all workers have finished once every slot is held.
	if err := sem.Acquire(ctx, holdsRecalcConcurrency); err != nil {
		return nil, err
	}
	sem.Release(holdsRecalcConcurrency)

	if len(result.Errors) > 0 {
		logrus.WithField("failed", len(result.Errors)).
			WithField("recalculated", len(result.Recalculated)).
			Warn("holds recalculation finished with errors")
	}
	return result, nil
}

// BookTrade books a buy trade against the ledger (TradeControl
// recalculateBlockedWaitingForTrade booking semantics):
//
//   - the money leg places an inflight hold from the client money balance to
//     the broker settlement balance (blockedAmount goes up);
//   - the security leg places an inflight delivery from the market
//     counterparty balance to the client T+N future position, created on the
//     fly with the holiday-aware settle date (waitingAmount goes up).
//
// Both legs are linked through metadata and settle together via SettleTrade.
func (l *Blnk) BookTrade(ctx context.Context, booking model.TradeBooking) (*model.TradeBookingResult, error) {
	if err := booking.Validate(); err != nil {
		return nil, err
	}
	tradeDate := booking.TradeDate
	if tradeDate.IsZero() {
		tradeDate = time.Now()
	}

	params, err := l.resolveTradeParams(ctx, booking.Instrument, booking.SettleOffset, booking.Venue)
	if err != nil {
		return nil, err
	}
	settleDate, settleCode, err := l.resolveSettlement(ctx, params, tradeDate, booking.SettleDate)
	if err != nil {
		return nil, err
	}

	moneyBalance, err := l.GetOrCreatePosition(ctx, model.PositionKey{
		LedgerID:   booking.LedgerID,
		IdentityID: booking.IdentityID,
		AccountRef: booking.AccountRef,
		Currency:   booking.Currency,
	})
	if err != nil {
		return nil, err
	}

	futurePosition, err := l.GetOrCreatePosition(ctx, model.PositionKey{
		LedgerID:   booking.LedgerID,
		IdentityID: booking.IdentityID,
		AccountRef: booking.AccountRef,
		Instrument: booking.Instrument,
		Currency:   booking.Currency,
		SettleDate: &settleDate,
		SettleCode: settleCode,
	})
	if err != nil {
		return nil, err
	}

	qtyPrecise, err := model.PreciseQuantity(booking.Quantity, booking.QuantityPrecision)
	if err != nil {
		return nil, apierror.NewAPIError(apierror.ErrBadRequest, err.Error(), err)
	}
	moneyPrecise, err := model.PreciseMoney(booking.Quantity, booking.Price, booking.MoneyPrecision)
	if err != nil {
		return nil, err
	}

	sharedMeta := func(leg string) map[string]interface{} {
		return map[string]interface{}{
			model.TradeMetaRef:        booking.Reference,
			model.TradeMetaLeg:        leg,
			model.TradeMetaSide:       model.TradeSideBuy,
			model.TradeMetaInstrument: booking.Instrument,
			model.TradeMetaVenue:      params.venue,
			model.TradeMetaQuantity:   booking.Quantity,
			model.TradeMetaPrice:      booking.Price,
			model.TradeMetaSettleDate: settleDate.Format(model.HolidayKeyFormat),
		}
	}

	moneyTxn := &model.Transaction{
		TransactionID: model.GenerateUUIDWithSuffix("txn"),
		Source:        moneyBalance.BalanceID,
		Destination:   booking.SettlementBalanceID,
		PreciseAmount: moneyPrecise,
		Precision:     float64(booking.MoneyPrecision),
		Currency:      booking.Currency,
		Reference:     booking.Reference + "_money",
		Description:   fmt.Sprintf("Trade %s money leg: buy %s %s @ %s", booking.Reference, booking.Quantity, booking.Instrument, booking.Price),
		Inflight:      true,
		SkipQueue:     true,
		MetaData:      sharedMeta(model.TradeMetaLegMoney),
	}
	recordedMoney, err := l.RecordTransaction(ctx, moneyTxn)
	if err != nil {
		return nil, fmt.Errorf("failed to book money leg: %w", err)
	}

	securityMeta := sharedMeta(model.TradeMetaLegSecurity)
	securityMeta[model.TradeMetaMoneyLegID] = recordedMoney.TransactionID
	securityTxn := &model.Transaction{
		TransactionID:  model.GenerateUUIDWithSuffix("txn"),
		Source:         booking.MarketBalanceID,
		Destination:    futurePosition.BalanceID,
		PreciseAmount:  qtyPrecise,
		Precision:      float64(booking.QuantityPrecision),
		Currency:       booking.Currency,
		Reference:      booking.Reference + "_sec",
		Description:    fmt.Sprintf("Trade %s security leg: deliver %s %s (T+%d)", booking.Reference, booking.Quantity, booking.Instrument, params.settleOffset),
		Inflight:       true,
		SkipQueue:      true,
		AllowOverdraft: true, // the market counterparty balance may go short
		MetaData:       securityMeta,
	}
	recordedSecurity, err := l.RecordTransaction(ctx, securityTxn)
	if err != nil {
		// Compensate the money hold so a failed booking leaves no dangling block.
		if _, voidErr := l.VoidInflightTransaction(ctx, recordedMoney.TransactionID); voidErr != nil {
			logrus.WithError(voidErr).WithField("transaction_id", recordedMoney.TransactionID).
				Error("failed to void money leg after security leg failure")
		}
		return nil, fmt.Errorf("failed to book security leg: %w", err)
	}

	metrics.BrokerageTradeBookedTotal.Add(ctx, 1, otelmetric.WithAttributes(
		attribute.String("side", model.TradeSideBuy),
		attribute.String("instrument", booking.Instrument)))
	return &model.TradeBookingResult{
		TradeRef:          booking.Reference,
		MoneyTxnID:        recordedMoney.TransactionID,
		SecurityTxnID:     recordedSecurity.TransactionID,
		MoneyBalanceID:    moneyBalance.BalanceID,
		PositionBalanceID: futurePosition.BalanceID,
		SettleDate:        settleDate,
	}, nil
}

// metaString extracts a string value from transaction metadata.
func metaString(meta map[string]interface{}, key string) string {
	if meta == nil {
		return ""
	}
	if value, ok := meta[key].(string); ok {
		return value
	}
	return ""
}

// SettleTrade settles the future balance bucket that holds a given trade's
// security leg. All trades sharing that settle date/instrument/account settle
// together: their money and security holds are committed, buys refresh the
// weighted-average price and record a lot, and the net delivered/received
// quantity is rolled to the spot position with a plain double-entry transaction
// (TradeControl trade settlement + recalculateWawPrice + BalanceDetail).
func (l *Blnk) SettleTrade(ctx context.Context, securityTxnID string) (*model.TradeSettlementResult, error) {
	securityTxn, err := l.datasource.GetTransaction(ctx, securityTxnID)
	if err != nil {
		return nil, err
	}
	if metaString(securityTxn.MetaData, model.TradeMetaLeg) != model.TradeMetaLegSecurity {
		return nil, fmt.Errorf("transaction %s is not a trade security leg", securityTxnID)
	}
	// The future balance is the destination for buys and the source for sells.
	futureBalanceID := securityTxn.Destination
	if metaString(securityTxn.MetaData, model.TradeMetaSide) == model.TradeSideSell {
		futureBalanceID = securityTxn.Source
	}

	settled, err := l.settleFutureBalance(ctx, futureBalanceID)
	if err != nil {
		return nil, err
	}
	for i := range settled {
		if settled[i].SecurityTxnID == securityTxnID {
			return &settled[i], nil
		}
	}
	// The bucket settled but did not contain a residual to report for this leg.
	return &model.TradeSettlementResult{TradeRef: metaString(securityTxn.MetaData, model.TradeMetaRef), SecurityTxnID: securityTxnID, FutureBalanceID: futureBalanceID}, nil
}

// settleFutureBalance settles every pending trade leg on a future balance,
// then nets the resulting position to spot in a single roll. Buys are
// committed before sells so the weighted-average blend sees the pre-sale
// quantity, and the net roll direction follows the sign of the settled
// quantity (incoming -> future->spot, outgoing -> spot->future), leaving the
// future balance at zero.
func (l *Blnk) settleFutureBalance(ctx context.Context, futureBalanceID string) ([]model.TradeSettlementResult, error) {
	future, err := l.datasource.GetPositionByID(ctx, futureBalanceID)
	if err != nil {
		return nil, err
	}
	if future.AccountRef == "" || future.Instrument == "" {
		return nil, fmt.Errorf("balance %s is not a brokerage security position", futureBalanceID)
	}
	spot, err := l.GetOrCreatePosition(ctx, model.PositionKey{
		LedgerID:   future.LedgerID,
		IdentityID: future.IdentityID,
		AccountRef: future.AccountRef,
		Instrument: future.Instrument,
		Currency:   future.Currency,
	})
	if err != nil {
		return nil, err
	}

	legs, err := l.datasource.GetPendingInflightByBalance(ctx, futureBalanceID)
	if err != nil {
		return nil, err
	}
	// Keep only security legs and order buys (incoming) before sells (outgoing).
	var buys, sells []*model.Transaction
	for _, leg := range legs {
		if metaString(leg.MetaData, model.TradeMetaLeg) != model.TradeMetaLegSecurity {
			continue
		}
		if metaString(leg.MetaData, model.TradeMetaSide) == model.TradeSideSell {
			sells = append(sells, leg)
		} else {
			buys = append(buys, leg)
		}
	}

	var results []model.TradeSettlementResult
	// addedPrecise tracks buy quantity committed in this run so the WA blend of a
	// second buy accounts for earlier buys not yet rolled into spot.
	addedPrecise := big.NewInt(0)
	for _, leg := range buys {
		res, qty, err := l.settleSecurityLeg(ctx, leg, spot, future, addedPrecise)
		if err != nil {
			return nil, err
		}
		addedPrecise = new(big.Int).Add(addedPrecise, qty)
		results = append(results, *res)
	}
	for _, leg := range sells {
		res, _, err := l.settleSecurityLeg(ctx, leg, spot, future, addedPrecise)
		if err != nil {
			return nil, err
		}
		results = append(results, *res)
	}

	// Net roll: move the future balance's settled quantity to spot and zero it.
	if err := l.netRollToSpot(ctx, futureBalanceID, spot.BalanceID); err != nil {
		return nil, err
	}
	return results, nil
}

// settleSecurityLeg commits one trade's money and security holds. For a buy it
// refreshes the weighted-average price and records a lot **through the
// settlement journal**, so the side effects are atomic and recoverable: a
// pending journal row capturing wa_before/qty_before is written before the
// commits, and the lot + wa_price + 'applied' flip happen in one DB transaction
// afterwards. A crash in between leaves a pending row that ReconcileSettlement
// finishes. Returns the bought quantity in precise units (zero for sells).
func (l *Blnk) settleSecurityLeg(ctx context.Context, securityTxn *model.Transaction, spot, future *model.Balance, addedPrecise *big.Int) (*model.TradeSettlementResult, *big.Int, error) {
	tradeRef := metaString(securityTxn.MetaData, model.TradeMetaRef)
	isSell := metaString(securityTxn.MetaData, model.TradeMetaSide) == model.TradeSideSell
	result := &model.TradeSettlementResult{
		TradeRef:        tradeRef,
		SecurityTxnID:   securityTxn.TransactionID,
		FutureBalanceID: future.BalanceID,
		SpotBalanceID:   spot.BalanceID,
	}

	zero := big.NewInt(0)
	priceStr := metaString(securityTxn.MetaData, model.TradeMetaPrice)
	hasSideEffects := !isSell && securityTxn.Precision >= 1 && priceStr != ""

	// Phase 1: record a pending journal capturing the inputs needed to make
	// wa_after deterministic on recovery (before any commit).
	if hasSideEffects {
		qtyBefore := new(big.Int).Add(spot.Balance, addedPrecise)
		_, _, err := l.datasource.BeginSettlementJournal(ctx, model.SettlementJournalEntry{
			SecurityTxnID: securityTxn.TransactionID,
			TradeRef:      tradeRef,
			SpotBalanceID: spot.BalanceID,
			Instrument:    future.Instrument,
			Side:          model.TradeSideBuy,
			WABefore:      spot.WAPrice,
			QtyBefore:     qtyBefore.String(),
			Price:         priceStr,
			Quantity:      securityTxn.PreciseAmount.String(),
			Precision:     int64(securityTxn.Precision),
			Currency:      securityTxn.Currency,
		})
		if err != nil {
			return nil, zero, err
		}
	}

	// Commit money + security holds.
	if moneyLegID := metaString(securityTxn.MetaData, model.TradeMetaMoneyLegID); moneyLegID != "" {
		if _, err := l.CommitInflightTransaction(ctx, moneyLegID, big.NewInt(0)); err != nil {
			return nil, zero, fmt.Errorf("failed to commit money leg %s: %w", moneyLegID, err)
		}
		result.MoneyTxnID = moneyLegID
	}
	if _, err := l.CommitInflightTransaction(ctx, securityTxn.TransactionID, big.NewInt(0)); err != nil {
		return nil, zero, fmt.Errorf("failed to commit security leg %s: %w", securityTxn.TransactionID, err)
	}

	if !hasSideEffects {
		return result, zero, nil
	}

	// Phase 2: apply WA + lot + journal atomically.
	price, err := decimal.NewFromString(priceStr)
	if err != nil {
		logrus.WithError(err).WithField("trade_ref", tradeRef).Warn("invalid trade price in metadata, skipping wa price update")
		return result, zero, nil
	}
	qtyBefore := new(big.Int).Add(spot.Balance, addedPrecise)
	waAfter, err := computeWAAfter(spot.WAPrice, qtyBefore, securityTxn.PreciseAmount, securityTxn.Precision, price)
	if err != nil {
		return nil, zero, fmt.Errorf("failed to recalculate wa price: %w", err)
	}
	lotID, err := l.datasource.CompleteSettlementJournal(ctx, securityTxn.TransactionID, waAfter, model.BalanceLot{
		BalanceID:  spot.BalanceID,
		Instrument: future.Instrument,
		Quantity:   new(big.Int).Set(securityTxn.PreciseAmount),
		Precision:  int64(securityTxn.Precision),
		Price:      price.String(),
		Currency:   securityTxn.Currency,
		Reference:  tradeRef,
	})
	if err != nil {
		return nil, zero, err
	}
	spot.WAPrice = waAfter // keep the in-memory snapshot consistent for the next buy
	result.WAPrice = waAfter
	result.LotID = lotID
	return result, new(big.Int).Set(securityTxn.PreciseAmount), nil
}

// computeWAAfter deterministically blends the weighted-average price from the
// journal inputs (all in precise units).
func computeWAAfter(waBefore string, qtyBeforePrecise, tradeQtyPrecise *big.Int, precision float64, price decimal.Decimal) (string, error) {
	qtyPrecision := decimal.NewFromFloat(precision)
	currentQty := decimal.NewFromBigInt(qtyBeforePrecise, 0).Div(qtyPrecision)
	tradeQty := decimal.NewFromBigInt(tradeQtyPrecise, 0).Div(qtyPrecision)
	tradeMoney := tradeQty.Mul(price)
	wa, err := model.RecalculateWAPrice(waBefore, currentQty, tradeQty, tradeMoney)
	if err != nil {
		return "", err
	}
	return wa.StringFixed(2), nil
}

// ReconcileSettlement completes settlement journal rows whose side effects
// (wa_price + lot) did not finish — e.g. a crash between committing the inflight
// legs and writing the side effects. The journal captured wa_before/qty_before,
// so wa_after is recomputed deterministically and applied idempotently. Returns
// the number of journals reconciled.
func (l *Blnk) ReconcileSettlement(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = defaultSettlementBatch
	}
	pending, err := l.datasource.ListPendingSettlementJournals(ctx, limit)
	if err != nil {
		return 0, err
	}
	reconciled := 0
	for _, e := range pending {
		price, err := decimal.NewFromString(e.Price)
		if err != nil {
			logrus.WithError(err).WithField("security_txn_id", e.SecurityTxnID).Warn("invalid price in settlement journal, skipping")
			continue
		}
		qtyBefore, ok := new(big.Int).SetString(e.QtyBefore, 10)
		if !ok {
			qtyBefore = big.NewInt(0)
		}
		tradeQty, ok := new(big.Int).SetString(e.Quantity, 10)
		if !ok {
			logrus.WithField("security_txn_id", e.SecurityTxnID).Warn("invalid quantity in settlement journal, skipping")
			continue
		}
		waAfter, err := computeWAAfter(e.WABefore, qtyBefore, tradeQty, float64(e.Precision), price)
		if err != nil {
			logrus.WithError(err).WithField("security_txn_id", e.SecurityTxnID).Warn("wa recompute failed, skipping")
			continue
		}
		if _, err := l.datasource.CompleteSettlementJournal(ctx, e.SecurityTxnID, waAfter, model.BalanceLot{
			BalanceID:  e.SpotBalanceID,
			Instrument: e.Instrument,
			Quantity:   tradeQty,
			Precision:  e.Precision,
			Price:      price.String(),
			Currency:   e.Currency,
			Reference:  e.TradeRef,
		}); err != nil {
			logrus.WithError(err).WithField("security_txn_id", e.SecurityTxnID).Error("failed to reconcile settlement journal")
			continue
		}
		reconciled++
	}
	if reconciled > 0 {
		metrics.BrokerageReconciledTotal.Add(ctx, int64(reconciled))
	}
	return reconciled, nil
}

// netRollToSpot moves the settled quantity left on a future balance to its spot
// counterpart and zeroes the future balance. A positive net (incoming) rolls
// future->spot, a negative net (outgoing) rolls spot->future; overdraft is
// allowed because the net economics were validated at booking time.
func (l *Blnk) netRollToSpot(ctx context.Context, futureBalanceID, spotBalanceID string) error {
	future, err := l.datasource.GetPositionByID(ctx, futureBalanceID)
	if err != nil {
		return err
	}
	future.InitializeBalanceFields()
	net := future.Balance
	if net.Sign() == 0 {
		return nil
	}

	source, destination := futureBalanceID, spotBalanceID
	amount := new(big.Int).Set(net)
	if net.Sign() < 0 {
		source, destination = spotBalanceID, futureBalanceID
		amount = new(big.Int).Neg(net)
	}

	_, err = l.RecordTransaction(ctx, &model.Transaction{
		TransactionID:  model.GenerateUUIDWithSuffix("txn"),
		Source:         source,
		Destination:    destination,
		PreciseAmount:  amount,
		Precision:      1,
		Currency:       future.Currency,
		Reference:      model.GenerateUUIDWithSuffix("roll"),
		Description:    "Settlement net roll T+N <-> spot",
		SkipQueue:      true,
		AllowOverdraft: true,
		MetaData:       map[string]interface{}{model.TradeMetaLeg: "roll"},
	})
	return err
}

// RunSettlement settles everything that matured by asOf
// (TradeControl settlement pass): finds future balances whose settle date has
// been reached and settles each bucket (commit holds, refresh WA, net roll to
// spot). Failures are aggregated per balance, mirroring the TradeControl error
// aggregation, so one broken bucket does not block the run.
func (l *Blnk) RunSettlement(ctx context.Context, asOf time.Time, limit int) (*model.SettlementRunResult, error) {
	if limit <= 0 {
		limit = defaultSettlementBatch
	}
	start := time.Now()
	metrics.BrokerageSettlementRunTotal.Add(ctx, 1)
	// Finish any side effects left pending by a previously interrupted run.
	if n, err := l.ReconcileSettlement(ctx, limit); err != nil {
		logrus.WithError(err).Warn("settlement reconcile pass failed")
	} else if n > 0 {
		logrus.WithField("reconciled", n).Info("recovered pending settlement journals")
	}
	matured, err := l.datasource.GetMaturedPositions(ctx, asOf, limit)
	if err != nil {
		return nil, err
	}

	result := &model.SettlementRunResult{AsOf: asOf, Errors: make(map[string]string)}
	for _, position := range matured {
		result.Examined++
		settled, err := l.settleFutureBalance(ctx, position.BalanceID)
		if err != nil {
			result.Errors[position.BalanceID] = err.Error()
			continue
		}
		result.Settled = append(result.Settled, settled...)
	}

	metrics.BrokerageMaturedBucketsExamined.Record(ctx, int64(result.Examined))
	metrics.BrokerageSettledTradesTotal.Add(ctx, int64(len(result.Settled)))
	if len(result.Errors) > 0 {
		metrics.BrokerageSettlementErrorsTotal.Add(ctx, int64(len(result.Errors)))
	}
	metrics.BrokerageSettlementDuration.Record(ctx, time.Since(start).Seconds())
	return result, nil
}
