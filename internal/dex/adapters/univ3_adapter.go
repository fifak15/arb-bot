package adapters

import (
	"context"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/you/arb-bot/internal/dex/core"
	"github.com/you/arb-bot/internal/dex/univ3"
)

type U3Quoter struct{ impl univ3.Quoter }
type U3Router struct{ impl univ3.Router }

func NewU3Quoter(q univ3.Quoter) core.Quoter { return U3Quoter{impl: q} }
func NewU3Router(r univ3.Router) core.Router { return U3Router{impl: r} }

func (u U3Quoter) QuoteDexOutUSD(ctx context.Context, in, out common.Address, qty, ethUSD float64) (float64, float64, core.QuoteMeta, error) {
	v, g, fee, err := u.impl.QuoteDexOutUSD(ctx, in, out, qty, ethUSD)
	return v, g, core.QuoteMeta{FeeTier: fee}, err
}

func (u U3Quoter) QuoteDexInUSD(ctx context.Context, in, out common.Address, qty, ethUSD float64) (float64, float64, core.QuoteMeta, error) {
	v, g, fee, err := u.impl.QuoteDexInUSD(ctx, in, out, qty, ethUSD)
	return v, g, core.QuoteMeta{FeeTier: fee}, err
}

func (u U3Router) SwapExactInput(ctx context.Context, in, out common.Address, amountIn, minOut *big.Int, meta core.QuoteMeta) (string, error) {
	return u.impl.SwapExactInput(ctx, in, out, amountIn, minOut, meta.FeeTier)
}

func (u U3Router) SwapExactOutput(ctx context.Context, in, out common.Address, amountOut, maxIn *big.Int, meta core.QuoteMeta) (string, error) {
	return u.impl.SwapExactOutput(ctx, in, out, amountOut, maxIn, meta.FeeTier)
}
