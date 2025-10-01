package config

import (
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type RedisCfg struct {
	Addr      string `yaml:"addr"`
	DB        int    `yaml:"db"`
	Username  string `yaml:"username"`
	Password  string `yaml:"password"`
	Stream    string `yaml:"stream"`
	PubChan   string `yaml:"pubchan"`
	ActiveKey string `yaml:"active_key"`
	SnapNS    string `yaml:"snap_ns"`
}

type ArbBotCfg struct {
	MinPairs          int           `yaml:"-"`
	BootstrapLookback time.Duration `yaml:"-"`
	BootstrapPoll     time.Duration `yaml:"-"`
}

type DiscoveryCfg struct {
	FromRank         int    `yaml:"from_rank"`
	ToRank           int    `yaml:"to_rank"`
	Pick             int    `yaml:"pick"`
	CoinGeckoKey     string `yaml:"coingecko_key"`
	CoinGeckoVerbose bool   `yaml:"coingecko_verbose"`
}

type Config struct {
	Pair     string `yaml:"pair"`
	Scenario string `yaml:"scenario"`
	Mode     string `yaml:"mode"`
	DryRun   bool   `yaml:"dry_run"`

	ArbBot    ArbBotCfg    `yaml:"-"`
	Discovery DiscoveryCfg `yaml:"discovery"`

	MEXC struct {
		ApiKey            string  `yaml:"api_key"`
		ApiSecret         string  `yaml:"api_secret"`
		RestURL           string  `yaml:"rest_url"`
		WsURL             string  `yaml:"ws_url"`
		TakerFeeBps       int     `yaml:"taker_fee_bps"`
		WithdrawalFeeBase float64 `yaml:"withdrawal_fee_base"`
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
		Router   string   `yaml:"router"`
		QuoterV1 string   `yaml:"quoter_v1"`
		QuoterV2 string   `yaml:"quoter_v2"`
		WETH     string   `yaml:"weth"`
		USDT     string   `yaml:"usdt"`
		FeeTier  uint32   `yaml:"fee_tier"`
		FeeTiers []uint32 `yaml:"fee_tiers"`
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

	Metrics struct {
		ListenAddr string `yaml:"listen_addr"`
	} `yaml:"metrics"`

	Timings struct {
		QuoteIntervalMs int `yaml:"quote_interval_ms"`
		DetectorTickMs  int `yaml:"detector_tick_ms"`
	} `yaml:"timings"`

	Redis RedisCfg `yaml:"redis"`
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
	if c.Redis.Stream == "" {
		c.Redis.Stream = "book:stream"
	}
	if c.Redis.ActiveKey == "" {
		c.Redis.ActiveKey = "book:active"
	}
	if c.Redis.SnapNS == "" {
		c.Redis.SnapNS = "book:snap:"
	}
	return &c, nil
}

func (c *Config) QuoteInterval() time.Duration {
	return time.Duration(c.Timings.QuoteIntervalMs) * time.Millisecond
}

func (c *Config) DetectorTick() time.Duration {
	return time.Duration(c.Timings.DetectorTickMs) * time.Millisecond
}
