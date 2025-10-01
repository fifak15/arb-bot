package marketdata

import "time"

// DexQuotes holds the final, processed results for both sides of a DEX trade for a single pair.
type DexQuotes struct {
	PairSymbol string
	// Result for CEX_BUY_DEX_SELL (selling base on DEX)
	DexOutAmountUSD float64
	DexOutFeeTier   uint32
	DexOutError     error

	// Result for DEX_BUY_CEX_SELL (buying base on DEX)
	DexInAmountUSD float64
	DexInFeeTier   uint32
	DexInError      error

	GasUSD float64 // Estimated gas for a single swap
	Ts     time.Time
}

// Snapshot represents a combined view of CEX and DEX prices at a point in time.
type Snapshot struct {
	BestAskCEX, BestBidCEX float64
	DexOutUSD, GasSellUSD  float64 // CEX_BUY_DEX_SELL
	DexInUSD, GasBuyUSD    float64 // DEX_BUY_CEX_SELL
	DexSellFeeTier         uint32
	DexBuyFeeTier          uint32
	Ts                     time.Time
}

// CexIface is the interface for a CEX client.
type CexIface interface {
	BestBidAsk(symbol string) (bid, ask float64, err error)
}