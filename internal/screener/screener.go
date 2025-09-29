package screener

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

type PairInfo struct {
	Symbol      string // e.g. "ABCUSDT"
	Base        string // e.g. "ABC"
	Quote       string // e.g. "USDT"
	Rank        int
	ContractETH string // ERC-20 (если есть)
	CoinGeckoID string
}

type mexc24hr struct {
	Symbol      string `json:"symbol"`
	QuoteVolume string `json:"quoteVolume"` // stringified float
}

func TopUSDTByQuoteVol(ctx context.Context, restURL string, fromRank, toRank int) ([]PairInfo, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(restURL, "/")+"/api/v3/ticker/24hr", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var rows []mexc24hr
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		return nil, err
	}
	type row struct {
		sym string
		qv  float64
	}
	acc := make([]row, 0, len(rows))
	for _, r := range rows {
		if !strings.HasSuffix(r.Symbol, "USDT") {
			continue
		}
		var qv float64
		fmt.Sscan(r.QuoteVolume, &qv)
		acc = append(acc, row{sym: r.Symbol, qv: qv})
	}
	sort.Slice(acc, func(i, j int) bool { return acc[i].qv > acc[j].qv })

	start := fromRank - 1
	if start < 0 {
		start = 0
	}
	end := toRank
	if end > len(acc) {
		end = len(acc)
	}
	if start >= end {
		return nil, fmt.Errorf("rank range out of bounds")
	}

	out := make([]PairInfo, 0, end-start)
	for i := start; i < end; i++ {
		s := acc[i].sym
		base := strings.TrimSuffix(s, "USDT")
		out = append(out, PairInfo{
			Symbol: s, Base: base, Quote: "USDT", Rank: i + 1,
		})
	}
	return out, nil
}

// -------- CoinGecko --------

type cgSearchResp struct {
	Coins []struct {
		ID     string `json:"id"`
		Symbol string `json:"symbol"`
		Name   string `json:"name"`
	} `json:"coins"`
}

type cgCoinResp struct {
	Platforms map[string]string `json:"platforms"` // platform -> contract
}

// ResolveERC20Addr запрашивает id монеты, затем адрес контракта на ethereum (если есть).
func ResolveERC20Addr(ctx context.Context, baseSymbol string) (id string, erc20 string, err error) {
	// 1) /search?query=BASE
	u := "https://api.coingecko.com/api/v3/search?query=" + url.QueryEscape(baseSymbol)
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	var sr cgSearchResp
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		return "", "", err
	}

	baseLower := strings.ToLower(baseSymbol)
	for _, c := range sr.Coins {
		if strings.EqualFold(c.Symbol, baseLower) {
			id = c.ID
			break
		}
	}
	if id == "" && len(sr.Coins) > 0 {
		id = sr.Coins[0].ID
	}
	if id == "" {
		return "", "", fmt.Errorf("coingecko id not found for %s", baseSymbol)
	}

	coinURL := fmt.Sprintf("https://api.coingecko.com/api/v3/coins/%s?localization=false&tickers=false&market_data=false&community_data=false&developer_data=false&sparkline=false", url.PathEscape(id))
	req2, _ := http.NewRequestWithContext(ctx, "GET", coinURL, nil)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		return id, "", err
	}
	defer resp2.Body.Close()
	var cr cgCoinResp
	if err := json.NewDecoder(resp2.Body).Decode(&cr); err != nil {
		return id, "", err
	}

	if addr, ok := cr.Platforms["ethereum"]; ok {
		return id, strings.ToLower(addr), nil
	}
	return id, "", nil // просто нет адреса на ETH — ок
}

// EnrichPairs вызывает CoinGecko для каждой пары (с бэк-оффом).
func EnrichPairs(ctx context.Context, pairs []PairInfo) ([]PairInfo, error) {
	out := make([]PairInfo, 0, len(pairs))
	for i, p := range pairs {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		id, addr, err := ResolveERC20Addr(ctx, p.Base)
		if err != nil {
			out = append(out, p) // без адреса тоже возвращаем
			continue
		}
		p.CoinGeckoID, p.ContractETH = id, addr
		out = append(out, p)
		if i+1 < len(pairs) {
			time.Sleep(250 * time.Millisecond) // щадим rate-limit
		}
	}
	return out, nil
}
