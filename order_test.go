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
package blnk

import (
	"context"
	"testing"

	"github.com/blnkfinance/blnk/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

func draftOrder(side string) *model.Order {
	return &model.Order{
		OrderID: "ord_1", Reference: "ord-1", LedgerID: "ldg", IdentityID: "idn", AccountRef: "acc",
		Instrument: "AAPL", Venue: "", Currency: "USD", Side: side,
		Quantity: "10", QuantityPrecision: 1, Price: "150.00", MoneyPrecision: 100,
		SettlementBalanceID: "bln_s", MarketBalanceID: "bln_m",
		Status: model.OrderStatusDraft,
	}
}

func TestCheckOrder_RejectsStopListed(t *testing.T) {
	service, datasource, cleanup := newBrokerageTestBlnk(t)
	defer cleanup()

	datasource.On("GetOrder", mock.Anything, "ord_1").Return(draftOrder(model.TradeSideBuy), nil)
	datasource.On("IsStopListed", mock.Anything, "idn", "AAPL").Return(true, "sanctioned", nil)
	datasource.On("UpdateOrder", mock.Anything, mock.MatchedBy(func(o *model.Order) bool {
		return o.Status == model.OrderStatusRejected && o.RejectReason == "stop list" &&
			o.AMLStatus == model.AMLStatusBlocked
	})).Return(nil)

	o, err := service.CheckOrder(context.Background(), "ord_1")
	assert.NoError(t, err) // rejection is a normal outcome
	assert.Equal(t, model.OrderStatusRejected, o.Status)
	assert.Equal(t, "stop list", o.RejectReason)
	datasource.AssertExpectations(t)
}

func TestApproveOrder_RejectsWrongState(t *testing.T) {
	service, datasource, cleanup := newBrokerageTestBlnk(t)
	defer cleanup()

	// A DRAFT order cannot be approved (must be CHECKED first).
	datasource.On("GetOrder", mock.Anything, "ord_1").Return(draftOrder(model.TradeSideBuy), nil)
	_, err := service.ApproveOrder(context.Background(), "ord_1")
	assert.Error(t, err)
	datasource.AssertNotCalled(t, "UpdateOrder", mock.Anything, mock.Anything)
}

func TestExecuteOrder_RejectsWrongState(t *testing.T) {
	service, datasource, cleanup := newBrokerageTestBlnk(t)
	defer cleanup()

	// A CHECKED order cannot be executed (must be APPROVED first).
	checked := draftOrder(model.TradeSideBuy)
	checked.Status = model.OrderStatusChecked
	datasource.On("GetOrder", mock.Anything, "ord_1").Return(checked, nil)
	_, err := service.ExecuteOrder(context.Background(), "ord_1")
	assert.Error(t, err)
	datasource.AssertNotCalled(t, "UpdateOrder", mock.Anything, mock.Anything)
}
