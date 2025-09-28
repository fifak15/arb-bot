package detector

import (
	"context"
	"github.com/you/arb-bot/internal/config"
	"github.com/you/arb-bot/internal/marketdata"
	"github.com/you/arb-bot/internal/types"
	"go.uber.org/zap"
	"time"
)

func Run(ctx context.Context, cfg *config.Config, in <-chan marketdata.Snapshot, out chan<- types.Opportunity, log *zap.Logger) {
	t := time.NewTicker(cfg.DetectorTick())
	defer t.Stop()
	var last marketdata.Snapshot
	for {
		select {
		case <-ctx.Done():
			return
		case s := <-in:
			last = s
		case <-t.C:
			if last.BestAskCEX == 0 || last.DexOutUSD == 0 {
				continue
			}
			q := cfg.Trade.BaseQty
			taker := last.BestAskCEX * q * float64(cfg.MEXC.TakerFeeBps) / 10000.0
			cost := last.BestAskCEX*q + taker
			net := last.DexOutUSD - cost - last.GasUSD
			roi := 0.0
			if cost > 0 {
				roi = net / cost
			}
			if net >= cfg.Risk.MinProfitUSD && roi >= (cfg.Risk.MinROIBps/10000.0) {
				out <- types.Opportunity{QtyBase: q, BuyPxCEX: last.BestAskCEX, DexOutUSD: last.DexOutUSD, GasUSD: last.GasUSD, NetUSD: net, ROI: roi, Ts: time.Now()}
			}
		}
	}
}
