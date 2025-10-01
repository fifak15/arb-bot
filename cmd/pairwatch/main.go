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

	"github.com/you/arb-bot/internal/connectors/cex/mexc"
	"github.com/you/arb-bot/internal/screener"
)

type t24 struct {
	Symbol      string `json:"symbol"`
	LastPrice   string `json:"lastPrice"`
	Volume      string `json:"volume"`
	QuoteVolume string `json:"quoteVolume"`
}

func main() {
	// ---- flags ----
	var wsURL string
	var fromRank int
	var toRank int
	var pick int
	var cgKey string
	var logEverySec int
	var cgVerbose bool

	flag.StringVar(&wsURL, "mexc-ws", "wss://wbs-api.mexc.com/ws", "MEXC WS url")
	flag.IntVar(&fromRank, "from", 100, "начальный ранг (включительно)")
	flag.IntVar(&toRank, "to", 600, "конечный ранг (включительно)")
	flag.IntVar(&pick, "pick", 15, "сколько случайных пар выбрать")
	flag.StringVar(&cgKey, "cg-key", "CG-6xYAcpo3zeBewP95hk2UksPK", "CoinGecko Pro/Demo API key")
	flag.IntVar(&logEverySec, "log-every", 20, "троттлинг логов для каждого символа, сек")
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

	fmt.Println("[mexc] GET /api/v3/ticker/24hr …")
	tickers, err := fetchTicker24h(ctx)
	if err != nil {
		panic(err)
	}
	if len(tickers) == 0 {
		fmt.Println("[mexc] пустой ответ по тикерам")
		return
	}

	type row struct {
		Sym string
		QV  float64
		LP  float64
		Vol float64
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
		if !isFinite(qv) && isFinite(lp) && isFinite(vol) && lp > 0 && vol > 0 {
			qv = lp * vol
		}
		if !isFinite(qv) || qv <= 0 {
			continue
		}
		rows = append(rows, row{Sym: sym, QV: qv, LP: lp, Vol: vol})
	}
	if len(rows) == 0 {
		fmt.Println("[mexc] нет USDT-пар с валидным quoteVolume")
		return
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

	// ---- 2) индекс CoinGecko arbitrum-one ----
	if cgKey == "" {
		fmt.Println("[cg] предупреждение: ключ не задан — запрос может быть ограничен по rate-limit")
	}
	fmt.Println("[cg] строим индекс arbitrum-one…")
	idx, err := screener.FetchArbitrumIndex(ctx, cgKey) // /coins/list?include_platform=true
	if err != nil {
		fmt.Printf("[cg] ошибка индекса: %v\n", err)
		return
	}

	// ---- 3) подготовить пары и 4) обогатить + фильтр arbitrum-one ----
	pairs := make([]screener.PairInfo, 0, len(window))
	for i, r := range window {
		base := strings.TrimSuffix(r.Sym, "USDT")
		pairs = append(pairs, screener.PairInfo{
			Symbol: r.Sym, Base: base, Quote: "USDT", Rank: fromRank + i,
		})
	}
	enriched := screener.EnrichPairsFromIndex(pairs, idx) // без сетевых вызовов
	withArb := enriched[:0]
	for _, p := range enriched {
		if strings.TrimSpace(p.ContractETH) != "" {
			withArb = append(withArb, p)
		}
	}
	fmt.Printf("[cg] после фильтра arbitrum-one: %d из %d\n", len(withArb), len(enriched))
	if len(withArb) == 0 {
		fmt.Println("[cg] адресов arbitrum-one не найдено в выбранном окне")
		return
	}

	if pick > len(withArb) {
		pick = len(withArb)
	}
	sample := cryptShuffle(withArb)
	sample = sample[:pick]

	fmt.Printf("[final] выбрано случайных %d пар с адресами Arbitrum One:\n", len(sample))
	for _, p := range sample {
		fmt.Printf("[final] %3d) %-12s base=%-10s addr=%s (cg:%s)\n",
			p.Rank, p.Symbol, p.Base, p.ContractETH, p.CoinGeckoID)
	}

	metaBySym := make(map[string]struct {
		Base string
		Addr string
		CGID string
	}, len(sample))
	for _, p := range sample {
		metaBySym[p.Symbol] = struct {
			Base string
			Addr string
			CGID string
		}{
			Base: p.Base,
			Addr: strings.TrimSpace(p.ContractETH),
			CGID: strings.TrimSpace(p.CoinGeckoID),
		}
	}

	syms := make([]string, 0, len(sample))
	for _, p := range sample {
		syms = append(syms, p.Symbol)
	}
	fmt.Printf("[book] подписка на %d символов: %v\n", len(syms), syms)

	bt := mexc.NewWS(wsURL)
	stream, err := bt.SubscribeBookTicker(ctx, syms)
	if err != nil {
		panic(err)
	}
	logEvery := time.Duration(logEverySec) * time.Second
	last := make(map[string]time.Time)

	fmt.Println("[book] поток запущен; троттлинг логов =", logEvery)
	for {
		select {
		case <-ctx.Done():
			return
		case t := <-stream:
			now := time.Now()
			if prev, ok := last[t.Symbol]; ok && now.Sub(prev) < logEvery {
				continue
			}
			last[t.Symbol] = now

			mid := (t.Bid + t.Ask) / 2
			spread := 0.0
			if mid > 0 {
				spread = (t.Ask - t.Bid) / mid * 100
			}
			ts := t.TS.Format(time.RFC3339)

			if m, ok := metaBySym[t.Symbol]; ok && m.Addr != "" {
				fmt.Printf("[book] %s  %-12s base=%-10s addr=%s (cg:%s)  bid=%.8f ask=%.8f spread=%.4f%%\n",
					ts, t.Symbol, m.Base, m.Addr, m.CGID, t.Bid, t.Ask, spread)
			} else {
				fmt.Printf("[book] %s  %-12s bid=%.8f ask=%.8f spread=%.4f%%\n",
					ts, t.Symbol, t.Bid, t.Ask, spread)
			}
		}
	}
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

func toF(s string) float64 {
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

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
