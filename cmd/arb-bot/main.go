package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/you/arb-bot/internal/config"
	"github.com/you/arb-bot/internal/connectors/cex/mexc"
	"github.com/you/arb-bot/internal/detector"
	"github.com/you/arb-bot/internal/dex/univ3"
	"github.com/you/arb-bot/internal/execution"
	"github.com/you/arb-bot/internal/marketdata"
	"github.com/you/arb-bot/internal/metrics"
	"github.com/you/arb-bot/internal/risk"
	"github.com/you/arb-bot/internal/types"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func newRussianLogger() (*zap.Logger, error) {
	ruLevel := func(l zapcore.Level, enc zapcore.PrimitiveArrayEncoder) {
		switch l {
		case zapcore.DebugLevel:
			enc.AppendString("debug")
		case zapcore.InfoLevel:
			enc.AppendString("info")
		case zapcore.WarnLevel:
			enc.AppendString("warning")
		case zapcore.ErrorLevel:
			enc.AppendString("error")
		case zapcore.DPanicLevel, zapcore.PanicLevel, zapcore.FatalLevel:
			enc.AppendString("fatality")
		default:
			enc.AppendString(l.String())
		}
	}
	ruTime := func(t time.Time, enc zapcore.PrimitiveArrayEncoder) {
		enc.AppendString(t.Format(time.RFC3339))
	}

	cfg := zap.Config{
		Level:       zap.NewAtomicLevelAt(zap.DebugLevel),
		Development: false,
		Encoding:    "json",
		EncoderConfig: zapcore.EncoderConfig{
			TimeKey:        "время",
			LevelKey:       "уровень",
			NameKey:        "лог",
			CallerKey:      "файл",
			MessageKey:     "сообщение",
			StacktraceKey:  "стек",
			LineEnding:     zapcore.DefaultLineEnding,
			EncodeLevel:    ruLevel,
			EncodeTime:     ruTime,
			EncodeDuration: zapcore.MillisDurationEncoder,
			EncodeCaller:   zapcore.ShortCallerEncoder,
		},
		OutputPaths:      []string{"stdout"},
		ErrorOutputPaths: []string{"stderr"},
	}
	return cfg.Build()
}

func main() {
	cfgPath := flag.String("config", "./config.yaml", "путь к конфигу")
	flag.Parse()

	logger, err := newRussianLogger()
	if err != nil {
		panic(err)
	}
	defer logger.Sync()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		logger.Fatal("ошибка загрузки конфига", zap.Error(err))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigs
		logger.Warn("получен сигнал, выходим…")
		cancel()
	}()
	metrics.Serve(ctx, cfg.Metrics.ListenAddr, nil, logger)
	logger.Info("распознаны fee-тиры DEX", zap.Uint32s("тиры", cfg.DEX.FeeTiers))

	cex, err := mexc.NewClient(cfg, logger)
	if err != nil {
		logger.Fatal("инициализация MEXC", zap.Error(err))
	}
	quoter, err := univ3.NewSlot0Quoter(cfg, logger)
	if err != nil {
		logger.Fatal("инициализация квотера Uniswap", zap.Error(err))
	}

	mdCh := make(chan marketdata.Snapshot, 1024)
	oppCh := make(chan types.Opportunity, 1024)
	go marketdata.Run(ctx, cfg, cex, quoter, mdCh, logger)
	go detector.Run(ctx, cfg, mdCh, oppCh, logger)

	if cfg.DryRun {
		logger.Warn("DRY-RUN: реальные ордера/свапы отправляться не будут")
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case opp := <-oppCh:
					logger.Info("opportunity",
						zap.Float64("qty_base", opp.QtyBase),
						zap.Float64("buy_px_cex", opp.BuyPxCEX),
						zap.Float64("dex_out_usd", opp.DexOutUSD),
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
		router, err := univ3.NewRouter(cfg, logger)
		if err != nil {
			logger.Fatal("инициализация роутера Uniswap", zap.Error(err))
		}
		riskEng := risk.NewEngine(cfg)
		exec, err := execution.NewExecutor(cfg, cex, router, riskEng, logger)
		if err != nil {
			logger.Fatal("инициализация исполнителя", zap.Error(err))
		}
		go exec.Run(ctx, oppCh)
	}

	logger.Info("бот запущен",
		zap.String("пара", cfg.Pair),
		zap.Bool("dry_run", cfg.DryRun),
	)

	for ctx.Err() == nil {
		time.Sleep(250 * time.Millisecond)
	}
}
