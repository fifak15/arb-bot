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

type VenuePoint struct {
	Venue      core.VenueID `json:"venue"`
	SellPxUSD  float64      `json:"sellPxUSD"` // price per 1 base (base->USDT)
	BuyPxUSD   float64      `json:"buyPxUSD"`  // price per 1 base (USDT->base, implied)
	GasSellUSD float64      `json:"gasSellUSD"`
	GasBuyUSD  float64      `json:"gasBuyUSD"`
	FeeSell    uint32       `json:"feeSell"`
	FeeBuy     uint32       `json:"feeBuy"`
}

type Snapshot struct {
	BestAskCEX, BestBidCEX float64

	// “лучший” по всем DEX (как было раньше)
	DexOutUSD, GasSellUSD float64
	DexSellFeeTier        uint32
	DexSellVenue          core.VenueID

	DexInUSD, GasBuyUSD float64
	DexBuyFeeTier       uint32
	DexBuyVenue         core.VenueID

	// НОВОЕ: покомпактно по каждому venue
	Venues []VenuePoint

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
			// 1) CEX mid для пары
			bid, ask, err := cex.BestBidAsk(cfg.Pair)
			if err != nil || bid == 0 || ask == 0 {
				if err != nil {
					log.Warn("marketdata: CEX BestBidAsk failed", zap.Error(err))
				}
				continue
			}
			mid := 0.5 * (bid + ask)
			imetrics.CEXMid.Set(mid)

			// 2) Цена ETH в USD для конвертации газа
			ethUSD := 2000.0
			if eb, ea, eerr := cex.BestBidAsk("ETHUSDT"); eerr == nil && eb > 0 && ea > 0 {
				ethUSD = 0.5 * (eb + ea)
			}

			venues := core.Enabled(cfg.DEX.Venues)
			if len(venues) == 0 {
				log.Warn("marketdata: no DEX venues enabled")
				continue
			}

			baseQty := cfg.Trade.BaseQty
			type agg struct{ v VenuePoint }
			per := make(map[core.VenueID]*agg, len(venues))

			var (
				bestSellOut, bestSellGas float64
				bestSellFee              uint32
				bestSellVenue            core.VenueID
				haveSell                 bool

				bestBuyIn, bestBuyGas float64
				bestBuyFee            uint32
				bestBuyVenue          core.VenueID
				haveBuy               bool

				wg sync.WaitGroup
				mu sync.Mutex
			)

			// SELL path: base -> USDT
			for _, ven := range venues {
				ven := ven
				wg.Add(1)
				go func() {
					defer wg.Done()
					outUSD, gasUSD, meta, err := ven.Quoter.QuoteDexOutUSD(ctx, base, usdx, baseQty, ethUSD)
					if err == nil && outUSD > 0 {
						mu.Lock()
						a := per[ven.ID]
						if a == nil {
							a = &agg{}
							a.v.Venue = ven.ID
							per[ven.ID] = a
						}
						a.v.SellPxUSD = outUSD / baseQty
						a.v.GasSellUSD = gasUSD
						a.v.FeeSell = meta.FeeTier

						if !haveSell || (outUSD-gasUSD) > (bestSellOut-bestSellGas) {
							haveSell = true
							bestSellOut, bestSellGas, bestSellFee, bestSellVenue = outUSD, gasUSD, meta.FeeTier, ven.ID
						}
						mu.Unlock()
					}
				}()
			}
			wg.Wait()

			// BUY path: USDT -> base (exact output)
			wg = sync.WaitGroup{}
			for _, ven := range venues {
				ven := ven
				wg.Add(1)
				go func() {
					defer wg.Done()
					inUSD, gasUSD, meta, err := ven.Quoter.QuoteDexInUSD(ctx, usdx, base, baseQty, ethUSD)
					if err == nil && inUSD > 0 {
						mu.Lock()
						a := per[ven.ID]
						if a == nil {
							a = &agg{}
							a.v.Venue = ven.ID
							per[ven.ID] = a
						}
						a.v.BuyPxUSD = inUSD / baseQty
						a.v.GasBuyUSD = gasUSD
						a.v.FeeBuy = meta.FeeTier

						if !haveBuy || (inUSD+gasUSD) < (bestBuyIn+bestBuyGas) {
							haveBuy = true
							bestBuyIn, bestBuyGas, bestBuyFee, bestBuyVenue = inUSD, gasUSD, meta.FeeTier, ven.ID
						}
						mu.Unlock()
					}
				}()
			}
			wg.Wait()

			if !haveSell && !haveBuy {
				log.Debug("marketdata: no valid quotes from any venue",
					zap.String("pair", cfg.Pair),
					zap.Float64("eth_usd", ethUSD),
				)
				continue
			}

			flat := make([]VenuePoint, 0, len(per))
			for _, a := range per {
				flat = append(flat, a.v)
			}

			snap := Snapshot{
				BestAskCEX: ask,
				BestBidCEX: bid,

				DexOutUSD:      bestSellOut,
				GasSellUSD:     bestSellGas,
				DexSellFeeTier: bestSellFee,
				DexSellVenue:   bestSellVenue,

				DexInUSD:      bestBuyIn,
				GasBuyUSD:     bestBuyGas,
				DexBuyFeeTier: bestBuyFee,
				DexBuyVenue:   bestBuyVenue,

				Venues: flat,
				Ts:     time.Now(),
			}

			select {
			case out <- snap:
			default:
				log.Warn("marketdata: snapshot channel full; dropping")
			}
		}
	}
}
