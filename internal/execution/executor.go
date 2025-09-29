package execution

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/you/arb-bot/internal/config"
	"github.com/you/arb-bot/internal/types"
	"go.uber.org/zap"
)

type cexIface interface {
	PlaceIOC(ctx context.Context, symbol string, side string, qty float64, price float64) (orderID string, filledQty, avgPrice float64, err error)
}
type routerIface interface {
	SwapExactInput(ctx context.Context, amountInWei *big.Int, minOut uint64) (txHash string, err error)
}
type Risk interface {
	AllowTrade(netUSD, roi float64) bool
}

type Executor struct {
	cfg    *config.Config
	cex    cexIface
	router routerIface
	risk   Risk
	log    *zap.Logger
}

func NewExecutor(cfg *config.Config, cex cexIface, router routerIface, risk Risk, log *zap.Logger) *Executor {
	return &Executor{cfg: cfg, cex: cex, router: router, risk: risk, log: log}
}

func (e *Executor) Run(ctx context.Context, in <-chan types.Opportunity) {
	for {
		select {
		case <-ctx.Done():
			return
		case opp := <-in:
			if !e.risk.AllowTrade(opp.NetUSD, opp.ROI) {
				continue
			}
			orderID, filled, avg, err := e.cex.PlaceIOC(ctx, e.cfg.Pair, "BUY", opp.QtyBase, opp.BuyPxCEX)
			if err != nil || filled <= 0 {
				e.log.Warn("CEX IOC failed", zap.Error(err))
				continue
			}
			minOut := uint64(opp.DexOutUSD * (1.0 - float64(e.cfg.Risk.MaxSlippageBps)/10000.0) * 1e6)
			inWei := new(big.Int)
			inWei.SetString(fmt.Sprintf("%.0f", filled*1e18), 10)
			tx, err := e.router.SwapExactInput(ctx, inWei, minOut)
			if err != nil {
				e.log.Error("DEX swap failed", zap.Error(err))
				continue
			}
			e.log.Info("EXECUTED", zap.String("order_id", orderID), zap.String("tx", tx), zap.Float64("filled", filled), zap.Float64("avgPx", avg), zap.Float64("net", opp.NetUSD))
			time.Sleep(10 * time.Millisecond)
		}
	}
}
