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
	"net/http"

	model2 "github.com/blnkfinance/blnk/api/model"
	"github.com/blnkfinance/blnk/internal/apierror"
	"github.com/gin-gonic/gin"
)

// CreateOrder creates a draft order.
func (a Api) CreateOrder(c *gin.Context) {
	var request model2.CreateOrderRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		respondCode(c, apierror.ErrGenMalformedRequest, err.Error(), nil)
		return
	}
	o, err := request.ToOrder()
	if err != nil {
		respondCode(c, apierror.ErrGenValidation, err.Error(), nil)
		return
	}
	resp, err := a.blnk.CreateOrder(c.Request.Context(), o)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusCreated, resp)
}

// GetOrder retrieves an order by ID.
func (a Api) GetOrder(c *gin.Context) {
	id, passed := c.Params.Get("id")
	if !passed {
		respondCode(c, apierror.ErrGenValidation, "order id is required", nil)
		return
	}
	resp, err := a.blnk.GetOrder(c.Request.Context(), id)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, resp)
}

// CheckOrder validates a draft order.
func (a Api) CheckOrder(c *gin.Context) {
	id, passed := c.Params.Get("id")
	if !passed {
		respondCode(c, apierror.ErrGenValidation, "order id is required", nil)
		return
	}
	resp, err := a.blnk.CheckOrder(c.Request.Context(), id)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, resp)
}

// ApproveOrder approves a checked order.
func (a Api) ApproveOrder(c *gin.Context) {
	id, passed := c.Params.Get("id")
	if !passed {
		respondCode(c, apierror.ErrGenValidation, "order id is required", nil)
		return
	}
	resp, err := a.blnk.ApproveOrder(c.Request.Context(), id)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, resp)
}

// ExecuteOrder books the trade for an approved order.
func (a Api) ExecuteOrder(c *gin.Context) {
	id, passed := c.Params.Get("id")
	if !passed {
		respondCode(c, apierror.ErrGenValidation, "order id is required", nil)
		return
	}
	resp, err := a.blnk.ExecuteOrder(c.Request.Context(), id)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, resp)
}

// CancelOrder cancels a non-terminal order.
func (a Api) CancelOrder(c *gin.Context) {
	id, passed := c.Params.Get("id")
	if !passed {
		respondCode(c, apierror.ErrGenValidation, "order id is required", nil)
		return
	}
	resp, err := a.blnk.CancelOrder(c.Request.Context(), id)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, resp)
}

// SubmitOrder runs the full straight-through lifecycle.
func (a Api) SubmitOrder(c *gin.Context) {
	var request model2.CreateOrderRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		respondCode(c, apierror.ErrGenMalformedRequest, err.Error(), nil)
		return
	}
	o, err := request.ToOrder()
	if err != nil {
		respondCode(c, apierror.ErrGenValidation, err.Error(), nil)
		return
	}
	resp, err := a.blnk.SubmitOrder(c.Request.Context(), o)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusCreated, resp)
}

// UpsertDict stores a configurable reference value.
func (a Api) UpsertDict(c *gin.Context) {
	var request model2.DictRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		respondCode(c, apierror.ErrGenMalformedRequest, err.Error(), nil)
		return
	}
	resp, err := a.blnk.UpsertDict(c.Request.Context(), request.ToDictEntry())
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusCreated, resp)
}

// GetDict lists reference values of a category.
func (a Api) GetDict(c *gin.Context) {
	category, passed := c.Params.Get("category")
	if !passed {
		respondCode(c, apierror.ErrGenValidation, "category is required", nil)
		return
	}
	resp, err := a.blnk.GetDict(c.Request.Context(), category)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, resp)
}

// AddStopList blocks an identity.
func (a Api) AddStopList(c *gin.Context) {
	var request model2.StopListRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		respondCode(c, apierror.ErrGenMalformedRequest, err.Error(), nil)
		return
	}
	resp, err := a.blnk.AddStopList(c.Request.Context(), request.ToStopListEntry())
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusCreated, resp)
}

// SetTradingTime upserts a venue trading window.
func (a Api) SetTradingTime(c *gin.Context) {
	var request model2.TradingTimeRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		respondCode(c, apierror.ErrGenMalformedRequest, err.Error(), nil)
		return
	}
	resp, err := a.blnk.SetTradingTime(c.Request.Context(), request.ToTradingTime())
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusCreated, resp)
}

// GetTradingTimes lists a venue's trading windows.
func (a Api) GetTradingTimes(c *gin.Context) {
	venue, passed := c.Params.Get("venue")
	if !passed {
		respondCode(c, apierror.ErrGenValidation, "venue is required", nil)
		return
	}
	resp, err := a.blnk.GetTradingTimes(c.Request.Context(), venue)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, resp)
}
