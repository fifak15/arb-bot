package main

import (
	"context"
	"flag"
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
	"github.com/you/arb-bot/internal/detector"
	"github.com/you/arb-bot/internal/dex/univ3"
	"github.com/you/arb-bot/internal/execution"
	"github.com/you/arb-bot/internal/marketdata"
	"github.com/you/arb-bot/internal/risk"
	"github.com/you/arb-bot/internal/types"

	// WS-клиент MEXC уже есть у тебя в репозитории
	"github.com/you/arb-bot/internal/connectors/cex/mexc"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

/*************** logger ***************/
func newLogger() (*zap.Logger, error) {
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

/*************** Redis helpers: ждём пары из кеша ***************/
type pairMeta struct {
	Symbol string
	Base   string
	Addr   string
	CgID   string
}

func waitPairsFromRedis(ctx context.Context, rdb *redis.Client, minPairs int, lookback time.Duration, poll time.Duration, log *zap.Logger) ([]pairMeta, error) {
	deadline := time.Now().Add(lookback)
	seen := map[string]pairMeta{}

	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		now := time.Now().UnixMilli()
		since := now - int64(lookback/time.Millisecond)

		// читаем активные пары за lookback
		syms, err := rdb.ZRangeByScore(ctx, "pair:active", &redis.ZRangeBy{
			Min: strconv.FormatInt(since, 10), Max: "+inf",
		}).Result()
		if err != nil {
			log.Warn("redis ZRangeByScore(pair:active) failed", zap.Error(err))
			time.Sleep(poll)
			continue
		}

		for _, s := range syms {
			if _, ok := seen[s]; ok {
				continue
			}
			m, err := rdb.HGetAll(ctx, "pair:meta:"+s).Result()
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
			log.Warn("bootstrap timeout — стартуем с меньшим количеством пар", zap.Int("pairs", len(seen)))
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

/*************** WS bookTicker cache + cex adapter ***************/
// BookCache держит свежие bid/ask из WS MEXC.
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

// wsCEX адаптирует BookCache под интерфейс marketdata.cexIface (BestBidAsk).
type wsCEX struct{ book *BookCache }

func (w *wsCEX) BestBidAsk(symbol string) (float64, float64, error) {
	return w.book.BestBidAsk(symbol)
}

func runPairPipeline(
	ctx context.Context,
	rootCfg *config.Config,
	pm pairMeta,
	cex *wsCEX,
	quoter univ3.Quoter,
	log *zap.Logger,
) {
	// локальная копия конфига под конкретную пару
	cfg := *rootCfg
	cfg.Pair = pm.Symbol

	mdCh := make(chan marketdata.Snapshot, 64)
	oppCh := make(chan types.Opportunity, 64)

	// marketdata → detector
	go marketdata.Run(ctx, &cfg, cex, quoter, mdCh, log) // берёт BestBidAsk из wsCEX (WS-cache) :contentReference[oaicite:3]{index=3}
	go detector.Run(ctx, &cfg, mdCh, oppCh, log)         // считает net/roi и решает направление :contentReference[oaicite:4]{index=4}

	if cfg.DryRun {
		log.Warn("DRY-RUN: реальные ордера/свапы отправляться не будут", zap.String("pair", cfg.Pair))
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case opp := <-oppCh:
					log.Info("opportunity",
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
		// полноценное исполнение сделки
		router, err := univ3.NewRouter(&cfg, log)
		if err != nil {
			log.Fatal("инициализация роутера Uniswap", zap.String("pair", cfg.Pair), zap.Error(err))
		}
		riskEng := risk.NewEngine(&cfg)
		exec, err := execution.NewExecutor(&cfg, nil /*не нужен PlaceIOC тут*/, router, riskEng, log)
		if err != nil {
			log.Fatal("инициализация исполнителя", zap.String("pair", cfg.Pair), zap.Error(err))
		}
		go exec.Run(ctx, oppCh) // исполняет лучший сценарий CEX/DEX (логика у тебя уже есть) :contentReference[oaicite:5]{index=5}
	}

	log.Info("pipeline стартовал", zap.String("pair", cfg.Pair), zap.String("addr", pm.Addr))
}

/*************** main ***************/
func main() {
	cfgPath := flag.String("config", "./config.yaml", "путь к конфигу")
	minPairs := flag.Int("min-pairs", 15, "минимум пар, чтобы стартовать мониторинг")
	bootstrapLookback := flag.Duration("bootstrap-lookback", 30*time.Second, "как далеко назад смотреть активные пары")
	bootstrapPoll := flag.Duration("bootstrap-poll", 500*time.Millisecond, "частота опроса Redis на старте")
	flag.Parse()

	log, err := newLogger()
	if err != nil {
		panic(err)
	}
	defer log.Sync()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatal("ошибка загрузки конфига", zap.Error(err))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() { <-sigs; log.Warn("получен сигнал, выходим…"); cancel() }()

	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.Redis.Addr,
		DB:       cfg.Redis.DB,
		Username: cfg.Redis.Username,
		Password: cfg.Redis.Password,
	})
	pairs, err := waitPairsFromRedis(ctx, rdb, *minPairs, *bootstrapLookback, *bootstrapPoll, log)
	if err != nil || len(pairs) == 0 {
		log.Fatal("не получили пары из Redis", zap.Error(err))
	}
	log.Info("получены пары из Redis", zap.Int("count", len(pairs)))

	// 1) поднимаем WS на MEXC ровно по этим парам и ведём локальный кэш bid/ask
	symbols := make([]string, 0, len(pairs))
	for _, pm := range pairs {
		symbols = append(symbols, pm.Symbol)
	}
	wsURL := cfg.MEXC.WsURL
	if wsURL == "" {
		wsURL = "wss://wbs-api.mexc.com/ws"
	}
	book := NewBookCache()
	bt := mexc.NewWS(wsURL) // твой WS-клиент
	wsStream, err := bt.SubscribeBookTicker(ctx, symbols)
	if err != nil {
		log.Fatal("SubscribeBookTicker", zap.Error(err))
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
	log.Info("WS bookTicker подписан", zap.Strings("symbols", symbols))

	quoter, err := univ3.NewSlot0Quoter(cfg, log)
	if err != nil {
		log.Fatal("инициализация квотера Uniswap", zap.Error(err))
	}

	cex := &wsCEX{book: book}
	for _, pm := range pairs {
		pm := pm
		go runPairPipeline(ctx, cfg, pm, cex, quoter, log)
	}

	<-ctx.Done()
	log.Info("arb-bot завершён")
}
