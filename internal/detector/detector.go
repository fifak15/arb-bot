package detector

import (
	"context"
	"strings"
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
			// Determine which scenarios to check based on config
			checkCexBuy := strings.ToUpper(cfg.Scenario) != string(types.DEXBuyCEXSell)
			checkDexBuy := strings.ToUpper(cfg.Scenario) != string(types.CEXBuyDEXSell)

			if checkCexBuy {
				evaluateCexBuyDexSell(cfg, last, out, log)
			}
			if checkDexBuy {
				evaluateDexBuyCexSell(cfg, last, out, log)
			}
		}
	}
}

// evaluateCexBuyDexSell checks for the "Buy on CEX, Sell on DEX" opportunity.
func evaluateCexBuyDexSell(cfg *config.Config, snap marketdata.Snapshot, out chan<- types.Opportunity, log *zap.Logger) {
	if snap.BestAskCEX == 0 || snap.DexOutUSD == 0 {
		return
	}

	q := cfg.Trade.BaseQty
	takerFee := snap.BestAskCEX * q * float64(cfg.MEXC.TakerFeeBps) / 10000.0
	cost := snap.BestAskCEX*q + takerFee

	midCEX := 0.5 * (snap.BestBidCEX + snap.BestAskCEX)
	withdrawUSDCost := midCEX * cfg.MEXC.WithdrawalFeeBase
	net := snap.DexOutUSD - cost - snap.GasSellUSD - withdrawUSDCost

	roi := 0.0
	if cost > 0 {
		roi = net / cost
	}

	if net >= cfg.Risk.MinProfitUSD && roi >= (cfg.Risk.MinROIBps/10000.0) {
		log.Info("Opportunity found: CEX_BUY_DEX_SELL", zap.Float64("net_usd", net), zap.Float64("roi_bps", roi*10000))
		out <- types.Opportunity{
			Direction:  types.CEXBuyDEXSell,
			QtyBase:    q,
			BuyPxCEX:   snap.BestAskCEX,
			DexOutUSD:  snap.DexOutUSD,
			GasUSD:     snap.GasSellUSD,
			DexFeeTier: snap.DexSellFeeTier,
			NetUSD:     net,
			ROI:        roi,
			Ts:         time.Now(),
		}
	}
}

// evaluateDexBuyCexSell checks for the "Buy on DEX, Sell on CEX" opportunity.
func evaluateDexBuyCexSell(cfg *config.Config, snap marketdata.Snapshot, out chan<- types.Opportunity, log *zap.Logger) {
	if snap.BestBidCEX == 0 || snap.DexInUSD == 0 {
		return
	}

	q := cfg.Trade.BaseQty
	cost := snap.DexInUSD + snap.GasBuyUSD

	takerFee := snap.BestBidCEX * q * float64(cfg.MEXC.TakerFeeBps) / 10000.0
	revenue := snap.BestBidCEX*q - takerFee

	midCEX := 0.5 * (snap.BestBidCEX + snap.BestAskCEX)
	withdrawUSDCost := midCEX * cfg.MEXC.WithdrawalFeeBase
	net := revenue - cost - withdrawUSDCost

	roi := 0.0
	if cost > 0 {
		roi = net / cost
	}

	if net >= cfg.Risk.MinProfitUSD && roi >= (cfg.Risk.MinROIBps/10000.0) {
		log.Info("Opportunity found: DEX_BUY_CEX_SELL", zap.Float64("net_usd", net), zap.Float64("roi_bps", roi*10000))
		out <- types.Opportunity{
			Direction:  types.DEXBuyCEXSell,
			QtyBase:    q,
			SellPxCEX:  snap.BestBidCEX,
			DexInUSD:   snap.DexInUSD,
			GasUSD:     snap.GasBuyUSD,
			DexFeeTier: snap.DexBuyFeeTier,
			NetUSD:     net,
			ROI:        roi,
			Ts:         time.Now(),
		}
	}
}