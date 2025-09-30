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
	var logEverySec int
	var cgVerbose bool

	flag.StringVar(&wsURL, "mexc-ws", "wss://wbs-api.mexc.com/ws", "MEXC WS url")
	flag.StringVar(&cgKey, "cg-key", "CG-6xYAcpo3zeBewP95hk2UksPK", "CoinGecko Pro/Demo API key")
	flag.IntVar(&warmupSec, "warmup", 120, "секунд сбор мини-тиков (warmup)")
	flag.IntVar(&fromRank, "from", 20, "начальный ранг (включительно)")
	flag.IntVar(&toRank, "to", 35, "конечный ранг (включительно)")
	flag.IntVar(&logEverySec, "log-every", 25, "интервал логов на символ после формирования топа, сек")
	flag.BoolVar(&cgVerbose, "cg-verbose", true, "подробные логи CoinGecko (внутри screener)")
	flag.Parse()

	// прокинем подробность логов CG в пакет screener
	screener.CGVerbose = cgVerbose

	logEvery := time.Duration(logEverySec) * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
		<-ch
		fmt.Println("[sys] получен сигнал завершения — останавливаемся…")
		cancel()
	}()

	// --- WS #1: miniTickers ---
	fmt.Printf("[mini] старт прогрева на %d сек…\n", warmupSec)
	warmupStarted := time.Now()

	mini := mexc.NewMiniWS(wsURL)
	mtc, errs, err := mini.SubscribeMiniTickers(ctx)
	if err != nil {
		panic(err)
	}
	r := screener.NewVolRanker()
	warmup := time.NewTimer(time.Duration(warmupSec) * time.Second)

	// Прогрев: печатаем каждый miniTick и кормим ранжировщик
collect:
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-errs:
			fmt.Printf("[mini] ошибка потока: %v\n", e)
			return
		case t := <-mtc:
			ts := time.UnixMilli(t.SendTime)
			fmt.Printf("[mini] пара=%s время=%s цена=%s объём=%s макс=%s мин=%s изм=%s\n",
				t.Symbol, ts.Format(time.RFC3339), t.Price, t.Volume, t.High, t.Low, t.Rate)

			price, perr := strconv.ParseFloat(t.Price, 64)
			vol, verr := strconv.ParseFloat(t.Volume, 64)
			if perr != nil || verr != nil {
				fmt.Printf("[mini] ошибка парсинга: пара=%s цена=%q err=%v объём=%q err=%v\n",
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
	warmupDur := time.Since(warmupStarted).Truncate(time.Millisecond)
	if len(all) == 0 {
		fmt.Printf("[mini] прогрев завершён за %s — данных нет\n", warmupDur)
		return
	}
	fmt.Printf("[mini] прогрев завершён за %s — собрано %d символов\n", warmupDur, len(all))
	// mini НЕ закрываем — ниже переведём его в «редкий» лог

	// --- Ранги 50..65 (информационный вывод) ---
	fmt.Println("[rank] формируем срез по квотному объёму…")
	sort.Slice(all, func(i, j int) bool { return all[i].QV > all[j].QV })
	start := fromRank - 1
	if start < 0 {
		start = 0
	}
	end := toRank
	if end > len(all) {
		end = len(all)
	}
	if start >= end {
		fmt.Println("[rank] диапазон рангов вне границ — остановка")
		return
	}
	selected := all[start:end]
	for i, v := range selected {
		fmt.Printf("[rank] %2d) %-12s квотный_объём=%s\n", fromRank+i, v.Symbol, human(v.QV))
	}

	// --- Готовим пары и обогащаем через CoinGecko ---
	fmt.Println("[cg] начинаем обогащение адресами (arbitrum-one)…")
	pairs := make([]screener.PairInfo, 0, len(selected))
	for i, s := range selected {
		base := s.Symbol[:len(s.Symbol)-4] // …USDT
		pairs = append(pairs, screener.PairInfo{
			Symbol: s.Symbol, Base: base, Quote: "USDT", Rank: fromRank + i,
		})
	}
	enrichStart := time.Now()
	if cgKey != "" {
		if enriched, err := screener.EnrichPairsArbitrum(ctx, pairs, cgKey); err == nil {
			pairs = enriched
		} else {
			fmt.Printf("[cg] ошибка обогащения (базовый срез): %v\n", err)
		}
	} else {
		fmt.Println("[cg] ключ не задан — пропускаем запросы к CoinGecko")
	}

	found := make([]screener.PairInfo, 0, 15)
	for _, p := range pairs {
		if p.ContractETH != "" {
			found = append(found, p)
		}
	}
	fmt.Printf("[cg] обогащение завершено за %s — найдено адресов: %d из %d\n",
		time.Since(enrichStart).Truncate(time.Millisecond), len(found), len(pairs))

	// --- Если не набрали 15 адресов, досканируем вниз по рейтингу ---
	target := 15
	if len(found) < target && cgKey != "" {
		fmt.Printf("[cg] нужно %d адресов, найдено %d — начинаем доскан вниз по рейтингу…\n", target, len(found))
		scanPos := end
		batch := 30
		scanned := 0
		backfillStart := time.Now()

		for len(found) < target && scanPos < len(all) {
			j := scanPos + batch
			if j > len(all) {
				j = len(all)
			}
			window := all[scanPos:j]

			cand := make([]screener.PairInfo, 0, len(window))
			for i, e := range window {
				base := e.Symbol[:len(e.Symbol)-4]
				cand = append(cand, screener.PairInfo{
					Symbol: e.Symbol, Base: base, Quote: "USDT", Rank: scanPos + 1 + i,
				})
			}

			fmt.Printf("[cg] доскан батч %d..%d (всего %d шт.)…\n", scanPos+1, j, len(window))
			enr, err := screener.EnrichPairsArbitrum(ctx, cand, cgKey)
			if err != nil {
				fmt.Printf("[cg] ошибка обогащения (доскан @%d..%d): %v\n", scanPos+1, j, err)
			}
			addedThisBatch := 0
			for _, p := range enr {
				if p.ContractETH != "" {
					found = append(found, p)
					addedThisBatch++
					if len(found) == target {
						break
					}
				}
			}
			fmt.Printf("[cg] доскан батч %d..%d: добавлено %d адрес(ов); суммарно %d/%d\n",
				scanPos+1, j, addedThisBatch, len(found), target)

			scanned += len(window)
			scanPos = j
		}
		fmt.Printf("[cg] доскан завершён за %s; просмотрено доп. символов: %d; итог адресов: %d\n",
			time.Since(backfillStart).Truncate(time.Millisecond), scanned, len(found))
	}

	if len(found) == 0 {
		fmt.Println("[cg] в рейтинге не найдено ни одного адреса arbitrum-one — завершение")
		return
	}
	if len(found) > target {
		found = found[:target]
	}

	fmt.Printf("[final] выбрано %d пар с адресами Arbitrum One (исходный диапазон %d..%d):\n", len(found), fromRank, toRank)
	for _, p := range found {
		fmt.Printf("[final] %3d) %-12s базовый=%-8s адрес=%s (cg:%s)\n",
			p.Rank, p.Symbol, p.Base, p.ContractETH, p.CoinGeckoID)
	}

	// --- После формирования топа: троттлим логи mini по каждому символу ---
	fmt.Printf("[mini] включён редкий лог по mini: не чаще 1 сообщения на символ каждые %ds\n", logEverySec)
	miniLast := make(map[string]time.Time)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case e := <-errs:
				fmt.Printf("[mini] ошибка потока: %v\n", e)
				return
			case t := <-mtc:
				now := time.Now()
				if last, ok := miniLast[t.Symbol]; ok && now.Sub(last) < logEvery {
					continue
				}
				miniLast[t.Symbol] = now

				ts := time.UnixMilli(t.SendTime)
				fmt.Printf("[mini] пара=%s время=%s цена=%s объём=%s макс=%s мин=%s изм=%s\n",
					t.Symbol, ts.Format(time.RFC3339), t.Price, t.Volume, t.High, t.Low, t.Rate)
			}
		}
	}()

	// --- WS #2: подписка по выбранным символам (только с адресами) ---
	syms := make([]string, 0, len(found))
	for _, p := range found {
		syms = append(syms, p.Symbol)
	}
	fmt.Printf("[book] подписка на %d символов: %v\n", len(syms), syms)

	bt := mexc.NewWS(wsURL)
	stream, err := bt.SubscribeBookTicker(ctx, syms)
	if err != nil {
		panic(err)
	}
	fmt.Println("[book] поток bookTicker запущен; применяется троттлинг по", logEvery)

	// троттлинг логов bookTicker по каждому символу
	bookLast := make(map[string]time.Time)

	for {
		select {
		case <-ctx.Done():
			return
		case t := <-stream:
			now := time.Now()
			if last, ok := bookLast[t.Symbol]; ok && now.Sub(last) < logEvery {
				continue
			}
			bookLast[t.Symbol] = now

			fmt.Printf("[book] %s  %s  bid=%f  ask=%f  спрэд=%f%%\n",
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
