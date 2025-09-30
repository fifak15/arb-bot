package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"syscall"
	"time"

	"github.com/you/arb-bot/internal/connectors/cex/mexc"
	"github.com/you/arb-bot/internal/screener"
)

func main() {
	var wsURL string
	var cgKey string
	var warmupSec int
	var fromRank, toRank int

	flag.StringVar(&wsURL, "mexc-ws", "wss://wbs-api.mexc.com/ws", "MEXC WS url")
	flag.StringVar(&cgKey, "cg-key", "CG-6xYAcpo3zeBewP95hk2UksPK", "CoinGecko Pro API key")
	flag.IntVar(&warmupSec, "warmup", 90, "warmup seconds for miniTickers ranking")
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

	mini := mexc.NewMiniWS(wsURL)
	mtc, errs, err := mini.SubscribeMiniTickers(ctx)
	if err != nil {
		panic(err)
	}
	r := screener.NewVolRanker()

	warmup := time.NewTimer(time.Duration(warmupSec) * time.Second)
collect:
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-errs:
			fmt.Printf("miniTickers error: %v\n", e)
			return
		case t := <-mtc:
			ts := time.UnixMilli(t.SendTime)
			fmt.Printf("[mini] sym=%s ts=%s price=%s vol=%s high=%s low=%s rate=%s\n",
				t.Symbol, ts.Format(time.RFC3339), t.Price, t.Volume, t.High, t.Low, t.Rate)

			price, perr := strconv.ParseFloat(t.Price, 64)
			vol, verr := strconv.ParseFloat(t.Volume, 64)
			if perr != nil || verr != nil {
				fmt.Printf("[mini] parse error: sym=%s price=%q err=%v vol=%q err=%v\n",
					t.Symbol, t.Price, perr, t.Volume, verr)
				break
			}

			r.Ingest(screener.MiniTick{
				Symbol: t.Symbol,
				Price:  price,
				Vol:    vol,
				TS:     ts,
			})

		case <-warmup.C:
			break collect
		}
	}
	all := r.TopAll()
	if len(all) == 0 {
		fmt.Println("no miniTickers data collected")
		return
	}
	mini.Close()

	// 2) выбираем ранги 50..65 (15 пар)
	sort.Slice(all, func(i, j int) bool { return all[i].QV > all[j].QV }) // на всякий случай
	start := fromRank - 1
	if start < 0 {
		start = 0
	}
	end := toRank
	if end > len(all) {
		end = len(all)
	}
	if start >= end {
		fmt.Println("rank range out of bounds")
		return
	}
	selected := all[start:end]

	for i, v := range selected {
		fmt.Printf("%2d) %-12s qv=%s\n", fromRank+i, v.Symbol, human(v.QV))
	}

	pairs := make([]screener.PairInfo, 0, len(selected))
	for i, s := range selected {
		base := s.Symbol[:len(s.Symbol)-4] // ...USDT
		pairs = append(pairs, screener.PairInfo{
			Symbol: s.Symbol, Base: base, Quote: "USDT", Rank: fromRank + i,
		})
	}
	if cgKey != "" {
		if enriched, err := screener.EnrichPairsArbitrum(ctx, pairs, cgKey); err == nil {
			pairs = enriched
		}
	}

	fmt.Println("Selected pairs (ranks 50..65) with Arbitrum One addresses:")
	for _, p := range pairs {
		fmt.Printf("%2d) %-12s base=%-8s arb=%s (cg:%s)\n", p.Rank, p.Symbol, p.Base, p.ContractETH, p.CoinGeckoID)
	}

	syms := make([]string, 0, len(pairs))
	for _, p := range pairs {
		syms = append(syms, p.Symbol)
	}

	bt := mexc.NewWS(wsURL)
	stream, err := bt.SubscribeBookTicker(ctx, syms)
	if err != nil {
		panic(err)
	}
	fmt.Println("--- streaming bookTicker (top 15, ranks 50..65) ---")
	for {
		select {
		case <-ctx.Done():
			return
		case t := <-stream:
			fmt.Printf("%s  %s  bid=%f  ask=%f  spread=%f%%\n",
				t.TS.Format(time.RFC3339), t.Symbol, t.Bid, t.Ask,
				((t.Ask-t.Bid)/((t.Ask+t.Bid)/2))*100,
			)
		}
	}
}

func human(v float64) string {
	switch {
	case v >= 1e12:
		return strconv.FormatFloat(v/1e12, 'f', 2, 64) + "T"
	case v >= 1e9:
		return strconv.FormatFloat(v/1e9, 'f', 2, 64) + "B"
	case v >= 1e6:
		return strconv.FormatFloat(v/1e6, 'f', 2, 64) + "M"
	case v >= 1e3:
		return strconv.FormatFloat(v/1e3, 'f', 2, 64) + "K"
	default:
		return strconv.FormatFloat(v, 'f', 2, 64)
	}
}
