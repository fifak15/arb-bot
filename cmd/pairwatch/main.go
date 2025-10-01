package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/you/arb-bot/internal/config"
	"github.com/you/arb-bot/internal/connectors/redisfeed"
	"github.com/you/arb-bot/internal/screener"
	"github.com/you/arb-bot/internal/types"
)

type t24 struct {
	Symbol      string `json:"symbol"`
	LastPrice   string `json:"lastPrice"`
	Volume      string `json:"volume"`
	QuoteVolume string `json:"quoteVolume"`
}

func main() {
	var fromRank, toRank, pick int
	var cgKey string
	var cgVerbose bool

	flag.IntVar(&fromRank, "from", 100, "начальный ранг (включительно)")
	flag.IntVar(&toRank, "to", 600, "конечный ранг (включительно)")
	flag.IntVar(&pick, "pick", 15, "сколько случайных пар выбрать")
	flag.StringVar(&cgKey, "cg-key", "CG-6xYAcpo3zeBewP95hk2UksPK", "CoinGecko Pro/Demo API key")
	flag.BoolVar(&cgVerbose, "cg-verbose", false, "подробные логи CoinGecko")
	flag.Parse()

	screener.CGVerbose = cgVerbose

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
		<-ch
		fmt.Println("[sys] сигнал завершения — выходим…")
		cancel()
	}()

	// загрузим конфиг и инициализируем publisher
	cfg, err := config.Load("./config.yaml")
	if err != nil {
		panic(err)
	}
	pub := redisfeed.NewPublisher(cfg)

	// 1) MEXC 24h
	fmt.Println("[mexc] GET /api/v3/ticker/24hr …")
	tickers, err := fetchTicker24h(ctx)
	if err != nil {
		panic(err)
	}
	if len(tickers) == 0 {
		fmt.Println("[mexc] пустой ответ по тикерам")
		return
	}

	// 2) /USDT + QV sort + окно [from..to]
	type row struct {
		Sym         string
		QV, LP, Vol float64
	}
	rows := make([]row, 0, len(tickers))
	for _, r := range tickers {
		sym := strings.ToUpper(strings.TrimSpace(r.Symbol))
		if !strings.HasSuffix(sym, "USDT") {
			continue
		}
		lp := toF(r.LastPrice)
		vol := toF(r.Volume)
		qv := toF(r.QuoteVolume)
		if !isFinite(qv) && lp > 0 && vol > 0 {
			qv = lp * vol
		}
		if qv <= 0 {
			continue
		}
		rows = append(rows, row{Sym: sym, QV: qv, LP: lp, Vol: vol})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].QV > rows[j].QV })
	if fromRank < 1 {
		fromRank = 1
	}
	start := fromRank - 1
	if start > len(rows) {
		start = len(rows)
	}
	end := toRank
	if end > len(rows) {
		end = len(rows)
	}
	window := rows[start:end]
	fmt.Printf("[rank] окно %d..%d: всего %d пар\n", fromRank, toRank, len(window))
	if len(window) == 0 {
		return
	}

	// 3) CoinGecko: индекс arbitrum-one
	if cgKey == "" {
		fmt.Println("[cg] предупреждение: ключ не задан — возможны 429")
	}
	fmt.Println("[cg] строим индекс arbitrum-one…")
	idx, err := screener.FetchArbitrumIndex(ctx, cgKey)
	if err != nil {
		fmt.Printf("[cg] ошибка индекса: %v\n", err)
		return
	}

	// 4) enrich + фильтр только с адресом arbitrum-one
	pairs := make([]screener.PairInfo, 0, len(window))
	for i, r := range window {
		base := strings.TrimSuffix(r.Sym, "USDT")
		pairs = append(pairs, screener.PairInfo{
			Symbol: r.Sym, Base: base, Quote: "USDT", Rank: fromRank + i,
		})
	}
	enriched := screener.EnrichPairsFromIndex(pairs, idx)
	withArb := enriched[:0]
	for _, p := range enriched {
		addr := strings.TrimSpace(p.ContractETH) // arbitrum-one addr из индекса
		if addr != "" {
			withArb = append(withArb, p)
		}
	}
	fmt.Printf("[cg] после фильтра arbitrum-one: %d из %d\n", len(withArb), len(enriched))
	if len(withArb) == 0 {
		return
	}

	if pick > len(withArb) {
		pick = len(withArb)
	}
	sample := cryptShuffle(withArb)[:pick]

	nowMs := time.Now().UnixMilli()
	fmt.Printf("[final] публикуем %d пар в Redis как метаданные (symbol/base/addr/cg_id)…\n", len(sample))
	for _, p := range sample {
		pm := types.PairMeta{
			Symbol: p.Symbol,
			Base:   p.Base,
			Addr:   strings.TrimSpace(p.ContractETH),
			CgID:   strings.TrimSpace(p.CoinGeckoID),
		}
		if err := pub.UpsertPairMeta(ctx, pm, nowMs); err != nil {
			fmt.Printf("[redis] ошибка UpsertPairMeta для %s: %v\n", p.Symbol, err)
			continue
		}
		fmt.Printf("[pair] %-12s base=%-8s addr=%s (cg:%s)\n", pm.Symbol, pm.Base, pm.Addr, pm.CgID)
	}

	fmt.Println("[done] пары записаны в Redis; второй сервис может стартовать мониторинг.")
}

func fetchTicker24h(ctx context.Context) ([]t24, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://api.mexc.com/api/v3/ticker/24hr", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var arr []t24
	if err := json.NewDecoder(resp.Body).Decode(&arr); err != nil {
		return nil, err
	}
	return arr, nil
}

func toF(s string) float64    { f, _ := strconv.ParseFloat(s, 64); return f }
func isFinite(f float64) bool { return !((f != f) || (f > 1e308) || (f < -1e308)) }

func cryptShuffle[T any](in []T) []T {
	out := make([]T, len(in))
	copy(out, in)
	for i := len(out) - 1; i > 0; i-- {
		j := crandInt(i + 1)
		out[i], out[j] = out[j], out[i]
	}
	return out
}
func crandInt(n int) int {
	if n <= 1 {
		return 0
	}
	bi, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0
	}
	return int(bi.Int64())
}
