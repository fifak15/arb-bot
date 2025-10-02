package main

import (
	"context"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/you/arb-bot/internal/config"
	"github.com/you/arb-bot/internal/dex/univ3"
	"github.com/you/arb-bot/internal/discovery"
	"go.uber.org/zap"
)

func main() {
	cfgPath := flag.String("config", "./config.yaml", "path to config")
	tiersStr := flag.String("tiers", "100,500,3000,10000", "fee tiers to test, comma-separated")
	limit := flag.Int("limit", 5, "limit pairs to check")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		panic(err)
	}

	tiers := parseTiers(*tiersStr)
	ctx := context.Background()

	// ВАЖНО: используем не-nil логгер
	svc := discovery.NewService(cfg, zap.NewNop())

	pairs, err := svc.Discover(ctx)
	if err != nil {
		panic(err)
	}
	if *limit > 0 && *limit < len(pairs) {
		pairs = pairs[:*limit]
	}

	usdt := common.HexToAddress(cfg.DEX.USDT)
	if usdt == (common.Address{}) {
		panic("DEX.USDT is empty in config.yaml")
	}

	fmt.Printf("RPC: %s\n", cfg.Chain.RPCHTTP)
	fmt.Printf("USDT: %s\n", usdt.Hex())
	fmt.Printf("Testing tiers: %v\n\n", tiers)

	for _, pm := range pairs {
		base := common.HexToAddress(pm.Addr)
		present, pools, err := univ3.CheckAvailableFeeTiers(ctx, cfg.Chain.RPCHTTP, base, usdt, tiers)
		if err != nil {
			fmt.Printf("%-10s error: %v\n", pm.Symbol, err)
			continue
		}
		if len(present) == 0 {
			fmt.Printf("%-10s no pools on given tiers\n", pm.Symbol)
			continue
		}
		fmt.Printf("%-10s tiers: %v", pm.Symbol, present)
		for _, f := range present {
			fmt.Printf("  [fee=%d] %s", f, pools[uint32(f)].Hex())
		}
		fmt.Println()
	}

	_ = time.Second
}

func parseTiers(s string) []uint32 {
	parts := strings.Split(s, ",")
	var out []uint32
	for _, p := range parts {
		p = strings.TrimSpace(p)
		var v uint32
		fmt.Sscanf(p, "%d", &v)
		if v > 0 {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		out = []uint32{100, 500, 3000, 10000}
	}
	return out
}
