package marketdata

import (
	"context"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/you/arb-bot/internal/config"
	"github.com/you/arb-bot/internal/dex/core"
	"github.com/you/arb-bot/internal/discovery"
	imetrics "github.com/you/arb-bot/internal/metrics"
	"go.uber.org/zap"
)

type Snapshot struct {
	BestAskCEX, BestBidCEX float64

	// CEX_BUY -> DEX_SELL
	DexOutUSD, GasSellUSD float64
	DexSellFeeTier        uint32
	DexSellVenue          core.VenueID

	// DEX_BUY -> CEX_SELL
	DexInUSD, GasBuyUSD float64
	DexBuyFeeTier       uint32
	DexBuyVenue         core.VenueID

	Ts time.Time
}

type cexIface interface {
	BestBidAsk(symbol string) (bid, ask float64, err error)
}

func Run(
	ctx context.Context,
	cfg *config.Config,
	pm discovery.PairMeta,
	cex cexIface,
	out chan<- Snapshot,
	log *zap.Logger,
) {
	log.Info("marketdata runner started",
		zap.String("pair", cfg.Pair),
		zap.Duration("quote_interval", cfg.QuoteInterval()),
		zap.Float64("base_qty", cfg.Trade.BaseQty),
	)

	baseTokenAddr := common.HexToAddress(pm.Addr)
	quoteTokenAddr := common.HexToAddress(cfg.DEX.USDT)

	t := time.NewTicker(cfg.QuoteInterval())
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info("marketdata runner stopped (context done)")
			return
		case <-t.C:
			bid, ask, err := cex.BestBidAsk(cfg.Pair)
			if err != nil || bid == 0 || ask == 0 {
				if err != nil {
					log.Warn("cex BestBidAsk failed", zap.String("pair", cfg.Pair), zap.Error(err))
				}
				continue
			}
			mid := 0.5 * (bid + ask)
			imetrics.CEXMid.Set(mid)

			venues := core.Enabled(cfg.DEX.Venues)
			if len(venues) == 0 {
				log.Warn("no enabled DEX venues in config")
				continue
			}

			// SELL path (maximize out - gas)
			var (
				sellOut, sellGas float64
				sellFee          uint32
				sellVenue        core.VenueID
				haveSell         bool
				mu               sync.Mutex
				wg               sync.WaitGroup
			)
			for _, v := range venues {
				v := v
				wg.Add(1)
				go func() {
					defer wg.Done()
					outUSD, gasUSD, meta, err := v.Quoter.QuoteDexOutUSD(ctx, baseTokenAddr, quoteTokenAddr, cfg.Trade.BaseQty, mid)
					if err != nil || outUSD == 0 {
						return
					}
					mu.Lock()
					if !haveSell || (outUSD-gasUSD) > (sellOut-sellGas) {
						haveSell = true
						sellOut, sellGas, sellFee, sellVenue = outUSD, gasUSD, meta.FeeTier, v.ID
					}
					mu.Unlock()
				}()
			}
			wg.Wait()

			// BUY path (minimize in + gas)
			var (
				buyIn, buyGas float64
				buyFee        uint32
				buyVenue      core.VenueID
				haveBuy       bool
			)
			wg = sync.WaitGroup{}
			mu = sync.Mutex{}
			for _, v := range venues {
				v := v
				wg.Add(1)
				go func() {
					defer wg.Done()
					inUSD, gasUSD, meta, err := v.Quoter.QuoteDexInUSD(ctx, quoteTokenAddr, baseTokenAddr, cfg.Trade.BaseQty, mid)
					if err != nil || inUSD == 0 {
						return
					}
					mu.Lock()
					if !haveBuy || (inUSD+gasUSD) < (buyIn+buyGas) {
						haveBuy = true
						buyIn, buyGas, buyFee, buyVenue = inUSD, gasUSD, meta.FeeTier, v.ID
					}
					mu.Unlock()
				}()
			}
			wg.Wait()

			if haveSell {
				imetrics.DexOutUSD.Set(sellOut)
				imetrics.GasUSD.Set(sellGas)
				log.Info("dex quote ok (sell)",
					zap.String("pair", cfg.Pair),
					zap.Float64("out_usd", sellOut),
					zap.Float64("gas_usd", sellGas),
					zap.Uint32("fee_tier", sellFee),
					zap.String("venue", string(sellVenue)),
				)
			}
			if haveBuy {
				log.Info("dex quote ok (buy)",
					zap.String("pair", cfg.Pair),
					zap.Float64("in_usd", buyIn),
					zap.Float64("gas_usd", buyGas),
					zap.Uint32("fee_tier", buyFee),
					zap.String("venue", string(buyVenue)),
				)
			}

			snap := Snapshot{
				BestAskCEX: ask,
				BestBidCEX: bid,

				DexOutUSD:      sellOut,
				GasSellUSD:     sellGas,
				DexSellFeeTier: sellFee,
				DexSellVenue:   sellVenue,

				DexInUSD:      buyIn,
				GasBuyUSD:     buyGas,
				DexBuyFeeTier: buyFee,
				DexBuyVenue:   buyVenue,

				Ts: time.Now(),
			}

			select {
			case out <- snap:
			case <-ctx.Done():
				log.Info("marketdata runner stopped while publishing")
				return
			default:
				log.Warn("snapshot channel is full; dropping snapshot")
			}
		}
	}
}
