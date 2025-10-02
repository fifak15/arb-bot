package test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/you/arb-bot/internal/config"
	"github.com/you/arb-bot/internal/discovery"
	"github.com/you/arb-bot/internal/screener"
	"go.uber.org/zap"
)

func newTestDiscoveryConfig(mexcURL, cgURL string) *config.Config {
	cfg := &config.Config{
		Discovery: config.DiscoveryCfg{
			FromRank:     1,
			ToRank:       10,
			Pick:         5,
			CoinGeckoKey: "test-key",
		},
		Redis: config.RedisCfg{},
	}
	cfg.MEXC.RestURL = mexcURL
	return cfg
}

type cgListCoin struct {
	ID        string            `json:"id"`
	Symbol    string            `json:"symbol"`
	Name      string            `json:"name"`
	Platforms map[string]string `json:"platforms"`
}

func mockMexcAPI(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/ticker/24hr" {
			http.NotFound(w, r)
			return
		}
		tickers := []struct {
			Symbol      string `json:"symbol"`
			QuoteVolume string `json:"quoteVolume"`
		}{
			{Symbol: "ETHUSDT", QuoteVolume: "1000000"},
			{Symbol: "BTCUSDT", QuoteVolume: "2000000"},
			{Symbol: "SOLUSDT", QuoteVolume: "500000"},
		}
		json.NewEncoder(w).Encode(tickers)
	}))
}

// mockCoinGeckoAPI creates a mock CoinGecko API server.
func mockCoinGeckoAPI(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		coins := []cgListCoin{
			{ID: "ethereum", Symbol: "eth", Name: "Ethereum", Platforms: map[string]string{"arbitrum-one": "0x82af49447d8a07e3bd95bd0d56f35241523fbab1"}},
			{ID: "bitcoin", Symbol: "btc", Name: "Bitcoin", Platforms: map[string]string{}},
			{ID: "solana", Symbol: "sol", Name: "Solana", Platforms: map[string]string{"arbitrum-one": "0x0000000000000000000000000000000000000001"}},
		}
		json.NewEncoder(w).Encode(coins)
	}))
}

func TestDiscoveryService_Run_Success(t *testing.T) {
	// Setup mock servers
	mexcServer := mockMexcAPI(t)
	defer mexcServer.Close()

	cgServer := mockCoinGeckoAPI(t)
	defer cgServer.Close()

	cfg := newTestDiscoveryConfig(mexcServer.URL, cgServer.URL)

	// Override screener URL to use the mock
	originalScreenerURL := screener.CoinGeckoURLOverride
	screener.CoinGeckoURLOverride = cgServer.URL
	defer func() { screener.CoinGeckoURLOverride = originalScreenerURL }()

	// Create service and run
	log := zap.NewNop()
	service := discovery.NewService(cfg, log)

	pairs, err := service.Discover(context.Background())
	assert.NoError(t, err)
	assert.NotEmpty(t, pairs, "expected pairs to be discovered")
	assert.Len(t, pairs, 2, "expected 2 pairs to be discovered")
}

func TestDiscoveryService_Run_APIFailure(t *testing.T) {
	// Setup mock server that always fails
	mexcServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "API is down", http.StatusInternalServerError)
	}))
	defer mexcServer.Close()

	cfg := newTestDiscoveryConfig(mexcServer.URL, "")
	cfg.MEXC.RestURL = mexcServer.URL

	log := zap.NewNop()
	service := discovery.NewService(cfg, log)

	_, err := service.Discover(context.Background())
	assert.Error(t, err, "expected an error when the API fails")
}
