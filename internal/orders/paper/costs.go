package paper

import (
	"math"

	"github.com/sanjeev2040/jev-trader/internal/domain"
)

// CostModel computes the charges on one executed order. Rates are fractions of
// turnover (quantity × price), so 0.0003 is 0.03%.
type CostModel struct {
	BrokerageRate float64 // brokerage as a fraction of turnover
	BrokerageCap  float64 // maximum brokerage per executed order, in rupees
	STTBuyRate    float64 // securities transaction tax on buys
	STTSellRate   float64 // securities transaction tax on sells
	ExchangeRate  float64 // exchange transaction charges
	SEBIRate      float64 // SEBI turnover fees
	GSTRate       float64 // GST on brokerage, exchange charges and SEBI fees
	StampBuyRate  float64 // stamp duty on buys
}

// ZerodhaIntraday is Zerodha's published equity-intraday charge schedule on
// NSE. Check it against https://zerodha.com/charges before relying on paper
// PnL; the rates change from time to time.
func ZerodhaIntraday() CostModel {
	return CostModel{
		BrokerageRate: 0.0003,
		BrokerageCap:  20,
		STTSellRate:   0.00025,
		ExchangeRate:  0.0000297,
		SEBIRate:      0.000001,
		GSTRate:       0.18,
		StampBuyRate:  0.00003,
	}
}

// Charges returns the total charges, in rupees rounded to the paisa, for one
// executed order.
func (c CostModel) Charges(side domain.Action, qty int64, price float64) float64 {
	turnover := float64(qty) * price
	brokerage := turnover * c.BrokerageRate
	if c.BrokerageCap > 0 {
		brokerage = min(brokerage, c.BrokerageCap)
	}
	exchange := turnover * c.ExchangeRate
	sebi := turnover * c.SEBIRate
	total := brokerage + exchange + sebi + (brokerage+exchange+sebi)*c.GSTRate
	if side == domain.ActionBuy {
		total += turnover * (c.STTBuyRate + c.StampBuyRate)
	} else {
		total += turnover * c.STTSellRate
	}
	return roundPaise(total)
}

func roundPaise(v float64) float64 { return math.Round(v*100) / 100 }
