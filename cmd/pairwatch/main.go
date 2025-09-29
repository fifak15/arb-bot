package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/you/arb-bot/internal/connectors/cex/mexc"
	"github.com/you/arb-bot/internal/screener"
)

func main() {
	var restURL string
	var wsURL string
	var fromRank, toRank int
	flag.StringVar(&restURL, "mexc-rest", "https://api.mexc.com", "MEXC REST url")
	flag.StringVar(&wsURL, "mexc-ws", "wss://wbs-api.mexc.com/ws", "MEXC WS url")
	flag.IntVar(&fromRank, "from", 50, "rank from (inclusive)")
	flag.IntVar(&toRank, "to", 65, "rank to (inclusive)")
	flag.Parse()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
		<-ch
		cancel()
	}()
	pairs, err := screener.TopUSDTByQuoteVol(ctx, restURL, fromRank, toRank)
	if err != nil {
		panic(err)
	}

	pairs, _ = screener.EnrichPairs(ctx, pairs)

	fmt.Println("Selected pairs:")
	for _, p := range pairs {
		fmt.Printf("%2d) %-12s  base=%-8s  eth=%s  (cg:%s)\n", p.Rank, p.Symbol, p.Base, p.ContractETH, p.CoinGeckoID)
	}

	syms := make([]string, 0, len(pairs))
	for _, p := range pairs {
		syms = append(syms, p.Symbol)
	}

	ws := mexc.NewWS(wsURL)
	stream, err := ws.SubscribeBookTicker(ctx, syms)
	if err != nil {
		panic(err)
	}

	fmt.Println("--- streaming bookTicker ---")
	for {
		select {
		case <-ctx.Done():
			return
		case t := <-stream:
			fmt.Printf("%s  %s  bid=%.8f  ask=%.8f\n", t.TS.Format(time.RFC3339), t.Symbol, t.Bid, t.Ask)
		}
	}
}
