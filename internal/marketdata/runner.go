package marketdata

import (
	"context"
	"time"

	"github.com/you/arb-bot/internal/config"
	imetrics "github.com/you/arb-bot/internal/metrics"
	"go.uber.org/zap"
)

type Snapshot struct {
	BestAskCEX, BestBidCEX float64
	DexOutUSD, GasSellUSD  float64 // CEX_BUY_DEX_SELL
	DexInUSD, GasBuyUSD    float64 // DEX_BUY_CEX_SELL
	DexSellFeeTier         uint32
	DexBuyFeeTier          uint32
	Ts                     time.Time
}

type cexIface interface {
	BestBidAsk(symbol string) (bid, ask float64, err error)
}
type quoterIface interface {
	QuoteDexOutUSD(ctx context.Context, qtyBase float64, ethUSD float64) (outUSD float64, gasUSD float64, feeTier uint32, err error)
	QuoteDexInUSD(ctx context.Context, qtyBase float64, ethUSD float64) (inUSD float64, gasUSD float64, feeTier uint32, err error)
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
				log.Warn("cex BestBidAsk failed", zap.String("pair", cfg.Pair), zap.Error(err))
				continue
			}
			if bid == 0 || ask == 0 {
				log.Debug("cex book empty or zero", zap.String("pair", cfg.Pair), zap.Float64("bid", bid), zap.Float64("ask", ask))
				continue
			}

			mid := 0.5 * (bid + ask)
			imetrics.CEXMid.Set(mid)

			// Quote for CEX_BUY_DEX_SELL
			startedSell := time.Now()
			dexOut, gasSell, sellFee, errSell := quoter.QuoteDexOutUSD(ctx, cfg.Trade.BaseQty, mid)
			if errSell != nil {
				imetrics.QuoterErrors.Inc()
				log.Warn("quoter failed (dex sell)", zap.Error(errSell))
				// continue? or publish partial? for now, skip.
				continue
			}
			log.Info("dex quote ok (sell)",
				zap.Float64("out_usd", dexOut),
				zap.Float64("gas_usd", gasSell),
				zap.Uint32("fee_tier", sellFee),
				zap.Duration("took", time.Since(startedSell)),
			)

			// Quote for DEX_BUY_CEX_SELL
			startedBuy := time.Now()
			dexIn, gasBuy, buyFee, errBuy := quoter.QuoteDexInUSD(ctx, cfg.Trade.BaseQty, mid)
			if errBuy != nil {
				imetrics.QuoterErrors.Inc()
				log.Warn("quoter failed (dex buy)", zap.Error(errBuy))
				continue
			}
			log.Info("dex quote ok (buy)",
				zap.Float64("in_usd", dexIn),
				zap.Float64("gas_usd", gasBuy),
				zap.Uint32("fee_tier", buyFee),
				zap.Duration("took", time.Since(startedBuy)),
			)

			imetrics.DexOutUSD.Set(dexOut)
			imetrics.GasUSD.Set(gasSell) // Note: using sell gas for generic metric

			snap := Snapshot{
				BestAskCEX:     ask,
				BestBidCEX:     bid,
				DexOutUSD:      dexOut,
				GasSellUSD:     gasSell,
				DexSellFeeTier: sellFee,
				DexInUSD:       dexIn,
				GasBuyUSD:      gasBuy,
				DexBuyFeeTier:  buyFee,
				Ts:             time.Now(),
			}
			select {
			case out <- snap:
			// OK
			case <-ctx.Done():
				log.Info("marketdata runner stopped while publishing")
				return
			default:
				log.Warn("snapshot channel is full; dropping snapshot")
			}
		}
	}
}
