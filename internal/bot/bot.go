package bot

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/common" // ⬅️ добавили
	"github.com/you/arb-bot/internal/config"
	"github.com/you/arb-bot/internal/connectors/cex/mexc"
	"github.com/you/arb-bot/internal/detector"
	"github.com/you/arb-bot/internal/dex/univ3"
	"github.com/you/arb-bot/internal/discovery"
	"github.com/you/arb-bot/internal/execution"
	"github.com/you/arb-bot/internal/marketdata"
	"github.com/you/arb-bot/internal/risk"
	"github.com/you/arb-bot/internal/types"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// Bot manages the application's lifecycle and components.
type Bot struct {
	cfg       *config.Config
	log       *zap.Logger
	discovery *discovery.Service
}

func New(cfg *config.Config, log *zap.Logger) *Bot {
	return &Bot{
		cfg:       cfg,
		log:       log,
		discovery: discovery.NewService(cfg, log),
	}
}

func (b *Bot) Run(ctx context.Context, _ bool) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// graceful shutdown
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigs
		b.log.Warn("received signal, shutting down...")
		cancel()
	}()

	// 1) discovery
	pairs, err := b.discovery.Discover(ctx)
	if err != nil {
		b.log.Fatal("pair discovery failed", zap.Error(err))
	}
	if len(pairs) == 0 {
		b.log.Fatal("no pairs discovered")
	}
	b.log.Info("discovered pairs", zap.Int("count", len(pairs)))

	symbols := make([]string, 0, len(pairs))
	for _, pm := range pairs {
		symbols = append(symbols, pm.Symbol)
	}

	wsURL := b.cfg.MEXC.WsURL
	if wsURL == "" {
		wsURL = "wss://wbs-api.mexc.com/ws"
	}

	book := NewBookCache()
	bt := mexc.NewWS(wsURL)
	wsStream, err := bt.SubscribeBookTicker(ctx, symbols)
	if err != nil {
		b.log.Fatal("failed to subscribe to book ticker", zap.Error(err))
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case t := <-wsStream:
				book.Set(t.Symbol, t.Bid, t.Ask)
			}
		}
	}()
	b.log.Info("subscribed to WS book ticker", zap.Strings("symbols", symbols))

	bootstrap := b.cfg.ArbBot.BootstrapLookback
	if bootstrap == 0 {
		bootstrap = 5 * time.Second
	}
	missing := waitWSBootstrap(ctx, book, symbols, bootstrap, b.log)
	if len(missing) > 0 {
		b.log.Warn("WS bootstrap timeout, continue with partial set",
			zap.Int("missing", len(missing)),
			zap.Strings("symbols_missing", missing),
		)
	} else {
		b.log.Info("WS book ready for all symbols")
	}

	quoter, err := univ3.NewMultiQuoter(b.cfg, b.log)
	if err != nil {
		b.log.Fatal("failed to initialize uniswap multiquoter", zap.Error(err))
	}

	cex := &wsCEX{book: book}

	tiersToTest := b.cfg.DEX.FeeTiers
	if len(tiersToTest) == 0 {
		tiersToTest = []uint32{100, 500, 3000, 10000}
	}
	usdt := common.HexToAddress(b.cfg.DEX.USDT)
	if usdt == (common.Address{}) {
		b.log.Fatal("DEX.USDT address is empty in config")
	}

	filtered := make([]discovery.PairMeta, 0, 3)
	perPairTiers := make(map[string][]uint32, 3)

	for _, pm := range pairs {
		base := common.HexToAddress(pm.Addr)

		present, _, err := univ3.CheckAvailableFeeTiers(ctx, b.cfg.Chain.RPCHTTP, base, usdt, tiersToTest)
		if err != nil {
			b.log.Debug("fee tiers check failed", zap.String("pair", pm.Symbol), zap.Error(err))
			continue
		}
		if len(present) == 0 {
			b.log.Debug("no direct USDT pool on given tiers", zap.String("pair", pm.Symbol))
			continue
		}

		perPairTiers[pm.Symbol] = present
		filtered = append(filtered, pm)
		b.log.Info("pair allowed (has USDT pool)", zap.String("pair", pm.Symbol), zap.Uint32s("tiers", present))

		if len(filtered) == 3 {
			break
		}
	}

	if len(filtered) == 0 {
		b.log.Fatal("no pairs with direct USDT pools on given fee tiers; aborting")
	}
	if len(filtered) < 3 {
		b.log.Warn("less than 3 pairs qualified; proceeding with what we have", zap.Int("count", len(filtered)))
	}

	for _, pm := range filtered {
		pm := pm
		ft := perPairTiers[pm.Symbol]
		go b.runPairPipeline(ctx, pm, cex, quoter, ft)
	}

	<-ctx.Done()
	b.log.Info("arb-bot finished")
}

func (b *Bot) runPairPipeline(
	ctx context.Context,
	pm discovery.PairMeta,
	cex *wsCEX,
	quoter univ3.Quoter,
	feeTiersOverride []uint32, // ⬅️ добавили
) {
	cfg := *b.cfg
	cfg.Pair = pm.Symbol
	if len(feeTiersOverride) > 0 {
		cfg.DEX.FeeTiers = feeTiersOverride
	}

	mdCh := make(chan marketdata.Snapshot, 64)
	oppCh := make(chan types.Opportunity, 64)

	go marketdata.Run(ctx, &cfg, pm, cex, quoter, mdCh, b.log)
	go detector.Run(ctx, &cfg, mdCh, oppCh, b.log)

	if cfg.DryRun {
		b.log.Warn("DRY-RUN: no real orders/swaps will be sent", zap.String("pair", cfg.Pair))
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case opp := <-oppCh:
					b.log.Info("opportunity",
						zap.String("pair", cfg.Pair),
						zap.Float64("qty_base", opp.QtyBase),
						zap.Float64("buy_px_cex", opp.BuyPxCEX),
						zap.Float64("sell_px_cex", opp.SellPxCEX),
						zap.Float64("dex_out_usd", opp.DexOutUSD),
						zap.Float64("dex_in_usd", opp.DexInUSD),
						zap.Float64("gas_usd", opp.GasUSD),
						zap.Float64("net_usd", opp.NetUSD),
						zap.Float64("roi", opp.ROI),
						zap.Uint32("dex_fee_tier", opp.DexFeeTier),
						zap.Time("ts", opp.Ts),
					)
				}
			}
		}()
	} else {
		router, err := univ3.NewRouter(&cfg, b.log)
		if err != nil {
			b.log.Fatal("failed to initialize uniswap router", zap.String("pair", cfg.Pair), zap.Error(err))
		}
		riskEng := risk.NewEngine(&cfg)
		baseAddr := common.HexToAddress(pm.Addr)
		exec, err := execution.NewExecutor(&cfg, nil, router, riskEng, b.log, baseAddr)
		if err != nil {
			b.log.Fatal("failed to initialize executor", zap.String("pair", cfg.Pair), zap.Error(err))
		}
		go exec.Run(ctx, oppCh)
	}

	b.log.Info("pipeline started", zap.String("pair", cfg.Pair), zap.String("addr", pm.Addr))
}

type BookCache struct {
	mu   sync.RWMutex
	bids map[string]float64
	asks map[string]float64
}

func NewBookCache() *BookCache {
	return &BookCache{
		bids: make(map[string]float64, 64),
		asks: make(map[string]float64, 64),
	}
}

func (bc *BookCache) Set(symbol string, bid, ask float64) {
	bc.mu.Lock()
	bc.bids[symbol] = bid
	bc.asks[symbol] = ask
	bc.mu.Unlock()
}

func (bc *BookCache) BestBidAsk(symbol string) (float64, float64, error) {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	bid := bc.bids[symbol]
	ask := bc.asks[symbol]
	if bid == 0 || ask == 0 {
		return 0, 0, fmt.Errorf("empty book for %s", symbol)
	}
	return bid, ask, nil
}

func (bc *BookCache) Has(symbol string) bool {
	bc.mu.RLock()
	_, ok1 := bc.bids[symbol]
	_, ok2 := bc.asks[symbol]
	bc.mu.RUnlock()
	return ok1 && ok2
}

func waitWSBootstrap(ctx context.Context, book *BookCache, symbols []string, timeout time.Duration, log *zap.Logger) []string {
	deadline := time.Now().Add(timeout)
	missing := make(map[string]struct{}, len(symbols))
	for _, s := range symbols {
		missing[s] = struct{}{}
	}
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		for s := range missing {
			if book.Has(s) {
				delete(missing, s)
			}
		}
		if len(missing) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			out := make([]string, 0, len(missing))
			for s := range missing {
				out = append(out, s)
			}
			sort.Strings(out)
			return out
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

type wsCEX struct{ book *BookCache }

func (w *wsCEX) BestBidAsk(symbol string) (float64, float64, error) {
	return w.book.BestBidAsk(symbol)
}

func NewLogger() (*zap.Logger, error) {
	cfg := zap.NewProductionConfig()
	cfg.Level = zap.NewAtomicLevelAt(zap.DebugLevel)
	cfg.Encoding = "json"
	cfg.EncoderConfig.TimeKey = "ts"
	cfg.EncoderConfig.LevelKey = "level"
	cfg.EncoderConfig.MessageKey = "msg"
	cfg.EncoderConfig.CallerKey = "caller"
	cfg.EncoderConfig.StacktraceKey = "stacktrace"
	cfg.EncoderConfig.EncodeTime = zapcore.TimeEncoderOfLayout(time.RFC3339)
	return cfg.Build()
}
