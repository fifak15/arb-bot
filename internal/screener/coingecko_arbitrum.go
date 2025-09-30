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
		WrongRootPro:  resp.StatusCode == 400 && strings.Contains(lmsg, "pro-api.coingecko.com"),                                                                                             // 10010
		WrongRootDemo: resp.StatusCode == 400 && (strings.Contains(lmsg, "demo api key") || strings.Contains(lmsg, "change your root url  from pro-api.coingecko.com to api.coingecko.com")), // 10011
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

func ResolveArbitrumAddr(ctx context.Context, baseSymbol, apiKey string) (id string, arb string, err error) {
	cli := &http.Client{Timeout: 10 * time.Second}

	// ВАЖНО: всегда стартуем с demo-домена; переключаемся по ошибкам 10010/10011.
	isPro := false
	base := cgBaseDemo

	vprintf("[cg] >>> resolve start: symbol=%s mode=%s base=%s\n",
		baseSymbol, map[bool]string{true: "pro", false: "demo"}[isPro], base)

	// 1) /search — список кандидатов, с авто-переключением домена
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

	vprintf("[cg] search: найдено %d кандидат(ов) для %q\n", len(sr.Coins), baseSymbol)
	for i := 0; i < len(sr.Coins) && i < 10; i++ {
		c := sr.Coins[i]
		vprintf("    #%d id=%s symbol=%s name=%s\n", i+1, c.ID, c.Symbol, c.Name)
	}

	// Сортировка кандидатов
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
	vprintf("[cg] search: после сортировки первым идёт id=%s (symbol=%s, name=%s)\n", cand[0].ID, cand[0].Symbol, cand[0].Name)

	// 2) Перебираем до 5 кандидатов и ищем platforms["arbitrum-one"]
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

		// Полный список платформ
		if len(cr.Platforms) == 0 {
			vprintf("[cg] id=%s platforms=<empty>\n", c.ID)
		} else {
			plist := keys(cr.Platforms)
			vprintf("[cg] id=%s platforms=%v\n", c.ID, plist)
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

		vprintf("[cg] id=%s — arbitrum-one отсутствует, продолжаем перебор\n", c.ID)

		if k+1 >= maxProbe {
			vprintf("[cg] достигнут лимит попыток (%d)\n", maxProbe)
			break
		}
		time.Sleep(120 * time.Millisecond)
	}

	// Никого не нашли — вернём первый id, но без адреса (как раньше)
	vprintf("[cg] для символа %q адрес arbitrum-one не найден; возвращаем firstID=%s, addr=<none>\n", baseSymbol, firstID)
	return firstID, "", nil
}

func keys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func EnrichPairsArbitrum(ctx context.Context, pairs []PairInfo, cgProKey string) ([]PairInfo, error) {
	out := make([]PairInfo, 0, len(pairs))
	vprintf("[cg] === старт обогащения %d пар (arbitrum-one) ===\n", len(pairs))
	found := 0
	for i, p := range pairs {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		vprintf("[cg] [%2d/%2d] symbol=%-12s base=%-8s rank=%d → запрос id/адреса\n",
			i+1, len(pairs), p.Symbol, p.Base, p.Rank)

		id, addr, err := ResolveArbitrumAddr(ctx, p.Base, cgProKey)
		if err != nil {
			fmt.Printf("[cg] base=%s ошибка resolve: %v\n", p.Base, err)
		}
		p.CoinGeckoID = id
		p.ContractETH = addr // уже checksum (или пусто)

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

// вспомогательное: усечение длинных тел ошибок
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}

// вспомогательное: min
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
