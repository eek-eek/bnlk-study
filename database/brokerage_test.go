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
package database

import (
	"context"
	"math/big"
	"os"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/blnkfinance/blnk/internal/apierror"
	"github.com/blnkfinance/blnk/model"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
)

// positionRows builds a sqlmock row set matching positionSelectColumns.
func positionRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"balance_id", "balance", "credit_balance", "debit_balance",
		"inflight_balance", "inflight_credit_balance", "inflight_debit_balance",
		"currency", "ledger_id", "identity_id", "created_at", "version",
		"account_ref", "instrument", "settle_date", "settle_code", "wa_price",
	})
}

func TestGetActivePosition_CascadePicksLatestByDate(t *testing.T) {
	db, mock, err := sqlmock.New()
	assert.NoError(t, err)
	defer func() { _ = db.Close() }()
	ds := Datasource{Conn: db}

	settleDate := time.Date(2026, 6, 16, 0, 0, 0, 0, time.UTC)
	rows := positionRows().AddRow(
		"bln_future", "100", "100", "0", "0", "0", "0",
		"KZT", "ldg1", "idn1", time.Now(), 1,
		"acc1", "KZAP", settleDate, 2, "19000.00",
	)

	// The cascade orders by absolute settle date descending and limits to one row.
	maxDate := time.Date(2026, 6, 16, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`ORDER BY b.settle_date DESC NULLS LAST`).
		WithArgs("ldg1", "idn1", "acc1", "KZAP", "KZT", maxDate).
		WillReturnRows(rows)

	balance, err := ds.GetActivePosition(context.Background(), "ldg1", "idn1", "acc1", "KZAP", "KZT", &maxDate)
	assert.NoError(t, err)
	assert.Equal(t, "bln_future", balance.BalanceID)
	assert.Equal(t, "acc1", balance.AccountRef)
	assert.Equal(t, "KZAP", balance.Instrument)
	assert.NotNil(t, balance.SettleDate)
	assert.Equal(t, "19000.00", balance.WAPrice)
	assert.Equal(t, big.NewInt(100), balance.Balance)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestGetActivePosition_SpotLookupUsesNullDate(t *testing.T) {
	db, mock, err := sqlmock.New()
	assert.NoError(t, err)
	defer func() { _ = db.Close() }()
	ds := Datasource{Conn: db}

	rows := positionRows().AddRow(
		"bln_spot", "500", "500", "0", "0", "0", "0",
		"KZT", "ldg1", "idn1", time.Now(), 1,
		"acc1", "", nil, nil, nil,
	)
	// nil maxSettleDate restricts the lookup to the spot row (settle_date IS NULL).
	mock.ExpectQuery(`b.settle_date IS NULL`).
		WithArgs("ldg1", "idn1", "acc1", "", "KZT").
		WillReturnRows(rows)

	balance, err := ds.GetActivePosition(context.Background(), "ldg1", "idn1", "acc1", "", "KZT", nil)
	assert.NoError(t, err)
	assert.Equal(t, "bln_spot", balance.BalanceID)
	assert.Nil(t, balance.SettleCode)
	assert.Nil(t, balance.SettleDate)
	assert.Empty(t, balance.WAPrice)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestGetActivePosition_NotFound(t *testing.T) {
	db, mock, err := sqlmock.New()
	assert.NoError(t, err)
	defer func() { _ = db.Close() }()
	ds := Datasource{Conn: db}

	maxDate := time.Date(2026, 6, 16, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`ORDER BY b.settle_date DESC NULLS LAST`).
		WillReturnRows(positionRows())

	_, err = ds.GetActivePosition(context.Background(), "ldg1", "idn1", "acc1", "KZAP", "KZT", &maxDate)
	assert.Error(t, err)
	apiErr, ok := err.(apierror.APIError)
	assert.True(t, ok)
	assert.Equal(t, apierror.ErrNotFound, apiErr.Code)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestApplyBalanceDeltas_AppliesAmountAndHolds(t *testing.T) {
	db, mock, err := sqlmock.New()
	assert.NoError(t, err)
	defer func() { _ = db.Close() }()
	ds := Datasource{Conn: db}

	key := model.PositionKey{LedgerID: "ldg1", IdentityID: "idn1", AccountRef: "acc1", Currency: "KZT"}
	delta := model.BalanceDelta{
		Key:          key,
		AmountDelta:  big.NewInt(-300), // debit
		BlockedDelta: big.NewInt(200),
		WaitingDelta: big.NewInt(100),
	}

	mock.ExpectBegin()
	mock.ExpectQuery(`FOR UPDATE`).
		WithArgs("ldg1", "idn1", "acc1", "", "KZT", nil).
		WillReturnRows(sqlmock.NewRows([]string{
			"balance_id", "credit_balance", "debit_balance", "inflight_credit_balance", "inflight_debit_balance",
		}).AddRow("bln_1", "1000", "0", "0", "0"))
	// credit=1000 debit=300 -> balance=700; inflight credit=100 debit=200 -> inflight=-100
	mock.ExpectExec(`UPDATE blnk.balances`).
		WithArgs("bln_1", "700", "1000", "300", "-100", "100", "200").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	err = ds.ApplyBalanceDeltas(context.Background(), []model.BalanceDelta{delta})
	assert.NoError(t, err)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestApplyBalanceDeltas_CreatesMissingPosition(t *testing.T) {
	db, mock, err := sqlmock.New()
	assert.NoError(t, err)
	defer func() { _ = db.Close() }()
	ds := Datasource{Conn: db}

	key := model.PositionKey{LedgerID: "ldg1", AccountRef: "acc1", Currency: "KZT"}
	delta := model.BalanceDelta{Key: key, AmountDelta: big.NewInt(50)}

	mock.ExpectBegin()
	// First lock attempt finds nothing.
	mock.ExpectQuery(`FOR UPDATE`).
		WillReturnRows(sqlmock.NewRows([]string{
			"balance_id", "credit_balance", "debit_balance", "inflight_credit_balance", "inflight_debit_balance",
		}))
	mock.ExpectExec(`INSERT INTO blnk.balances`).
		WillReturnResult(sqlmock.NewResult(1, 1))
	// Second lock attempt sees the created row.
	mock.ExpectQuery(`FOR UPDATE`).
		WillReturnRows(sqlmock.NewRows([]string{
			"balance_id", "credit_balance", "debit_balance", "inflight_credit_balance", "inflight_debit_balance",
		}).AddRow("bln_new", "0", "0", "0", "0"))
	mock.ExpectExec(`UPDATE blnk.balances`).
		WithArgs("bln_new", "50", "50", "0", "0", "0", "0").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	err = ds.ApplyBalanceDeltas(context.Background(), []model.BalanceDelta{delta})
	assert.NoError(t, err)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestApplyBalanceDeltas_RejectsNegativeHolds(t *testing.T) {
	db, mock, err := sqlmock.New()
	assert.NoError(t, err)
	defer func() { _ = db.Close() }()
	ds := Datasource{Conn: db}

	key := model.PositionKey{LedgerID: "ldg1", AccountRef: "acc1", Currency: "KZT"}
	delta := model.BalanceDelta{Key: key, BlockedDelta: big.NewInt(-100)}

	mock.ExpectBegin()
	mock.ExpectQuery(`FOR UPDATE`).
		WillReturnRows(sqlmock.NewRows([]string{
			"balance_id", "credit_balance", "debit_balance", "inflight_credit_balance", "inflight_debit_balance",
		}).AddRow("bln_1", "0", "0", "0", "50")) // only 50 blocked, removing 100 would go negative
	mock.ExpectRollback()

	err = ds.ApplyBalanceDeltas(context.Background(), []model.BalanceDelta{delta})
	assert.Error(t, err)
	apiErr, ok := err.(apierror.APIError)
	assert.True(t, ok)
	assert.Equal(t, apierror.ErrBadRequest, apiErr.Code)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestApplyBalanceDeltas_EmptyPlanIsNoOp(t *testing.T) {
	db, mock, err := sqlmock.New()
	assert.NoError(t, err)
	defer func() { _ = db.Close() }()
	ds := Datasource{Conn: db}

	assert.NoError(t, ds.ApplyBalanceDeltas(context.Background(), nil))
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestRecomputeHolds_WritesBackAggregates(t *testing.T) {
	db, mock, err := sqlmock.New()
	assert.NoError(t, err)
	defer func() { _ = db.Close() }()
	ds := Datasource{Conn: db}

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT balance_id FROM blnk.balances WHERE balance_id = \$1 FOR UPDATE`).
		WithArgs("bln_1").
		WillReturnRows(sqlmock.NewRows([]string{"balance_id"}).AddRow("bln_1"))
	mock.ExpectQuery(`WITH live AS`).
		WithArgs("bln_1").
		WillReturnRows(sqlmock.NewRows([]string{"debit", "credit"}).AddRow("700", "300"))
	mock.ExpectExec(`UPDATE blnk.balances`).
		WithArgs("bln_1", "-400", "300", "700"). // inflight = credit - debit
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	debit, credit, err := ds.RecomputeHolds(context.Background(), "bln_1")
	assert.NoError(t, err)
	assert.Equal(t, big.NewInt(700), debit)
	assert.Equal(t, big.NewInt(300), credit)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestRecomputeHolds_BalanceNotFound(t *testing.T) {
	db, mock, err := sqlmock.New()
	assert.NoError(t, err)
	defer func() { _ = db.Close() }()
	ds := Datasource{Conn: db}

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT balance_id FROM blnk.balances WHERE balance_id = \$1 FOR UPDATE`).
		WithArgs("missing").
		WillReturnRows(sqlmock.NewRows([]string{"balance_id"}))
	mock.ExpectRollback()

	_, _, err = ds.RecomputeHolds(context.Background(), "missing")
	assert.Error(t, err)
	apiErr, ok := err.(apierror.APIError)
	assert.True(t, ok)
	assert.Equal(t, apierror.ErrNotFound, apiErr.Code)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestCreateHoliday_Success(t *testing.T) {
	db, mock, err := sqlmock.New()
	assert.NoError(t, err)
	defer func() { _ = db.Close() }()
	ds := Datasource{Conn: db}

	mock.ExpectQuery(`INSERT INTO blnk.market_holidays`).
		WithArgs("KASE", sqlmock.AnyArg(), "Independence Day", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(7)))

	holiday, err := ds.CreateHoliday(context.Background(), model.MarketHoliday{
		Venue:       "KASE",
		HolidayDate: time.Date(2026, 12, 16, 0, 0, 0, 0, time.UTC),
		Description: "Independence Day",
	})
	assert.NoError(t, err)
	assert.Equal(t, int64(7), holiday.ID)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestCreateHoliday_Duplicate(t *testing.T) {
	db, mock, err := sqlmock.New()
	assert.NoError(t, err)
	defer func() { _ = db.Close() }()
	ds := Datasource{Conn: db}

	mock.ExpectQuery(`INSERT INTO blnk.market_holidays`).
		WillReturnError(&pq.Error{Code: "23505", Message: "unique_violation"})

	_, err = ds.CreateHoliday(context.Background(), model.MarketHoliday{
		Venue:       "KASE",
		HolidayDate: time.Date(2026, 12, 16, 0, 0, 0, 0, time.UTC),
	})
	assert.Error(t, err)
	apiErr, ok := err.(apierror.APIError)
	assert.True(t, ok)
	assert.Equal(t, apierror.ErrConflict, apiErr.Code)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestCreateHoliday_RequiresVenue(t *testing.T) {
	db, _, err := sqlmock.New()
	assert.NoError(t, err)
	defer func() { _ = db.Close() }()
	ds := Datasource{Conn: db}

	_, err = ds.CreateHoliday(context.Background(), model.MarketHoliday{})
	assert.Error(t, err)
}

func TestCreateLot_Success(t *testing.T) {
	db, mock, err := sqlmock.New()
	assert.NoError(t, err)
	defer func() { _ = db.Close() }()
	ds := Datasource{Conn: db}

	mock.ExpectQuery(`INSERT INTO blnk.balance_lots`).
		WithArgs(sqlmock.AnyArg(), "bln_1", "KZAP", "100", int64(1), "19000.00", "KZT",
			"trade_001", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(1)))

	lot, err := ds.CreateLot(context.Background(), model.BalanceLot{
		BalanceID:  "bln_1",
		Instrument: "KZAP",
		Quantity:   big.NewInt(100),
		Precision:  1,
		Price:      "19000.00",
		Currency:   "KZT",
		Reference:  "trade_001",
	})
	assert.NoError(t, err)
	assert.NotEmpty(t, lot.LotID)
	assert.Equal(t, int64(1), lot.ID)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestCreateLot_RejectsNonPositiveQuantity(t *testing.T) {
	db, _, err := sqlmock.New()
	assert.NoError(t, err)
	defer func() { _ = db.Close() }()
	ds := Datasource{Conn: db}

	_, err = ds.CreateLot(context.Background(), model.BalanceLot{
		BalanceID: "bln_1", Instrument: "KZAP", Quantity: big.NewInt(0), Price: "1", Currency: "KZT",
	})
	assert.Error(t, err)
}

func TestGetLots_ReturnsOldestFirst(t *testing.T) {
	db, mock, err := sqlmock.New()
	assert.NoError(t, err)
	defer func() { _ = db.Close() }()
	ds := Datasource{Conn: db}

	now := time.Now()
	mock.ExpectQuery(`FROM blnk.balance_lots`).
		WithArgs("bln_1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "lot_id", "balance_id", "instrument", "quantity", "precision",
			"price", "currency", "reference", "purchased_at", "created_at",
		}).
			AddRow(int64(1), "lot_a", "bln_1", "KZAP", "100", int64(1), "19000.00", "KZT", "t1", now.Add(-time.Hour), now).
			AddRow(int64(2), "lot_b", "bln_1", "KZAP", "50", int64(1), "19500.00", "KZT", "t2", now, now))

	lots, err := ds.GetLots(context.Background(), "bln_1")
	assert.NoError(t, err)
	assert.Len(t, lots, 2)
	assert.Equal(t, "lot_a", lots[0].LotID)
	assert.Equal(t, big.NewInt(100), lots[0].Quantity)
	assert.Equal(t, "19500.00", lots[1].Price)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestFindOrCreatePosition_FutureBucketKeyedByDate(t *testing.T) {
	db, mock, err := sqlmock.New()
	assert.NoError(t, err)
	defer func() { _ = db.Close() }()
	ds := Datasource{Conn: db}

	settleDate := time.Date(2026, 6, 18, 0, 0, 0, 0, time.UTC)
	// Future bucket is matched by its absolute settle date (6th arg), not the offset.
	rows := positionRows().AddRow(
		"bln_future", "0", "0", "0", "0", "0", "0",
		"KZT", "ldg1", "", time.Now(), 1, "acc1", "KZAP", settleDate, nil, nil,
	)
	mock.ExpectQuery(`FROM blnk.balances`).
		WithArgs("ldg1", "", "acc1", "KZAP", "KZT", settleDate).
		WillReturnRows(rows)

	// SettleCode nil (uncontrolled offset) is allowed; only the date identifies the bucket.
	key := model.PositionKey{LedgerID: "ldg1", AccountRef: "acc1", Instrument: "KZAP", Currency: "KZT", SettleDate: &settleDate}
	balance, err := ds.FindOrCreatePosition(context.Background(), key)
	assert.NoError(t, err)
	assert.Equal(t, "bln_future", balance.BalanceID)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestFindOrCreatePosition_ReturnsExisting(t *testing.T) {
	db, mock, err := sqlmock.New()
	assert.NoError(t, err)
	defer func() { _ = db.Close() }()
	ds := Datasource{Conn: db}

	rows := positionRows().AddRow(
		"bln_existing", "0", "0", "0", "0", "0", "0",
		"KZT", "ldg1", "", time.Now(), 1, "acc1", "", nil, nil, nil,
	)
	// Spot key: the settle-date arg is NULL.
	mock.ExpectQuery(`FROM blnk.balances`).
		WithArgs("ldg1", "", "acc1", "", "KZT", nil).
		WillReturnRows(rows)

	key := model.PositionKey{LedgerID: "ldg1", AccountRef: "acc1", Currency: "KZT"}
	balance, err := ds.FindOrCreatePosition(context.Background(), key)
	assert.NoError(t, err)
	assert.Equal(t, "bln_existing", balance.BalanceID)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestFindOrCreatePosition_CreatesWhenMissing(t *testing.T) {
	db, mock, err := sqlmock.New()
	assert.NoError(t, err)
	defer func() { _ = db.Close() }()
	ds := Datasource{Conn: db}

	// Miss, insert, re-read.
	mock.ExpectQuery(`FROM blnk.balances`).WillReturnRows(positionRows())
	mock.ExpectExec(`INSERT INTO blnk.balances`).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery(`FROM blnk.balances`).WillReturnRows(positionRows().AddRow(
		"bln_created", "0", "0", "0", "0", "0", "0",
		"KZT", "ldg1", "", time.Now(), 1, "acc1", "", nil, nil, nil,
	))

	key := model.PositionKey{LedgerID: "ldg1", AccountRef: "acc1", Currency: "KZT"}
	balance, err := ds.FindOrCreatePosition(context.Background(), key)
	assert.NoError(t, err)
	assert.Equal(t, "bln_created", balance.BalanceID)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestGetMaturedPositions(t *testing.T) {
	db, mock, err := sqlmock.New()
	assert.NoError(t, err)
	defer func() { _ = db.Close() }()
	ds := Datasource{Conn: db}

	settleDate := time.Date(2026, 6, 16, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`b.settle_date IS NOT NULL AND b.settle_date <= \$1`).
		WithArgs(sqlmock.AnyArg(), 100).
		WillReturnRows(positionRows().AddRow(
			"bln_matured", "100", "100", "0", "0", "0", "0",
			"KZT", "ldg1", "idn1", time.Now(), 1, "acc1", "KZAP", settleDate, 2, nil,
		))

	matured, err := ds.GetMaturedPositions(context.Background(), time.Now(), 100)
	assert.NoError(t, err)
	assert.Len(t, matured, 1)
	assert.Equal(t, "bln_matured", matured[0].BalanceID)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestGetPendingInflightByBalance_BigPreciseAmount(t *testing.T) {
	db, mock, err := sqlmock.New()
	assert.NoError(t, err)
	defer func() { _ = db.Close() }()
	ds := Datasource{Conn: db}

	// precise_amount larger than int64 max (9.22e18) must survive as big.Int.
	big1 := "99999999999999999999999999"
	mock.ExpectQuery(`FROM blnk.transactions`).
		WithArgs("bln_1").
		WillReturnRows(sqlmock.NewRows([]string{
			"transaction_id", "parent_transaction", "source", "reference", "amount",
			"precise_amount", "precision", "currency", "destination", "description",
			"status", "created_at", "meta_data",
		}).AddRow("txn_1", "", "bln_1", "ref", "1", big1, float64(1), "USD", "bln_2", "d",
			"INFLIGHT", time.Now(), []byte(`{}`)))

	txns, err := ds.GetPendingInflightByBalance(context.Background(), "bln_1")
	assert.NoError(t, err)
	assert.Len(t, txns, 1)
	expected, _ := new(big.Int).SetString(big1, 10)
	assert.Equal(t, 0, txns[0].PreciseAmount.Cmp(expected), "precise_amount must round-trip without int64 overflow")
}

// TestBrokerageQueriesUseSettleDate guards against regressing to the settle_code
// predicate that excluded explicit-settle_date (settle_code NULL) buckets.
func TestBrokerageQueriesUseSettleDate(t *testing.T) {
	src, err := os.ReadFile("brokerage.go")
	assert.NoError(t, err)
	assert.NotContains(t, string(src), "settle_code IS NOT NULL",
		"brokerage queries must filter future buckets by settle_date, not settle_code")
}
