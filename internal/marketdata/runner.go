package marketdata

import (
	"context"
	"time"

	"github.com/you/arb-bot/internal/config"
	imetrics "github.com/you/arb-bot/internal/metrics"
	"go.uber.org/zap"
)

type Snapshot struct {
	BestAskCEX, BestBidCEX, DexOutUSD, GasUSD float64
	Ts                                        time.Time
}

type cexIface interface {
	BestBidAsk(symbol string) (bid, ask float64, err error)
}
type quoterIface interface {
	QuoteDexOutUSD(ctx context.Context, qtyBase float64, ethUSD float64) (outUSD float64, gasUSD float64, err error)
}

func Run(
	ctx context.Context,
	cfg *config.Config,
	cex cexIface,
	quoter quoterIface,
	out chan<- Snapshot,
	log *zap.Logger,
) {
	log.Info("marketdata runner started",
		zap.String("pair", cfg.Pair),
		zap.Duration("quote_interval", cfg.QuoteInterval()),
		zap.Float64("base_qty", cfg.Trade.BaseQty),
	)

	t := time.NewTicker(cfg.QuoteInterval())
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info("marketdata runner stopped (context done)")
			return

		case <-t.C:
			bid, ask, err := cex.BestBidAsk(cfg.Pair)
			if err != nil {
				log.Warn("cex BestBidAsk failed",
					zap.String("pair", cfg.Pair),
					zap.Error(err),
				)
				continue
			}
			if bid == 0 || ask == 0 {
				log.Debug("cex book empty or zero",
					zap.String("pair", cfg.Pair),
					zap.Float64("bid", bid),
					zap.Float64("ask", ask),
				)
				continue
			}

			mid := 0.5 * (bid + ask)
			log.Debug("cex mid computed",
				zap.String("pair", cfg.Pair),
				zap.Float64("bid", bid),
				zap.Float64("ask", ask),
				zap.Float64("mid", mid),
			)
			imetrics.CEXMid.Set(mid)
			started := time.Now()
			dexOut, gasUSD, err := quoter.QuoteDexOutUSD(ctx, cfg.Trade.BaseQty, mid)
			took := time.Since(started)

			if err != nil {
				imetrics.QuoterErrors.Inc()
				log.Warn("quoter failed",
					zap.String("pair", cfg.Pair),
					zap.Float64("qty_base", cfg.Trade.BaseQty),
					zap.Float64("eth_usd(mid)", mid),
					zap.Duration("took", took),
					zap.Error(err),
				)
				continue
			}

			imetrics.DexOutUSD.Set(dexOut)
			imetrics.GasUSD.Set(gasUSD)
			if imetrics.QuoteLatency != nil {
				imetrics.QuoteLatency.Observe(took.Seconds())
			}

			log.Info("dex quote ok",
				zap.String("pair", cfg.Pair),
				zap.Float64("qty_base", cfg.Trade.BaseQty),
				zap.Float64("eth_usd(mid)", mid),
				zap.Float64("dex_out_usd", dexOut),
				zap.Float64("gas_usd", gasUSD),
				zap.Duration("took", took),
			)

			snap := Snapshot{
				BestAskCEX: ask,
				BestBidCEX: bid,
				DexOutUSD:  dexOut,
				GasUSD:     gasUSD,
				Ts:         time.Now(),
			}
			select {
			case out <- snap:
				log.Debug("snapshot published",
					zap.Float64("best_bid_cex", bid),
					zap.Float64("best_ask_cex", ask),
					zap.Float64("dex_out_usd", dexOut),
					zap.Float64("gas_usd", gasUSD),
				)
			case <-ctx.Done():
				log.Info("marketdata runner stopped while publishing")
				return
			default:
				// если канал захлебнулся — не блокируемся, просто логируем
				log.Warn("snapshot channel is full; dropping snapshot")
			}
		}
	}
}
