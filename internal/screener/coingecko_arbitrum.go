package screener

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type cgSearchResp2 struct {
	Coins []struct {
		ID     string `json:"id"`
		Symbol string `json:"symbol"`
		Name   string `json:"name"`
	} `json:"coins"`
}

type cgCoinResp2 struct {
	Platforms map[string]string `json:"platforms"` // platform -> contract
}

func ResolveArbitrumAddr(ctx context.Context, baseSymbol, apiKey string) (id string, arb string, err error) {
	cli := &http.Client{Timeout: 10 * time.Second}

	// 1) /search
	u := "https://api.coingecko.com/api/v3/search?query=" + url.QueryEscape(baseSymbol)
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	if apiKey != "" {
		req.Header.Set("x-cg-pro-api-key", apiKey)
	}
	resp, err := cli.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	var sr cgSearchResp2
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

	// 2) /coins/{id}
	coinURL := fmt.Sprintf("https://api.coingecko.com/api/v3/coins/%s?localization=false&tickers=false&market_data=false&community_data=false&developer_data=false&sparkline=false", url.PathEscape(id))
	req2, _ := http.NewRequestWithContext(ctx, "GET", coinURL, nil)
	if apiKey != "" {
		req2.Header.Set("x-cg-pro-api-key", apiKey)
	}
	resp2, err := cli.Do(req2)
	if err != nil {
		return id, "", err
	}
	defer resp2.Body.Close()

	var cr cgCoinResp2
	if err := json.NewDecoder(resp2.Body).Decode(&cr); err != nil {
		return id, "", err
	}

	if addr, ok := cr.Platforms["arbitrum-one"]; ok && addr != "" {
		return id, strings.ToLower(addr), nil
	}
	return id, "", nil
}

// EnrichPairsArbitrum — как EnrichPairs, только кладём адрес из "arbitrum-one" и уважаем Pro-key
func EnrichPairsArbitrum(ctx context.Context, pairs []PairInfo, cgProKey string) ([]PairInfo, error) {
	out := make([]PairInfo, 0, len(pairs))
	for i, p := range pairs {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		id, addr, err := ResolveArbitrumAddr(ctx, p.Base, cgProKey)
		if err == nil {
			p.CoinGeckoID = id
			p.ContractETH = addr // временно используем поле ContractETH
		}
		out = append(out, p)
		if i+1 < len(pairs) {
			time.Sleep(200 * time.Millisecond)
		}
	}
	return out, nil
}
