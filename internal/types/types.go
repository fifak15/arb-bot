package types

import "time"

type Direction string

const (
	CEXBuyDEXSell Direction = "CEX_BUY_DEX_SELL"
	DEXBuyCEXSell Direction = "DEX_BUY_CEX_SELL"
)

type Opportunity struct {
	Direction      Direction
	QtyBase        float64
	BuyPxCEX       float64
	SellPxCEX      float64
	DexOutUSD      float64 // For CEXBuyDEXSell: how much USD we get for BaseQty
	DexInUSD       float64 // For DEXBuyCEXSell: how much USD we need to buy BaseQty
	DexFeeTier     uint32
	GasUSD, NetUSD float64
	ROI            float64
	Ts             time.Time
}
