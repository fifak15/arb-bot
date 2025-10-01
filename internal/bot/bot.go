package bot

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/you/arb-bot/internal/config"
	"github.com/you/arb-bot/internal/connectors/cex/mexc"
	"github.com/you/arb-bot/internal/detector"
	"github.com/you/arb-bot/internal/dex/univ3"
	"github.com/you/arb-bot/internal/execution"
	"github.com/you/arb-bot/internal/marketdata"
	"github.com/you/arb-bot/internal/risk"
	"github.com/you/arb-bot/internal/types"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

import (
	"github.com/you/arb-bot/internal/discovery"
)

// Bot manages the application's lifecycle and components.
type Bot struct {
	cfg       *config.Config
	log       *zap.Logger
	rdb       *redis.Client
	discovery *discovery.Service
}

// New creates a new Bot instance.
func New(cfg *config.Config, log *zap.Logger) *Bot {
	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.Redis.Addr,
		DB:       cfg.Redis.DB,
		Username: cfg.Redis.Username,
		Password: cfg.Redis.Password,
	})
	return &Bot{
		cfg:       cfg,
		log:       log,
		rdb:       rdb,
		discovery: discovery.NewService(cfg, log),
	}
}

// Run starts the bot's main loop.
func (b *Bot) Run(ctx context.Context, runDiscovery bool) {
	if runDiscovery {
		if err := b.discovery.Run(ctx); err != nil {
			b.log.Fatal("pair discovery failed", zap.Error(err))
		}
		b.log.Info("pair discovery finished, bot will now start monitoring")
		return
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigs
		b.log.Warn("received signal, shutting down...")
		cancel()
	}()

	pairs, err := b.waitPairsFromRedis(ctx, b.cfg.ArbBot.MinPairs, b.cfg.ArbBot.BootstrapLookback, b.cfg.ArbBot.BootstrapPoll)
	if err != nil || len(pairs) == 0 {
		b.log.Fatal("failed to get pairs from Redis", zap.Error(err))
	}
	b.log.Info("got pairs from redis", zap.Int("count", len(pairs)))

	// 1) Start WebSocket feed for CEX prices and maintain a local cache.
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

	quoter, err := univ3.NewMultiQuoter(b.cfg, b.log)
	if err != nil {
		b.log.Fatal("failed to initialize uniswap multiquoter", zap.Error(err))
	}

	cex := &wsCEX{book: book}
	for _, pm := range pairs {
		pm := pm
		go b.runPairPipeline(ctx, pm, cex, quoter)
	}

	<-ctx.Done()
	b.log.Info("arb-bot finished")
}

func (b *Bot) runPairPipeline(
	ctx context.Context,
	pm pairMeta,
	cex *wsCEX,
	quoter univ3.Quoter,
) {
	// Create a local copy of the config for the specific pair.
	cfg := *b.cfg
	cfg.Pair = pm.Symbol

	mdCh := make(chan marketdata.Snapshot, 64)
	oppCh := make(chan types.Opportunity, 64)

	go marketdata.Run(ctx, &cfg, cex, quoter, mdCh, b.log)
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
		exec, err := execution.NewExecutor(&cfg, nil, router, riskEng, b.log)
		if err != nil {
			b.log.Fatal("failed to initialize executor", zap.String("pair", cfg.Pair), zap.Error(err))
		}
		go exec.Run(ctx, oppCh)
	}

	b.log.Info("pipeline started", zap.String("pair", cfg.Pair), zap.String("addr", pm.Addr))
}

type pairMeta struct {
	Symbol string
	Base   string
	Addr   string
	CgID   string
}

func (b *Bot) waitPairsFromRedis(ctx context.Context, minPairs int, lookback, poll time.Duration) ([]pairMeta, error) {
	deadline := time.Now().Add(lookback)
	seen := map[string]pairMeta{}

	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		now := time.Now().UnixMilli()
		since := now - int64(lookback/time.Millisecond)

		syms, err := b.rdb.ZRangeByScore(ctx, "pair:active", &redis.ZRangeBy{
			Min: strconv.FormatInt(since, 10), Max: "+inf",
		}).Result()
		if err != nil {
			b.log.Warn("redis ZRangeByScore(pair:active) failed", zap.Error(err))
			time.Sleep(poll)
			continue
		}

		for _, s := range syms {
			if _, ok := seen[s]; ok {
				continue
			}
			m, err := b.rdb.HGetAll(ctx, "pair:meta:"+s).Result()
			if err != nil || len(m) == 0 {
				continue
			}
			pm := pairMeta{
				Symbol: strings.ToUpper(m["symbol"]),
				Base:   strings.ToUpper(m["base"]),
				Addr:   m["addr"],
				CgID:   m["cg_id"],
			}
			if pm.Symbol != "" && pm.Addr != "" {
				seen[pm.Symbol] = pm
			}
			if len(seen) >= minPairs {
				break
			}
		}

		if len(seen) >= minPairs {
			break
		}
		if time.Now().After(deadline) && len(seen) > 0 {
			b.log.Warn("bootstrap timeout - starting with fewer pairs", zap.Int("pairs", len(seen)))
			break
		}
		time.Sleep(poll)
	}

	out := make([]pairMeta, 0, len(seen))
	for _, pm := range seen {
		out = append(out, pm)
	}
	return out, nil
}

// BookCache holds the latest bid/ask from the WebSocket feed.
type BookCache struct {
	mu   sync.RWMutex
	bids map[string]float64
	asks map[string]float64
}

// NewBookCache creates a new BookCache.
func NewBookCache() *BookCache {
	return &BookCache{
		bids: make(map[string]float64, 64),
		asks: make(map[string]float64, 64),
	}
}

// Set updates the bid/ask for a symbol.
func (bc *BookCache) Set(symbol string, bid, ask float64) {
	bc.mu.Lock()
	bc.bids[symbol] = bid
	bc.asks[symbol] = ask
	bc.mu.Unlock()
}

// BestBidAsk returns the best bid/ask for a symbol.
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

// wsCEX adapts BookCache to the marketdata.cexIface interface.
type wsCEX struct{ book *BookCache }

func (w *wsCEX) BestBidAsk(symbol string) (float64, float64, error) {
	return w.book.BestBidAsk(symbol)
}

// NewLogger creates a new logger instance.
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