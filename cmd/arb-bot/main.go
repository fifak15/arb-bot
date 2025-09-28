package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/you/arb-bot/internal/config"
	"github.com/you/arb-bot/internal/connectors/cex/mexc"
	"github.com/you/arb-bot/internal/detector"
	"github.com/you/arb-bot/internal/dex/univ3"
	"github.com/you/arb-bot/internal/execution"
	"github.com/you/arb-bot/internal/marketdata"
	"github.com/you/arb-bot/internal/metrics"
	"github.com/you/arb-bot/internal/risk"
	"github.com/you/arb-bot/internal/types"
)

func main() {
	cfgPath := flag.String("config", "./config.yaml", "path to config")
	flag.Parse()

	logger, _ := zap.NewProduction()
	logger = logger.WithOptions(zap.IncreaseLevel(zap.DebugLevel))
	defer logger.Sync()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		logger.Fatal("load config", zap.Error(err))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigs
		logger.Warn("signal received, shutting down…")
		cancel()
	}()
	go metrics.Serve(cfg.Metrics.ListenAddr, logger)
	logger.Info("dex fee tiers parsed", zap.Uint32s("tiers", cfg.DEX.FeeTiers))
	cex, err := mexc.NewClient(cfg, logger)
	if err != nil {
		logger.Fatal("mexc init", zap.Error(err))
	}
	var (
		quoter univ3.Quoter
		router univ3.Router
	)
	if cfg.DryRun {
		q, err := univ3.NewSlot0Quoter(cfg, logger)
		if err != nil {
			logger.Fatal("uniswap quoter init", zap.Error(err))
		}
		quoter = q
	} else {
		q, err := univ3.NewSlot0Quoter(cfg, logger)
		if err != nil {
			logger.Fatal("uniswap init", zap.Error(err))
		}
		quoter = q
	}

	mdCh := make(chan marketdata.Snapshot, 1024)
	oppCh := make(chan types.Opportunity, 1024)
	go marketdata.Run(ctx, cfg, cex, quoter, mdCh, logger)
	go detector.Run(ctx, cfg, mdCh, oppCh, logger)
	if cfg.DryRun {
		logger.Warn("running in DRY-RUN mode: no real orders/swaps will be sent")
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
						zap.Time("ts", opp.Ts),
					)
				}
			}
		}()
	} else {
		riskEng := risk.NewEngine(cfg)
		exec := execution.NewExecutor(cfg, cex, router, riskEng, logger)
		go exec.Run(ctx, oppCh)
	}

	logger.Info("arb-bot started",
		zap.String("pair", cfg.Pair),
		zap.Bool("dry_run", cfg.DryRun),
	)

	for ctx.Err() == nil {
		time.Sleep(250 * time.Millisecond)
	}
}
