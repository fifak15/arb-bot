package marketdata

import (
	"context"
	"math"
	"time"

	"github.com/you/arb-bot/internal/config"
	imetrics "github.com/you/arb-bot/internal/metrics"
	"go.uber.org/zap"
)

// The Run function is now updated to work with the new channel-based quote system.
func Run(
	ctx context.Context,
	cfg *config.Config,
	cex CexIface,
	quoteCh <-chan DexQuotes, // The new channel for receiving DEX quotes
	out chan<- Snapshot,
	log *zap.Logger,
) {
	log.Info("marketdata runner started", zap.String("pair", cfg.Pair))

	// The loop is now driven by incoming DEX quotes, not a ticker.
	for {
		select {
		case <-ctx.Done():
			log.Info("marketdata runner stopped (context done)", zap.String("pair", cfg.Pair))
			return

		case quotes := <-quoteCh:
			// Once we receive new DEX quotes, we fetch the latest CEX price.
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

			// Process quotes for CEX_BUY_DEX_SELL
			if quotes.DexOutError != nil {
				imetrics.QuoterErrors.Inc()
				log.Warn("quoter failed (dex sell)", zap.String("pair", cfg.Pair), zap.Error(quotes.DexOutError))
				continue
			}
			log.Debug("dex quote ok (sell)",
				zap.String("pair", cfg.Pair),
				zap.Float64("out_usd", quotes.DexOutAmountUSD),
				zap.Uint32("fee_tier", quotes.DexOutFeeTier),
			)

			// Process quotes for DEX_BUY_CEX_SELL
			if quotes.DexInError != nil {
				imetrics.QuoterErrors.Inc()
				log.Warn("quoter failed (dex buy)", zap.String("pair", cfg.Pair), zap.Error(quotes.DexInError))
				continue
			}
			log.Debug("dex quote ok (buy)",
				zap.String("pair", cfg.Pair),
				zap.Float64("in_usd", quotes.DexInAmountUSD),
				zap.Uint32("fee_tier", quotes.DexInFeeTier),
			)

			gasUSD := quotes.GasUSD
			imetrics.DexOutUSD.Set(quotes.DexOutAmountUSD)
			imetrics.GasUSD.Set(gasUSD)

			if math.IsNaN(quotes.DexOutAmountUSD) || math.IsInf(quotes.DexOutAmountUSD, 0) || math.IsNaN(quotes.DexInAmountUSD) || math.IsInf(quotes.DexInAmountUSD, 0) {
				log.Warn("received non-finite dex quote", zap.String("pair", cfg.Pair))
				continue
			}

			snap := Snapshot{
				BestAskCEX:     ask,
				BestBidCEX:     bid,
				DexOutUSD:      quotes.DexOutAmountUSD,
				GasSellUSD:     gasUSD,
				DexSellFeeTier: quotes.DexOutFeeTier,
				DexInUSD:       quotes.DexInAmountUSD,
				GasBuyUSD:      gasUSD,
				DexBuyFeeTier:  quotes.DexInFeeTier,
				Ts:             time.Now(),
			}
			select {
			case out <- snap:
			case <-ctx.Done():
				log.Info("marketdata runner stopped while publishing", zap.String("pair", cfg.Pair))
				return
			default:
				log.Warn("snapshot channel is full; dropping snapshot", zap.String("pair", cfg.Pair))
			}
		}
	}
}