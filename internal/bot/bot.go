package bot

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

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
	pm discovery.PairMeta,
	cex *wsCEX,
	quoter univ3.Quoter,
) {
	cfg := *b.cfg
	cfg.Pair = pm.Symbol

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
		exec, err := execution.NewExecutor(&cfg, nil, router, riskEng, b.log)
		if err != nil {
			b.log.Fatal("failed to initialize executor", zap.String("pair", cfg.Pair), zap.Error(err))
		}
		go exec.Run(ctx, oppCh)
	}

	b.log.Info("pipeline started", zap.String("pair", cfg.Pair), zap.String("addr", pm.Addr))
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
