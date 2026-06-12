package metrics

import (
	"log"

	"go.opentelemetry.io/otel/metric"
)

// Brokerage-layer instruments. Initialized by the package init below so they
// are ready alongside the core instruments.
var (
	// BrokerageTradeBookedTotal counts booked trades.
	// Attributes: side (buy, sell), instrument
	BrokerageTradeBookedTotal metric.Int64Counter

	// BrokerageSellRejectedTotal counts sells rejected before booking.
	// Attributes: reason (insufficient_tradable, validation, settle_date, lookup)
	BrokerageSellRejectedTotal metric.Int64Counter

	// BrokerageSettlementRunTotal counts settlement passes (RunSettlement).
	BrokerageSettlementRunTotal metric.Int64Counter

	// BrokerageSettlementErrorsTotal counts per-artifact settlement errors.
	BrokerageSettlementErrorsTotal metric.Int64Counter

	// BrokerageSettledTradesTotal counts trade legs settled.
	BrokerageSettledTradesTotal metric.Int64Counter

	// BrokerageMaturedBucketsExamined records the number of matured buckets a
	// settlement pass examined (a proxy for the pending-settlement backlog).
	BrokerageMaturedBucketsExamined metric.Int64Histogram

	// BrokerageSettlementDuration records the wall-clock time of a settlement pass.
	BrokerageSettlementDuration metric.Float64Histogram

	// BrokerageReconciledTotal counts settlement journals completed by recovery.
	BrokerageReconciledTotal metric.Int64Counter

	// OrderRejectedTotal counts orders rejected. Attributes: stage (check, execute)
	OrderRejectedTotal metric.Int64Counter

	// OrderExecutedTotal counts orders executed into a trade. Attributes: side
	OrderExecutedTotal metric.Int64Counter
)

func init() {
	if err := initBrokerage(); err != nil {
		log.Fatalf("failed to initialize brokerage metrics instruments: %v", err)
	}
}

func initBrokerage() error {
	var err error
	if BrokerageTradeBookedTotal, err = meter.Int64Counter("blnk.brokerage.trade.booked.total",
		metric.WithDescription("Total brokerage trades booked by side and instrument"),
		metric.WithUnit("{trade}")); err != nil {
		return err
	}
	if BrokerageSellRejectedTotal, err = meter.Int64Counter("blnk.brokerage.sell.rejected.total",
		metric.WithDescription("Total brokerage sells rejected before booking by reason"),
		metric.WithUnit("{trade}")); err != nil {
		return err
	}
	if BrokerageSettlementRunTotal, err = meter.Int64Counter("blnk.brokerage.settlement.run.total",
		metric.WithDescription("Total brokerage settlement passes"),
		metric.WithUnit("{run}")); err != nil {
		return err
	}
	if BrokerageSettlementErrorsTotal, err = meter.Int64Counter("blnk.brokerage.settlement.errors.total",
		metric.WithDescription("Total per-artifact brokerage settlement errors"),
		metric.WithUnit("{error}")); err != nil {
		return err
	}
	if BrokerageSettledTradesTotal, err = meter.Int64Counter("blnk.brokerage.settled.trades.total",
		metric.WithDescription("Total brokerage trade legs settled"),
		metric.WithUnit("{trade}")); err != nil {
		return err
	}
	if BrokerageMaturedBucketsExamined, err = meter.Int64Histogram("blnk.brokerage.settlement.matured_buckets",
		metric.WithDescription("Matured buckets examined per settlement pass"),
		metric.WithUnit("{bucket}")); err != nil {
		return err
	}
	if BrokerageSettlementDuration, err = meter.Float64Histogram("blnk.brokerage.settlement.duration",
		metric.WithDescription("Duration of a brokerage settlement pass"),
		metric.WithUnit("s")); err != nil {
		return err
	}
	if BrokerageReconciledTotal, err = meter.Int64Counter("blnk.brokerage.settlement.reconciled.total",
		metric.WithDescription("Total settlement journals completed by recovery"),
		metric.WithUnit("{journal}")); err != nil {
		return err
	}
	if OrderRejectedTotal, err = meter.Int64Counter("blnk.brokerage.order.rejected.total",
		metric.WithDescription("Total orders rejected by stage"),
		metric.WithUnit("{order}")); err != nil {
		return err
	}
	if OrderExecutedTotal, err = meter.Int64Counter("blnk.brokerage.order.executed.total",
		metric.WithDescription("Total orders executed into a trade by side"),
		metric.WithUnit("{order}")); err != nil {
		return err
	}
	return nil
}
