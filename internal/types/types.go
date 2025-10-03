package types

import (
	"time"

	"github.com/you/arb-bot/internal/dex/core"
)

type Direction string

const (
	CEXBuyDEXSell Direction = "CEX_BUY_DEX_SELL"
	DEXBuyCEXSell Direction = "DEX_BUY_CEX_SELL"
)

type PairMeta struct {
	Symbol string `json:"symbol"`
	Base   string `json:"base"`
	Addr   string `json:"addr"`
	CgID   string `json:"cg_id"`
}

type Opportunity struct {
	Direction  Direction
	QtyBase    float64
	BuyPxCEX   float64
	SellPxCEX  float64
	DexOutUSD  float64
	DexInUSD   float64
	DexFeeTier uint32
	DexVenue   core.VenueID

	GasUSD float64
	NetUSD float64
	ROI    float64
	Ts     time.Time
}
