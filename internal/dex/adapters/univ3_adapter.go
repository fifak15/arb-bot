// internal/dex/adapters/univ3_adapter.go
package adapters

import (
	"context"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/you/arb-bot/internal/dex/core"
	"github.com/you/arb-bot/internal/dex/univ3"
)

type u3Quoter struct{ q univ3.Quoter }

func (u u3Quoter) QuoteDexOutUSD(ctx context.Context, in, out common.Address, qty, ethUSD float64) (float64, float64, core.QuoteMeta, error) {
	v, g, fee, err := u.q.QuoteDexOutUSD(ctx, in, out, qty, ethUSD) // уже есть реализация :contentReference[oaicite:0]{index=0}
	return v, g, core.QuoteMeta{FeeTier: fee}, err
}
func (u u3Quoter) QuoteDexInUSD(ctx context.Context, in, out common.Address, qty, ethUSD float64) (float64, float64, core.QuoteMeta, error) {
	v, g, fee, err := u.q.QuoteDexInUSD(ctx, in, out, qty, ethUSD) // уже есть реализация :contentReference[oaicite:1]{index=1}
	return v, g, core.QuoteMeta{FeeTier: fee}, err
}

type u3Router struct{ r univ3.Router }

func (u u3Router) SwapExactInput(ctx context.Context, in, out common.Address, amountIn, minOut *big.Int, meta core.QuoteMeta) (string, error) {
	return u.r.SwapExactInput(ctx, in, out, amountIn, minOut, meta.FeeTier) // уже есть реализация :contentReference[oaicite:2]{index=2}
}
func (u u3Router) SwapExactOutput(ctx context.Context, in, out common.Address, amountOut, maxIn *big.Int, meta core.QuoteMeta) (string, error) {
	return u.r.SwapExactOutput(ctx, in, out, amountOut, maxIn, meta.FeeTier) // уже есть реализация :contentReference[oaicite:3]{index=3}
}
