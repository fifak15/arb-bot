package core

import (
	"context"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
)

type VenueID string

const (
	VenueUniswapV3 VenueID = "uniswap_v3"
	VenueSushiV2   VenueID = "sushi_v2"
	VenueCamelotV2 VenueID = "camelot_v2"
	VenueCamelotV3 VenueID = "camelot_v3"
)

type QuoteMeta struct {
	FeeTier uint32
}

type Quoter interface {
	QuoteDexOutUSD(ctx context.Context, tokenIn, tokenOut common.Address, qtyBase, ethUSD float64) (outUSD, gasUSD float64, meta QuoteMeta, err error)
	QuoteDexInUSD(ctx context.Context, tokenIn, tokenOut common.Address, qtyBase, ethUSD float64) (inUSD, gasUSD float64, meta QuoteMeta, err error)
}

type Router interface {
	SwapExactInput(ctx context.Context, tokenIn, tokenOut common.Address, amountIn, minOut *big.Int, meta QuoteMeta) (txHash string, err error)
	SwapExactOutput(ctx context.Context, tokenIn, tokenOut common.Address, amountOut, maxIn *big.Int, meta QuoteMeta) (txHash string, err error)
}

type Venue struct {
	ID     VenueID
	Quoter Quoter
	Router Router
}

var registry = map[VenueID]*Venue{}

func Register(v *Venue)     { registry[v.ID] = v }
func Get(id VenueID) *Venue { return registry[id] }
func Enabled(ids []VenueID) []*Venue {
	out := make([]*Venue, 0, len(ids))
	for _, id := range ids {
		if v := Get(id); v != nil {
			out = append(out, v)
		}
	}
	return out
}
