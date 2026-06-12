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

	redlock "github.com/blnkfinance/blnk/internal/lock"
	"github.com/blnkfinance/blnk/model"
	"github.com/shopspring/decimal"
	"github.com/sirupsen/logrus"
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
// when missing. settleDate is required for future (T+N) keys.
func (l *Blnk) GetOrCreatePosition(ctx context.Context, key model.PositionKey, settleDate *time.Time) (*model.Balance, error) {
	return l.datasource.FindOrCreatePosition(ctx, key, settleDate)
}

// GetActivePosition resolves the active balance for the dimensions with the
// TradeControl settle cascade (requested T+N falls back through lower codes
// down to the spot balance).
func (l *Blnk) GetActivePosition(ctx context.Context, ledgerID, identityID, accountRef, instrument, currency string, maxSettleCode *int) (*model.Balance, error) {
	return l.datasource.GetActivePosition(ctx, ledgerID, identityID, accountRef, instrument, currency, maxSettleCode)
}

// GetBalanceLots lists the purchase lots of a balance (BalanceDetail analog).
func (l *Blnk) GetBalanceLots(ctx context.Context, balanceID string) ([]model.BalanceLot, error) {
	return l.datasource.GetLots(ctx, balanceID)
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

	settleDate, err := l.ComputeSettleDate(ctx, booking.Venue, tradeDate, booking.SettleOffset)
	if err != nil {
		return nil, err
	}

	moneyBalance, err := l.GetOrCreatePosition(ctx, model.PositionKey{
		LedgerID:   booking.LedgerID,
		IdentityID: booking.IdentityID,
		AccountRef: booking.AccountRef,
		Currency:   booking.Currency,
	}, nil)
	if err != nil {
		return nil, err
	}

	settleCode := booking.SettleOffset
	futurePosition, err := l.GetOrCreatePosition(ctx, model.PositionKey{
		LedgerID:   booking.LedgerID,
		IdentityID: booking.IdentityID,
		AccountRef: booking.AccountRef,
		Instrument: booking.Instrument,
		Currency:   booking.Currency,
		SettleCode: &settleCode,
	}, &settleDate)
	if err != nil {
		return nil, err
	}

	price, err := decimal.NewFromString(booking.Price)
	if err != nil {
		return nil, fmt.Errorf("invalid price %q: %w", booking.Price, err)
	}
	money := price.Mul(decimal.NewFromFloat(booking.Quantity))

	sharedMeta := func(leg string) map[string]interface{} {
		return map[string]interface{}{
			model.TradeMetaRef:        booking.Reference,
			model.TradeMetaLeg:        leg,
			model.TradeMetaInstrument: booking.Instrument,
			model.TradeMetaVenue:      booking.Venue,
			model.TradeMetaQuantity:   booking.Quantity,
			model.TradeMetaPrice:      booking.Price,
			model.TradeMetaSettleDate: settleDate.Format(model.HolidayKeyFormat),
		}
	}

	moneyTxn := &model.Transaction{
		Source:      moneyBalance.BalanceID,
		Destination: booking.SettlementBalanceID,
		Amount:      money.InexactFloat64(),
		Precision:   booking.MoneyPrecision,
		Currency:    booking.Currency,
		Reference:   booking.Reference + "_money",
		Description: fmt.Sprintf("Trade %s money leg: buy %v %s @ %s", booking.Reference, booking.Quantity, booking.Instrument, booking.Price),
		Inflight:    true,
		SkipQueue:   true,
		MetaData:    sharedMeta(model.TradeMetaLegMoney),
	}
	recordedMoney, err := l.RecordTransaction(ctx, moneyTxn)
	if err != nil {
		return nil, fmt.Errorf("failed to book money leg: %w", err)
	}

	securityMeta := sharedMeta(model.TradeMetaLegSecurity)
	securityMeta[model.TradeMetaMoneyLegID] = recordedMoney.TransactionID
	securityTxn := &model.Transaction{
		Source:         booking.MarketBalanceID,
		Destination:    futurePosition.BalanceID,
		Amount:         booking.Quantity,
		Precision:      booking.QuantityPrecision,
		Currency:       booking.Currency,
		Reference:      booking.Reference + "_sec",
		Description:    fmt.Sprintf("Trade %s security leg: deliver %v %s (T+%d)", booking.Reference, booking.Quantity, booking.Instrument, booking.SettleOffset),
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

// SettleTrade settles one booked trade by its security leg transaction ID:
// commits the money hold, commits the security delivery into the T+N future
// position, rolls the position to spot with a plain double-entry transaction
// and refreshes the weighted-average price plus the purchase lot
// (TradeControl trade settlement + recalculateWawPrice + BalanceDetail).
func (l *Blnk) SettleTrade(ctx context.Context, securityTxnID string) (*model.TradeSettlementResult, error) {
	securityTxn, err := l.datasource.GetTransaction(ctx, securityTxnID)
	if err != nil {
		return nil, err
	}
	if metaString(securityTxn.MetaData, model.TradeMetaLeg) != model.TradeMetaLegSecurity {
		return nil, fmt.Errorf("transaction %s is not a trade security leg", securityTxnID)
	}

	tradeRef := metaString(securityTxn.MetaData, model.TradeMetaRef)
	result := &model.TradeSettlementResult{
		TradeRef:        tradeRef,
		SecurityTxnID:   securityTxnID,
		FutureBalanceID: securityTxn.Destination,
	}

	// 1. Commit the money hold (full remaining amount).
	if moneyLegID := metaString(securityTxn.MetaData, model.TradeMetaMoneyLegID); moneyLegID != "" {
		if _, err := l.CommitInflightTransaction(ctx, moneyLegID, big.NewInt(0)); err != nil {
			return nil, fmt.Errorf("failed to commit money leg %s: %w", moneyLegID, err)
		}
		result.MoneyTxnID = moneyLegID
	}

	// 2. Commit the security delivery into the future position.
	if _, err := l.CommitInflightTransaction(ctx, securityTxnID, big.NewInt(0)); err != nil {
		return nil, fmt.Errorf("failed to commit security leg %s: %w", securityTxnID, err)
	}

	// 3. Resolve the future position and its spot counterpart.
	future, err := l.datasource.GetPositionByID(ctx, securityTxn.Destination)
	if err != nil {
		return nil, err
	}
	if future.AccountRef == "" || future.Instrument == "" {
		return nil, fmt.Errorf("balance %s is not a brokerage security position", future.BalanceID)
	}
	spot, err := l.GetOrCreatePosition(ctx, model.PositionKey{
		LedgerID:   future.LedgerID,
		IdentityID: future.IdentityID,
		AccountRef: future.AccountRef,
		Instrument: future.Instrument,
		Currency:   future.Currency,
	}, nil)
	if err != nil {
		return nil, err
	}
	result.SpotBalanceID = spot.BalanceID

	// 4. Roll the delivered quantity from the future balance to spot —
	//    plain double-entry, exact via the precise amount.
	quantity := securityTxn.PreciseAmount
	rollTxn := &model.Transaction{
		Source:        future.BalanceID,
		Destination:   spot.BalanceID,
		PreciseAmount: quantity,
		Precision:     securityTxn.Precision,
		Currency:      securityTxn.Currency,
		Reference:     securityTxn.Reference + "_roll",
		Description:   fmt.Sprintf("Trade %s settlement roll T+N -> spot", tradeRef),
		SkipQueue:     true,
		MetaData: map[string]interface{}{
			model.TradeMetaRef: tradeRef,
			model.TradeMetaLeg: "roll",
		},
	}
	recordedRoll, err := l.RecordTransaction(ctx, rollTxn)
	if err != nil {
		return nil, fmt.Errorf("failed to roll position to spot: %w", err)
	}
	result.RollTxnID = recordedRoll.TransactionID

	// 5. Refresh the weighted-average price and record the purchase lot.
	//    spot still holds the pre-roll quantity snapshot, which is exactly
	//    qty_old in the TradeControl formula.
	priceStr := metaString(securityTxn.MetaData, model.TradeMetaPrice)
	if priceStr != "" && securityTxn.Precision >= 1 {
		price, perr := decimal.NewFromString(priceStr)
		if perr != nil {
			logrus.WithError(perr).WithField("trade_ref", tradeRef).Warn("invalid trade price in metadata, skipping wa price update")
			return result, nil
		}
		qtyPrecision := decimal.NewFromFloat(securityTxn.Precision)
		currentQty := decimal.NewFromBigInt(spot.Balance, 0).Div(qtyPrecision)
		tradeQty := decimal.NewFromBigInt(quantity, 0).Div(qtyPrecision)
		tradeMoney := tradeQty.Mul(price)

		newWA, werr := model.RecalculateWAPrice(spot.WAPrice, currentQty, tradeQty, tradeMoney)
		if werr != nil {
			return nil, fmt.Errorf("failed to recalculate wa price: %w", werr)
		}
		if err := l.datasource.UpdateWAPrice(ctx, spot.BalanceID, newWA.StringFixed(2)); err != nil {
			return nil, err
		}
		result.WAPrice = newWA.StringFixed(2)

		lot, lerr := l.datasource.CreateLot(ctx, model.BalanceLot{
			BalanceID:   spot.BalanceID,
			Instrument:  future.Instrument,
			Quantity:    quantity,
			Precision:   int64(securityTxn.Precision),
			Price:       price.String(),
			Currency:    securityTxn.Currency,
			Reference:   tradeRef,
			PurchasedAt: time.Now(),
		})
		if lerr != nil {
			return nil, lerr
		}
		result.LotID = lot.LotID
	}

	return result, nil
}

// RunSettlement settles everything that matured by asOf
// (TradeControl settlement pass): finds future balances whose settle date has
// been reached, settles their pending trade legs and rolls any residual
// settled quantity to spot (crash recovery for interrupted settlements).
// Failures are aggregated per artifact, mirroring the TradeControl error
// aggregation, so one broken trade does not block the run.
func (l *Blnk) RunSettlement(ctx context.Context, asOf time.Time, limit int) (*model.SettlementRunResult, error) {
	if limit <= 0 {
		limit = defaultSettlementBatch
	}
	matured, err := l.datasource.GetMaturedPositions(ctx, asOf, limit)
	if err != nil {
		return nil, err
	}

	result := &model.SettlementRunResult{AsOf: asOf, Errors: make(map[string]string)}
	for _, position := range matured {
		result.Examined++

		pending, err := l.datasource.GetPendingInflightByDestination(ctx, position.BalanceID)
		if err != nil {
			result.Errors[position.BalanceID] = err.Error()
			continue
		}
		for _, txn := range pending {
			if metaString(txn.MetaData, model.TradeMetaLeg) != model.TradeMetaLegSecurity {
				continue
			}
			settled, err := l.SettleTrade(ctx, txn.TransactionID)
			if err != nil {
				result.Errors[txn.TransactionID] = err.Error()
				continue
			}
			result.Settled = append(result.Settled, *settled)
		}

		// Crash recovery: a settled-but-unrolled remainder is rolled to spot.
		if err := l.rollResidualToSpot(ctx, position.BalanceID); err != nil {
			result.Errors[position.BalanceID] = err.Error()
		}
	}
	return result, nil
}

// rollResidualToSpot moves any settled quantity left on a matured future
// balance to its spot counterpart.
func (l *Blnk) rollResidualToSpot(ctx context.Context, futureBalanceID string) error {
	future, err := l.datasource.GetPositionByID(ctx, futureBalanceID)
	if err != nil {
		return err
	}
	if future.Balance.Sign() <= 0 || future.AccountRef == "" {
		return nil
	}
	spot, err := l.GetOrCreatePosition(ctx, model.PositionKey{
		LedgerID:   future.LedgerID,
		IdentityID: future.IdentityID,
		AccountRef: future.AccountRef,
		Instrument: future.Instrument,
		Currency:   future.Currency,
	}, nil)
	if err != nil {
		return err
	}

	_, err = l.RecordTransaction(ctx, &model.Transaction{
		Source:        future.BalanceID,
		Destination:   spot.BalanceID,
		PreciseAmount: new(big.Int).Set(future.Balance),
		Precision:     1,
		Currency:      future.Currency,
		Reference:     model.GenerateUUIDWithSuffix("roll"),
		Description:   "Residual settlement roll T+N -> spot",
		SkipQueue:     true,
		MetaData:      map[string]interface{}{model.TradeMetaLeg: "roll"},
	})
	return err
}
