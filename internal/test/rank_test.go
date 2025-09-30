package test

import (
	"fmt"
	"testing"
	"time"

	screenerpkg "github.com/you/arb-bot/internal/screener"
)

func TestVolRanker_SelectTop50to65(t *testing.T) {
	r := screenerpkg.NewVolRanker()

	now := time.Now()
	for i := 1; i <= 80; i++ {
		sym := fmt.Sprintf("S%03dUSDT", i)
		r.Ingest(screenerpkg.MiniTick{Symbol: sym, Price: 1, Vol: float64(i), TS: now})
	}
	// шум
	r.Ingest(screenerpkg.MiniTick{Symbol: "WBTCBTC", Price: 60000, Vol: 10, TS: now})
	r.Ingest(screenerpkg.MiniTick{Symbol: "BADUSDT", Price: 0, Vol: 10, TS: now})

	all := r.TopAll()
	if len(all) != 80 {
		t.Fatalf("want 80 ranked USDT symbols, got %d", len(all))
	}

	fromRank, toRank := 50, 65
	start := fromRank - 1
	end := toRank
	selected := all[start:end]

	if got := len(selected); got != (toRank - fromRank + 1) {
		t.Fatalf("want %d selected, got %d", (toRank - fromRank + 1), got)
	}
	if selected[0].Symbol != "S031USDT" {
		t.Fatalf("rank %d: want S031USDT, got %s", fromRank, selected[0].Symbol)
	}
	if selected[len(selected)-1].Symbol != "S016USDT" {
		t.Fatalf("rank %d: want S016USDT, got %s", toRank, selected[len(selected)-1].Symbol)
	}
}
