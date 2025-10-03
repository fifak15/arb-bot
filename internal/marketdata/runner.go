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

	DexOutUSD, GasSellUSD float64
	DexSellFeeTier        uint32
	DexSellVenue          core.VenueID

	DexInUSD, GasBuyUSD float64
	DexBuyFeeTier       uint32
	DexBuyVenue         core.VenueID

	Ts time.Time
}

type cexIface interface {
	BestBidAsk(symbol string) (bid, ask float64, err error)
}

func Run(ctx context.Context, cfg *config.Config, pm discovery.PairMeta, cex cexIface, out chan<- Snapshot, log *zap.Logger) {
	base := common.HexToAddress(pm.Addr)
	usdx := common.HexToAddress(cfg.DEX.USDT)

	t := time.NewTicker(cfg.QuoteInterval())
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			bid, ask, err := cex.BestBidAsk(cfg.Pair)
			if err != nil || bid == 0 || ask == 0 {
				if err != nil {
					log.Warn("marketdata: CEX BestBidAsk failed", zap.Error(err))
				}
				continue
			}
			imetrics.CEXMid.Set(0.5 * (bid + ask))

			venues := core.Enabled(cfg.DEX.Venues)
			if len(venues) == 0 {
				log.Warn("marketdata: no DEX venues enabled")
				continue
			}

			var (
				sellOut, sellGas float64
				sellFee          uint32
				sellVenue        core.VenueID
				haveSell         bool

				buyIn, buyGas float64
				buyFee        uint32
				buyVenue      core.VenueID
				haveBuy       bool

				wg sync.WaitGroup
				mu sync.Mutex
			)

			for _, v := range venues {
				v := v
				wg.Add(1)
				go func() {
					defer wg.Done()
					outUSD, gasUSD, meta, err := v.Quoter.QuoteDexOutUSD(ctx, base, usdx, cfg.Trade.BaseQty, 0.5*(bid+ask))
					if err == nil && outUSD > 0 {
						mu.Lock()
						if !haveSell || (outUSD-gasUSD) > (sellOut-sellGas) {
							haveSell = true
							sellOut, sellGas, sellFee, sellVenue = outUSD, gasUSD, meta.FeeTier, v.ID
						}
						mu.Unlock()
					}
				}()
			}
			wg.Wait()

			wg = sync.WaitGroup{}
			for _, v := range venues {
				v := v
				wg.Add(1)
				go func() {
					defer wg.Done()
					inUSD, gasUSD, meta, err := v.Quoter.QuoteDexInUSD(ctx, usdx, base, cfg.Trade.BaseQty, 0.5*(bid+ask))
					if err == nil && inUSD > 0 {
						mu.Lock()
						if !haveBuy || (inUSD+gasUSD) < (buyIn+buyGas) {
							haveBuy = true
							buyIn, buyGas, buyFee, buyVenue = inUSD, gasUSD, meta.FeeTier, v.ID
						}
						mu.Unlock()
					}
				}()
			}
			wg.Wait()

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
			default:
				log.Warn("marketdata: snapshot channel full; dropping")
			}
		}
	}
}
