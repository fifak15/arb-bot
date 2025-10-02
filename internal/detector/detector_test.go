package detector

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/you/arb-bot/internal/config"
	"github.com/you/arb-bot/internal/marketdata"
	"github.com/you/arb-bot/internal/types"
	"go.uber.org/zap"
)

func newTestConfig() *config.Config {
	return &config.Config{
		Trade: struct {
			BaseQty float64 `yaml:"base_qty"`
		}{BaseQty: 1.0},
		MEXC: struct {
			ApiKey            string  `yaml:"api_key"`
			ApiSecret         string  `yaml:"api_secret"`
			RestURL           string  `yaml:"rest_url"`
			WsURL             string  `yaml:"ws_url"`
			TakerFeeBps       int     `yaml:"taker_fee_bps"`
			WithdrawalFeeBase float64 `yaml:"withdrawal_fee_base"`
		}{TakerFeeBps: 10, WithdrawalFeeBase: 0.0001},
		Risk: struct {
			MaxSlippageBps int     `yaml:"max_slippage_bps"`
			MinProfitUSD   float64 `yaml:"min_profit_usd"`
			MinROIBps      float64 `yaml:"min_roi_bps"`
			MaxGasUSD      float64 `yaml:"max_gas_usd"`
			MinFillRatio   float64 `yaml:"min_fill_ratio"`
		}{MinProfitUSD: 10.0, MinROIBps: 10.0},
	}
}

func TestEvaluateCexBuyDexSell_Profitable(t *testing.T) {
	cfg := newTestConfig()
	snap := marketdata.Snapshot{
		BestAskCEX: 2000.0,
		DexOutUSD:  2100.0,
		GasSellUSD: 5.0,
	}
	out := make(chan types.Opportunity, 1)
	log := zap.NewNop()

	evaluateCexBuyDexSell(cfg, snap, out, log)

	select {
	case opp := <-out:
		assert.Equal(t, types.CEXBuyDEXSell, opp.Direction)
		assert.Greater(t, opp.NetUSD, 0.0)
	default:
		t.Fatal("expected an opportunity, but got none")
	}
}

func TestEvaluateCexBuyDexSell_Unprofitable(t *testing.T) {
	cfg := newTestConfig()
	snap := marketdata.Snapshot{
		BestAskCEX: 2000.0,
		DexOutUSD:  2010.0, // Not enough to cover fees and gas
		GasSellUSD: 5.0,
	}
	out := make(chan types.Opportunity, 1)
	log := zap.NewNop()

	evaluateCexBuyDexSell(cfg, snap, out, log)

	select {
	case <-out:
		t.Fatal("expected no opportunity, but got one")
	case <-time.After(100 * time.Millisecond):
		// Success
	}
}

func TestEvaluateDexBuyCexSell_Profitable(t *testing.T) {
	cfg := newTestConfig()
	snap := marketdata.Snapshot{
		BestBidCEX: 2100.0,
		DexInUSD:   2000.0,
		GasBuyUSD:  5.0,
	}
	out := make(chan types.Opportunity, 1)
	log := zap.NewNop()

	evaluateDexBuyCexSell(cfg, snap, out, log)

	select {
	case opp := <-out:
		assert.Equal(t, types.DEXBuyCEXSell, opp.Direction)
		assert.Greater(t, opp.NetUSD, 0.0)
	default:
		t.Fatal("expected an opportunity, but got none")
	}
}

func TestEvaluateDexBuyCexSell_Unprofitable(t *testing.T) {
	cfg := newTestConfig()
	snap := marketdata.Snapshot{
		BestBidCEX: 2010.0,
		DexInUSD:   2000.0, // Not enough to cover fees and gas
		GasBuyUSD:  5.0,
	}
	out := make(chan types.Opportunity, 1)
	log := zap.NewNop()

	evaluateDexBuyCexSell(cfg, snap, out, log)

	select {
	case <-out:
		t.Fatal("expected no opportunity, but got one")
	case <-time.After(100 * time.Millisecond):
		// Success
	}
}

func TestEvaluate_ZeroPrice(t *testing.T) {
	cfg := newTestConfig()
	log := zap.NewNop()
	out := make(chan types.Opportunity, 1)

	snap1 := marketdata.Snapshot{BestAskCEX: 0, DexOutUSD: 2000}
	evaluateCexBuyDexSell(cfg, snap1, out, log)
	select {
	case <-out:
		t.Fatal("expected no opportunity with zero CEX price")
	default:
	}

	// Test DEX_BUY_CEX_SELL with zero price
	snap2 := marketdata.Snapshot{BestBidCEX: 2000, DexInUSD: 0}
	evaluateDexBuyCexSell(cfg, snap2, out, log)
	select {
	case <-out:
		t.Fatal("expected no opportunity with zero DEX price")
	default:
	}
}
