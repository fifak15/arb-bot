package detector

import (
	"context"
	"time"

	"github.com/you/arb-bot/internal/config"
	"github.com/you/arb-bot/internal/marketdata"
	"github.com/you/arb-bot/internal/types"
	"go.uber.org/zap"
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
			if last.BestBidCEX == 0 || last.BestAskCEX == 0 {
				continue
			}

			qty := cfg.Trade.BaseQty
			// CEX→DEX: купить на CEX, продать на DEX
			if last.DexOutUSD > 0 {
				buyPx := last.BestAskCEX
				gross := last.DexOutUSD
				cost := buyPx*qty + last.GasSellUSD
				net := gross - cost
				roi := 0.0
				if cost > 0 {
					roi = net / cost
				}
				opp := types.Opportunity{
					Direction:  types.CEXBuyDEXSell,
					QtyBase:    qty,
					BuyPxCEX:   buyPx,
					DexOutUSD:  last.DexOutUSD,
					DexFeeTier: last.DexSellFeeTier,
					DexVenue:   last.DexSellVenue,
					GasUSD:     last.GasSellUSD,
					NetUSD:     net,
					ROI:        roi,
					Ts:         last.Ts,
				}
				select {
				case out <- opp:
				default:
					log.Warn("detector: opportunity channel full; dropping CEX→DEX")
				}
			}

			// DEX→CEX: купить на DEX, продать на CEX
			if last.DexInUSD > 0 {
				sellPx := last.BestBidCEX
				revenue := sellPx * qty
				cost := last.DexInUSD + last.GasBuyUSD
				net := revenue - cost
				roi := 0.0
				if cost > 0 {
					roi = net / cost
				}
				opp := types.Opportunity{
					Direction:  types.DEXBuyCEXSell,
					QtyBase:    qty,
					SellPxCEX:  sellPx,
					DexInUSD:   last.DexInUSD,
					DexFeeTier: last.DexBuyFeeTier,
					DexVenue:   last.DexBuyVenue,
					GasUSD:     last.GasBuyUSD,
					NetUSD:     net,
					ROI:        roi,
					Ts:         last.Ts,
				}
				select {
				case out <- opp:
				default:
					log.Warn("detector: opportunity channel full; dropping DEX→CEX")
				}
			}
		}
	}
}
