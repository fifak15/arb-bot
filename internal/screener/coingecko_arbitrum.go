package screener

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	cgBaseDemo = "https://api.coingecko.com/api/v3"
	cgBasePro  = "https://pro-api.coingecko.com/api/v3"
)

// Глобальный переключатель «болтливости» логов CoinGecko.
var CGVerbose = true

func vprintf(format string, args ...any) {
	if CGVerbose {
		fmt.Printf(format, args...)
	}
}

// ---------- Общие структуры / утилиты ----------

type cgHTTPError struct {
	Status        int
	URL           string
	Body          string
	WrongRootPro  bool // 10010: нужен pro-api.coingecko.com
	WrongRootDemo bool // 10011: нужен api.coingecko.com
	RateLimited   bool
}

func (e *cgHTTPError) Error() string {
	return fmt.Sprintf("http %d %s: %s", e.Status, e.URL, e.Body)
}

func newCGHTTPError(resp *http.Response, body []byte) *cgHTTPError {
	msg := string(body)
	lmsg := strings.ToLower(msg)
	return &cgHTTPError{
		Status:        resp.StatusCode,
		URL:           resp.Request.URL.String(),
		Body:          strings.TrimSpace(msg),
		WrongRootPro:  resp.StatusCode == 400 && strings.Contains(lmsg, "pro-api.coingecko.com"),
		WrongRootDemo: resp.StatusCode == 400 && (strings.Contains(lmsg, "demo api key") || strings.Contains(lmsg, "change your root url  from pro-api.coingecko.com to api.coingecko.com")),
		RateLimited:   resp.StatusCode == 429 || strings.Contains(lmsg, "throttled"),
	}
}

func makeReq(ctx context.Context, base, pathAndQuery, apiKey string, isPro bool) (*http.Request, error) {
	u := strings.TrimRight(base, "/") + pathAndQuery
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	if apiKey != "" {
		if isPro {
			req.Header.Set("x-cg-pro-api-key", apiKey)
		} else {
			req.Header.Set("x-cg-demo-api-key", apiKey)
		}
	}
	return req, nil
}

func httpDoJSON[T any](cli *http.Client, req *http.Request, v *T) error {
	// Отладочный вывод запроса + инфо о ключе (без значений)
	if k := req.Header.Get("x-cg-pro-api-key"); k != "" {
		vprintf("[cg/http] %s %s  headers: x-cg-pro-api-key=*** (len=%d)\n", req.Method, req.URL.String(), len(k))
	} else if k := req.Header.Get("x-cg-demo-api-key"); k != "" {
		vprintf("[cg/http] %s %s  headers: x-cg-demo-api-key=*** (len=%d)\n", req.Method, req.URL.String(), len(k))
	} else {
		vprintf("[cg/http] %s %s  headers: <none>\n", req.Method, req.URL.String())
	}

	resp, err := cli.Do(req)
	if err != nil {
		vprintf("[cg/http] transport error: %v\n", err)
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		he := newCGHTTPError(resp, b)
		vprintf("[cg/http] ! status=%d url=%s wrongRootPro=%v wrongRootDemo=%v rateLimited=%v body=%s\n",
			he.Status, he.URL, he.WrongRootPro, he.WrongRootDemo, he.RateLimited, truncate(he.Body, 240))
		return he
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		vprintf("[cg/http] ! decode error: %v\n", err)
		return err
	}
	return nil
}

// ---------- Точечная логика (fallback): /search + /coins/{id} ----------

type cgSearchResp2 struct {
	Coins []struct {
		ID     string `json:"id"`
		Symbol string `json:"symbol"`
		Name   string `json:"name"`
	} `json:"coins"`
}

type cgCoinResp2 struct {
	Platforms map[string]string `json:"platforms"`
}

// ResolveArbitrumAddr — старый точечный путь, оставляем как fallback.
func ResolveArbitrumAddr(ctx context.Context, baseSymbol, apiKey string) (id string, arb string, err error) {
	cli := &http.Client{Timeout: 10 * time.Second}

	isPro := false
	base := cgBaseDemo

	vprintf("[cg] >>> resolve start: symbol=%s mode=%s base=%s\n",
		baseSymbol, map[bool]string{true: "pro", false: "demo"}[isPro], base)

	var sr cgSearchResp2
searchTry:
	{
		req, reqErr := makeReq(ctx, base, "/search?query="+url.QueryEscape(baseSymbol), apiKey, isPro)
		if reqErr != nil {
			return "", "", reqErr
		}
		err = httpDoJSON(cli, req, &sr)
		if err != nil {
			var he *cgHTTPError
			if errors.As(err, &he) {
				if he.WrongRootPro {
					vprintf("[cg] search WrongRoot(10010) → переключаемся на %s и повторяем\n", cgBasePro)
					isPro = true
					base = cgBasePro
					sr = cgSearchResp2{}
					err = nil
					goto searchTry
				}
				if he.WrongRootDemo {
					vprintf("[cg] search WrongRoot(10011) → переключаемся на %s и повторяем\n", cgBaseDemo)
					isPro = false
					base = cgBaseDemo
					sr = cgSearchResp2{}
					err = nil
					goto searchTry
				}
			}
			return "", "", fmt.Errorf("coingecko search: %w", err)
		}
	}

	if len(sr.Coins) == 0 {
		vprintf("[cg] search: кандидатов нет для %q\n", baseSymbol)
		return "", "", fmt.Errorf("coingecko id not found for %s", baseSymbol)
	}

	baseLower := strings.ToLower(baseSymbol)
	type candT struct{ ID, Symbol, Name string }
	cand := make([]candT, 0, len(sr.Coins))
	for _, c := range sr.Coins {
		cand = append(cand, candT{ID: c.ID, Symbol: strings.ToLower(c.Symbol), Name: c.Name})
	}
	sort.SliceStable(cand, func(i, j int) bool {
		ai := cand[i].Symbol == baseLower
		aj := cand[j].Symbol == baseLower
		if ai != aj {
			return ai
		}
		return strings.Contains(strings.ToLower(cand[i].Name), baseLower)
	})

	const maxProbe = 5
	var firstID string
	for k, c := range cand {
		if k == 0 {
			firstID = c.ID
		}
		vprintf("[cg] probe #%d/%d: id=%s symbol=%s\n", k+1, min(len(cand), maxProbe), c.ID, c.Symbol)

		var cr cgCoinResp2
		attempts := 0
	coinTry:
		{
			req2, reqErr := makeReq(ctx, base,
				"/coins/"+url.PathEscape(c.ID)+"?localization=false&tickers=false&market_data=false&community_data=false&developer_data=false&sparkline=false",
				apiKey, isPro)
			if reqErr != nil {
				fmt.Printf("[cg] %s id=%s ошибка построения запроса: %v\n", baseSymbol, c.ID, reqErr)
				continue
			}

			err = httpDoJSON(cli, req2, &cr)
			if err != nil {
				var he *cgHTTPError
				if errors.As(err, &he) {
					// 10010 -> перейти на pro
					if he.WrongRootPro {
						vprintf("[cg] coins/%s WrongRoot(10010) → переключаемся на %s и повторяем\n", c.ID, cgBasePro)
						isPro = true
						base = cgBasePro
						attempts++
						if attempts <= 1 {
							goto coinTry
						}
					}
					// 10011 -> перейти на demo
					if he.WrongRootDemo {
						vprintf("[cg] coins/%s WrongRoot(10011) → переключаемся на %s и повторяем\n", c.ID, cgBaseDemo)
						isPro = false
						base = cgBaseDemo
						attempts++
						if attempts <= 1 {
							goto coinTry
						}
					}
					// 429/5xx — мягкий ре-трай
					if (he.RateLimited || he.Status >= 500) && attempts < 3 {
						backoff := time.Duration(200*(attempts+1)) * time.Millisecond
						vprintf("[cg] coins/%s retry #%d после %s (status=%d)\n", c.ID, attempts+1, backoff, he.Status)
						time.Sleep(backoff)
						attempts++
						goto coinTry
					}
				}
				fmt.Printf("[cg] %s id=%s ошибка: %v\n", baseSymbol, c.ID, err)
				continue
			}
		}

		if raw := strings.TrimSpace(cr.Platforms["arbitrum-one"]); raw != "" {
			cs, e := toChecksumAddress(raw)
			if e != nil {
				fmt.Printf("[cg] %s id=%s platform=arbitrum-one адрес_битый=%q err=%v\n", baseSymbol, c.ID, raw, e)
				continue
			}
			fmt.Printf("[cg] %s id=%s platform=arbitrum-one адрес=%s\n", baseSymbol, c.ID, cs)
			return c.ID, cs, nil
		}

		if k+1 >= maxProbe {
			break
		}
		time.Sleep(120 * time.Millisecond)
	}

	return firstID, "", nil
}

// ---------- Новый быстрый путь: /coins/list?include_platform=true ----------

type cgListCoin struct {
	ID        string            `json:"id"`
	Symbol    string            `json:"symbol"`
	Name      string            `json:"name"`
	Platforms map[string]string `json:"platforms"`
}

type ArbInfo struct {
	ID     string
	AddrCS string // checksum
}

// FetchArbitrumIndex тянет единым запросом список коинов с платформами,
// фильтрует по "arbitrum-one" и возвращает индекс по символу (lowercase).
func FetchArbitrumIndex(ctx context.Context, apiKey string) (map[string]ArbInfo, error) {
	cli := &http.Client{Timeout: 25 * time.Second}

	// начинаем с demo, авто-переключение по 10010/10011
	base := cgBaseDemo
	isPro := false

	path := "/coins/list?include_platform=true"

	var coins []cgListCoin
listTry:
	{
		req, err := makeReq(ctx, base, path, apiKey, isPro)
		if err != nil {
			return nil, err
		}
		err = httpDoJSON(cli, req, &coins)
		if err != nil {
			var he *cgHTTPError
			if errors.As(err, &he) {
				if he.WrongRootPro {
					vprintf("[cg] list WrongRoot(10010) → переключаемся на %s и повторяем\n", cgBasePro)
					base, isPro = cgBasePro, true
					coins = nil
					goto listTry
				}
				if he.WrongRootDemo {
					vprintf("[cg] list WrongRoot(10011) → переключаемся на %s и повторяем\n", cgBaseDemo)
					base, isPro = cgBaseDemo, false
					coins = nil
					goto listTry
				}
				// Пара мягких ретраев на 429/5xx
				if he.RateLimited || he.Status >= 500 {
					for a := 1; a <= 3; a++ {
						backoff := time.Duration(300*a) * time.Millisecond
						vprintf("[cg] list retry #%d после %s (status=%d)\n", a, backoff, he.Status)
						time.Sleep(backoff)
						req2, _ := makeReq(ctx, base, path, apiKey, isPro)
						if err = httpDoJSON(cli, req2, &coins); err == nil {
							break
						}
					}
					if err != nil {
						return nil, err
					}
				} else {
					return nil, err
				}
			} else {
				return nil, err
			}
		}
	}

	vprintf("[cg] list: получено монет=%d — строим индекс arbitrum-one…\n", len(coins))

	idx := make(map[string]ArbInfo, 4096)
	added := 0
	for _, c := range coins {
		if c.Platforms == nil {
			continue
		}
		raw := strings.TrimSpace(c.Platforms["arbitrum-one"])
		if raw == "" {
			continue
		}
		cs, err := toChecksumAddress(raw)
		if err != nil {
			if CGVerbose {
				fmt.Printf("[cg] list skip id=%s symbol=%s addr_invalid=%q err=%v\n", c.ID, c.Symbol, raw, err)
			}
			continue
		}
		key := strings.ToLower(c.Symbol)
		// если несколько id на один символ — оставляем первый «встретившийся»
		if _, exists := idx[key]; !exists {
			idx[key] = ArbInfo{ID: c.ID, AddrCS: cs}
			added++
		}
	}

	vprintf("[cg] list: собрано адресов arbitrum-one=%d (уникальные символы)\n", added)
	return idx, nil
}

// Применяем готовый индекс к слайсу пар (без сетевых запросов)
func EnrichPairsFromIndex(pairs []PairInfo, idx map[string]ArbInfo) []PairInfo {
	out := make([]PairInfo, 0, len(pairs))
	found := 0
	for _, p := range pairs {
		key := strings.ToLower(p.Base)
		if ai, ok := idx[key]; ok {
			p.CoinGeckoID = ai.ID
			p.ContractETH = ai.AddrCS
			found++
			if CGVerbose {
				fmt.Printf("[cg] idx ✓ %s → id=%s addr=%s\n", p.Base, ai.ID, ai.AddrCS)
			}
		} else if CGVerbose {
			fmt.Printf("[cg] idx ✗ %s → не найдено в индексе\n", p.Base)
		}
		out = append(out, p)
	}
	vprintf("[cg] idx применён: найдено адресов=%d из %d\n", found, len(pairs))
	return out
}

// Fallback-обогащение (точечные запросы). Сохранил для совместимости.
func EnrichPairsArbitrum(ctx context.Context, pairs []PairInfo, cgKey string) ([]PairInfo, error) {
	out := make([]PairInfo, 0, len(pairs))
	vprintf("[cg] === старт обогащения %d пар (arbitrum-one, точечные запросы) ===\n", len(pairs))
	found := 0
	for i, p := range pairs {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		vprintf("[cg] [%2d/%2d] symbol=%-12s base=%-8s rank=%d → запрос id/адреса\n",
			i+1, len(pairs), p.Symbol, p.Base, p.Rank)

		id, addr, err := ResolveArbitrumAddr(ctx, p.Base, cgKey)
		if err != nil {
			fmt.Printf("[cg] base=%s ошибка resolve: %v\n", p.Base, err)
		}
		p.CoinGeckoID = id
		p.ContractETH = addr // checksum (или пусто)

		if addr != "" {
			found++
			vprintf("[cg]      ✓ найден адрес (checksum)=%s для base=%s (id=%s)\n", addr, p.Base, id)
		} else {
			vprintf("[cg]      ✗ адрес не найден для base=%s (id=%s)\n", p.Base, id)
		}

		out = append(out, p)
		if i+1 < len(pairs) {
			time.Sleep(200 * time.Millisecond)
		}
	}
	vprintf("[cg] === обогащение завершено: найдено адресов=%d из %d ===\n", found, len(pairs))
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}
