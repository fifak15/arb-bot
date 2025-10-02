package main

import (
	"context"
	"flag"
	"time"

	"github.com/you/arb-bot/internal/bot"
	"github.com/you/arb-bot/internal/config"
	"go.uber.org/zap"
)

func main() {
	cfgPath := flag.String("config", "./config.yaml", "path to config file")
	minPairs := flag.Int("min-pairs", 15, "minimum pairs to start monitoring")
	bootstrapLookback := flag.Duration("bootstrap-lookback", 30*time.Second, "how far back to look for active pairs")
	bootstrapPoll := flag.Duration("bootstrap-poll", 500*time.Millisecond, "frequency of polling Redis at startup")
	flag.Parse()

	log, err := bot.NewLogger()
	if err != nil {
		panic(err)
	}
	defer log.Sync()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatal("failed to load config", zap.Error(err))
	}
	cfg.ArbBot.MinPairs = *minPairs
	cfg.ArbBot.BootstrapLookback = *bootstrapLookback
	cfg.ArbBot.BootstrapPoll = *bootstrapPoll

	runDiscovery := flag.Bool("run-discovery", false, "run pair discovery and exit")
	flag.Parse()

	b := bot.New(cfg, log)
	b.Run(context.Background(), *runDiscovery)
}
