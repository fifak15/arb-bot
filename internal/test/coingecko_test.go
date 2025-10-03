package test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	screenerpkg "github.com/you/arb-bot/internal/screener"
)

// --- test helpers ---

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResp(status int, v any) *http.Response {
	b, _ := json.Marshal(v)
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(bytes.NewReader(b)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}
}

// ==== CoinGecko response shapes (aligned with docs) ====

// One coin hit in /search response
type cgSearchCoin struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	APISymbol     string `json:"api_symbol"` // present in docs; we don't use it
	Symbol        string `json:"symbol"`
	MarketCapRank int    `json:"market_cap_rank"` // present in docs; we don't use it
	Thumb         string `json:"thumb"`           // present in docs; we don't use it
	Large         string `json:"large"`           // present in docs; we don't use it
}

// /search response
type cgSearchResp struct {
	Coins      []cgSearchCoin `json:"coins"`
	Exchanges  any            `json:"exchanges,omitempty"`
	ICOs       any            `json:"icos,omitempty"`
	Categories any            `json:"categories,omitempty"`
	NFTs       any            `json:"nfts,omitempty"`
}

// /coins/{id} response (we only need platforms map)
type cgCoinResp struct {
	Platforms map[string]string `json:"platforms"`
}

// --- tests ---

// Мокаем HTTP-вызовы CoinGecko и проверяем:
// 1) GMX и COMP получают CoinGeckoID и адрес arbitrum-one,
// 2) синтетика SPYON — ID есть, адрес пустой.
func TestEnrichPairsArbitrum_MockedHTTP(t *testing.T) {
	orig := http.DefaultTransport
	defer func() { http.DefaultTransport = orig }()

	http.DefaultTransport = rtFunc(func(req *http.Request) (*http.Response, error) {
		// Мокаем только api.coingecko.com; остальное прокидываем как есть
		if !strings.Contains(req.URL.Host, "api.coingecko.com") {
			return orig.RoundTrip(req)
		}

		// /api/v3/search?query=...
		if strings.Contains(req.URL.Path, "/api/v3/search") {
			q := req.URL.Query().Get("query")
			switch strings.ToUpper(q) {
			case "GMX":
				return jsonResp(200, cgSearchResp{Coins: []cgSearchCoin{
					{ID: "gmx", Name: "GMX", APISymbol: "gmx", Symbol: "gmx", MarketCapRank: 100, Thumb: "", Large: ""},
				}}), nil
			case "COMP":
				return jsonResp(200, cgSearchResp{Coins: []cgSearchCoin{
					{ID: "compound-governance-token", Name: "Compound", APISymbol: "compound-governance-token", Symbol: "comp"},
				}}), nil
			case "SPYON":
				return jsonResp(200, cgSearchResp{Coins: []cgSearchCoin{
					{ID: "spyon", Name: "SPYON Index", APISymbol: "spyon", Symbol: "spyon"},
				}}), nil
			default:
				return jsonResp(200, cgSearchResp{Coins: nil}), nil
			}
		}

		// /api/v3/coins/{id}?...  — возвращаем только интересующую нас часть
		if strings.Contains(req.URL.Path, "/api/v3/coins/") {
			id := strings.TrimPrefix(req.URL.Path, "/api/v3/coins/")
			id, _ = url.PathUnescape(strings.SplitN(id, "?", 2)[0])

			switch id {
			case "gmx":
				return jsonResp(200, cgCoinResp{
					Platforms: map[string]string{"arbitrum-one": "0xFc5A1A6EB076a2C7aD06eD22C90d7E710E35ad0a"},
				}), nil
			case "compound-governance-token":
				return jsonResp(200, cgCoinResp{
					Platforms: map[string]string{"arbitrum-one": "0x1Fe16De955718CFAb7A44605458AB023838C2793"},
				}), nil
			case "spyon":
				return jsonResp(200, cgCoinResp{Platforms: map[string]string{}}), nil
			default:
				return jsonResp(404, map[string]string{"error": "not found"}), nil
			}
		}

		return jsonResp(404, map[string]string{"error": "unhandled"}), nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pairs := []screenerpkg.PairInfo{
		{Symbol: "GMXUSDT", Base: "GMX", Quote: "USDT", Rank: 63},
		{Symbol: "SPYONUSDT", Base: "SPYON", Quote: "USDT", Rank: 50},
		{Symbol: "COMPUSDT", Base: "COMP", Quote: "USDT", Rank: 61},
	}

	out, err := screenerpkg.EnrichPairsArbitrum(ctx, pairs, "CG_TEST_KEY")
	if err != nil {
		t.Fatalf("enrich error: %v", err)
	}
	if len(out) != len(pairs) {
		t.Fatalf("expected %d results, got %d", len(pairs), len(out))
	}

	var gmx, spyon, comp *screenerpkg.PairInfo
	for i := range out {
		switch out[i].Base {
		case "GMX":
			gmx = &out[i]
		case "SPYON":
			spyon = &out[i]
		case "COMP":
			comp = &out[i]
		}
	}

	if gmx == nil || gmx.CoinGeckoID != "gmx" || gmx.ContractETH == "" {
		t.Fatalf("GMX should have id=gmx and arbitrum addr, got id=%q addr=%q",
			func() string {
				if gmx != nil {
					return gmx.CoinGeckoID
				}
				return "<nil>"
			}(),
			func() string {
				if gmx != nil {
					return gmx.ContractETH
				}
				return "<nil>"
			}(),
		)
	}
	if comp == nil || comp.CoinGeckoID != "compound-governance-token" || comp.ContractETH == "" {
		t.Fatalf("COMP should have id=compound-governance-token and arbitrum addr, got id=%q addr=%q",
			func() string {
				if comp != nil {
					return comp.CoinGeckoID
				}
				return "<nil>"
			}(),
			func() string {
				if comp != nil {
					return comp.ContractETH
				}
				return "<nil>"
			}(),
		)
	}
	if spyon == nil || spyon.CoinGeckoID != "spyon" || spyon.ContractETH != "" {
		t.Fatalf("SPYON should have id=spyon and empty arbitrum addr, got id=%q addr=%q",
			func() string {
				if spyon != nil {
					return spyon.CoinGeckoID
				}
				return "<nil>"
			}(),
			func() string {
				if spyon != nil {
					return spyon.ContractETH
				}
				return "<nil>"
			}(),
		)
	}
	t.Logf("\nDiscovered contracts (network: arbitrum-one):")
	t.Logf("%-8s  %-30s  %s", "BASE", "CoinGeckoID", "ContractETH")
	for _, p := range out {
		addr := p.ContractETH
		if addr == "" {
			addr = "<none>"
		}
		t.Logf("%-8s  %-30s  %s", p.Base, p.CoinGeckoID, addr)
	}
}
