package config

import (
	"os"
	"time"

	"github.com/you/arb-bot/internal/dex/core"
	"gopkg.in/yaml.v3"
)

type ArbBotCfg struct {
	MinPairs          int           `yaml:"-"`
	BootstrapLookback time.Duration `yaml:"-"`
	BootstrapPoll     time.Duration `yaml:"-"`
}

type Config struct {
	Pair     string    `yaml:"pair"`
	Scenario string    `yaml:"scenario"`
	Mode     string    `yaml:"mode"`
	DryRun   bool      `yaml:"dry_run"`
	ArbBot   ArbBotCfg `yaml:"-"`

	Discovery struct {
		FromRank int `yaml:"from_rank"`
		ToRank   int `yaml:"to_rank"`
		Pick     int `yaml:"pick"`
	} `yaml:"discovery"`

	MEXC struct {
		ApiKey    string `yaml:"api_key"`
		ApiSecret string `yaml:"api_secret"`
		RestURL   string `yaml:"rest_url"`
		WsURL     string `yaml:"ws_url"`
	} `yaml:"mexc"`

	Chain struct {
		Network            string  `yaml:"network"`
		RPCHTTP            string  `yaml:"rpc_http"`
		RPCWS              string  `yaml:"rpc_ws"`
		WalletPK           string  `yaml:"wallet_pk"`
		MaxPriorityFeeGwei float64 `yaml:"max_priority_fee_gwei"`
		GasLimitSwap       uint64  `yaml:"gas_limit_swap"`
	} `yaml:"chain"`

	DEX struct {
		USDT     string         `yaml:"usdt"`
		QuoterV2 string         `yaml:"quoter_v2"`
		FeeTiers []uint32       `yaml:"fee_tiers"`
		Venues   []core.VenueID `yaml:"venues"`

		Sushi struct {
			Router string `yaml:"router"`
		} `yaml:"sushi"`
		CamelotV2 struct {
			Router string `yaml:"router"`
		} `yaml:"camelot_v2"`
	} `yaml:"dex"`

	Risk struct {
		MaxSlippageBps int     `yaml:"max_slippage_bps"`
		MinProfitUSD   float64 `yaml:"min_profit_usd"`
		MinROIBps      float64 `yaml:"min_roi_bps"`
		MaxGasUSD      float64 `yaml:"max_gas_usd"`
		MinFillRatio   float64 `yaml:"min_fill_ratio"`
	} `yaml:"risk"`

	Trade struct {
		BaseQty float64 `yaml:"base_qty"`
	} `yaml:"trade"`

	Timings struct {
		QuoteIntervalMs int `yaml:"quote_interval_ms"`
		DetectorTickMs  int `yaml:"detector_tick_ms"`
	} `yaml:"timings"`
}

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, err
	}

	if c.Timings.QuoteIntervalMs == 0 {
		c.Timings.QuoteIntervalMs = 300
	}
	if c.Timings.DetectorTickMs == 0 {
		c.Timings.DetectorTickMs = 150
	}
	if c.Risk.MinFillRatio == 0 {
		c.Risk.MinFillRatio = 0.7
	}
	if len(c.DEX.Venues) == 0 {
		c.DEX.Venues = []core.VenueID{core.VenueUniswapV3, core.VenueSushiV2, core.VenueCamelotV2}
	}
	return &c, nil
}

func (c *Config) QuoteInterval() time.Duration {
	return time.Duration(c.Timings.QuoteIntervalMs) * time.Millisecond
}
func (c *Config) DetectorTick() time.Duration {
	return time.Duration(c.Timings.DetectorTickMs) * time.Millisecond
}
