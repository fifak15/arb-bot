package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/you/arb-bot/internal/connectors/cex/mexc"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
		<-ch
		cancel()
	}()

	mini := mexc.NewMiniWS("wss://wbs-api.mexc.com/ws")
	stream, errs, err := mini.SubscribeMiniTickers(ctx)
	if err != nil {
		panic(err)
	}
	defer mini.Close()

	timeout := time.NewTimer(60 * time.Second)
	for {
		select {
		case e := <-errs:
			fmt.Println("miniTickers error:", e)
			return
		case t := <-stream:
			// Это уже «очищенный» MiniTicker (если protobuf распарсился).
			// Оставляем для наглядности: видно, что дошло до уровня структуры.
			fmt.Printf("[mini.struct] %s price=%s vol=%s high=%s low=%s rate=%s ts=%d\n",
				t.Symbol, t.Price, t.Volume, t.High, t.Low, t.Rate, t.SendTime)
		case <-timeout.C:
			fmt.Println("done.")
			return
		case <-ctx.Done():
			return
		}
	}
}
