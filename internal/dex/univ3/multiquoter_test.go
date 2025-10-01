package univ3

import (
	"context"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/you/arb-bot/internal/config"
	"github.com/you/arb-bot/internal/multicall"
	"go.uber.org/zap"
)

// MockMulticallClient is a mock implementation of the multicall client for testing.
type MockMulticallClient struct {
	Results []multicall.Result
	Error   error
}

func (m *MockMulticallClient) Aggregate(ctx context.Context, calls []multicall.Call) ([]multicall.Result, error) {
	if m.Error != nil {
		return nil, m.Error
	}
	return m.Results, nil
}

func TestMultiQuoter_QuoteAll(t *testing.T) {
	// Setup
	logger, _ := zap.NewDevelopment()
	cfg := &config.Config{
		DEX: config.DEXConfig{
			QuoterV2:  "0xb27308f9F90D607463bb33eA1BeBb41C27CE5AB6", // Arbitrum One QuoterV2
			Multicall: "0x842eC2c7D803033Edf55E478F461FC547Bc54EB2", // Arbitrum One Multicall2
		},
	}
	q2abi, err := abi.JSON(strings.NewReader(quoterV2ABI))
	require.NoError(t, err)

	wethAddr := common.HexToAddress("0x82af49447d8a07e3bd95bd0d56f35241523fbab1")
	usdtAddr := common.HexToAddress("0xfd086bc7cd5c481dcc9c85ebe478a1c0b69fcbb9")

	// Prepare mock response
	amountOut := big.NewInt(123456789) // e.g., 123.456789 USDT
	// Correctly pack the outputs for quoteExactInputSingle
	// The third argument, initializedTicksCrossed, must be a uint32.
	packedAmount, err := q2abi.Methods["quoteExactInputSingle"].Outputs.Pack(amountOut, big.NewInt(0), uint32(0), big.NewInt(0))
	require.NoError(t, err)

	mockMc := &MockMulticallClient{
		Results: []multicall.Result{
			{Success: true, Data: packedAmount}, // Fee tier 500
			{Success: false, Data: []byte{}},   // Fee tier 3000
		},
	}

	mq := &MultiQuoter{
		log:    logger,
		cfg:    cfg,
		mc:     mockMc,
		q2abi:  q2abi,
		quoter: common.HexToAddress(cfg.DEX.QuoterV2),
	}
	// Pre-populate the decimals cache to avoid a real RPC call in the test.
	mq.decimalsCache.Store(usdtAddr, 6)

	// Define test requests
	reqs := []MultiQuoteRequest{
		{
			PairSymbol: "WETH/USDT",
			TokenIn:    wethAddr,
			TokenOut:   usdtAddr,
			Amount:     big.NewInt(1e18), // 1 WETH
			FeeTiers:   []uint32{500, 3000},
			Type:       QuoteTypeExactInput,
		},
	}

	// Execute
	results, err := mq.QuoteAll(context.Background(), reqs)

	// Assertions
	require.NoError(t, err)
	require.NotNil(t, results)
	require.Contains(t, results, "WETH/USDT")

	quote := results["WETH/USDT"]
	assert.NoError(t, quote.Error)
	assert.Equal(t, uint32(500), quote.FeeTier)
	assert.Equal(t, 0, amountOut.Cmp(quote.Amount))
	assert.InDelta(t, 123.456789, quote.AmountUSD, 1e-9)
}