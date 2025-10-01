package marketdata

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/you/arb-bot/internal/config"
	"go.uber.org/zap"
)

// MockCex is a mock implementation of the CEX client for testing.
type MockCex struct {
	Bid   float64
	Ask   float64
	Error error
}

func (m *MockCex) BestBidAsk(symbol string) (float64, float64, error) {
	if m.Error != nil {
		return 0, 0, m.Error
	}
	return m.Bid, m.Ask, nil
}

func TestMarketDataRunner(t *testing.T) {
	// Setup
	logger, _ := zap.NewDevelopment()
	cfg := &config.Config{Pair: "WETH/USDT"}
	mockCex := &MockCex{Bid: 2999.0, Ask: 3001.0}
	quoteCh := make(chan DexQuotes, 1)
	snapshotCh := make(chan Snapshot, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Run the marketdata runner in a goroutine
	go Run(ctx, cfg, mockCex, quoteCh, snapshotCh, logger)

	// Send a sample DEX quote
	dexQuote := DexQuotes{
		PairSymbol:      "WETH/USDT",
		DexOutAmountUSD: 3005.0,
		DexOutFeeTier:   500,
		DexInAmountUSD:  2995.0,
		DexInFeeTier:    500,
		GasUSD:          10.0,
		Ts:              time.Now(),
	}
	quoteCh <- dexQuote

	// Receive and verify the snapshot
	select {
	case snap := <-snapshotCh:
		assert.Equal(t, 3001.0, snap.BestAskCEX)
		assert.Equal(t, 2999.0, snap.BestBidCEX)
		assert.Equal(t, 3005.0, snap.DexOutUSD)
		assert.Equal(t, 2995.0, snap.DexInUSD)
		assert.Equal(t, 10.0, snap.GasSellUSD)
		assert.Equal(t, 10.0, snap.GasBuyUSD)
		assert.Equal(t, uint32(500), snap.DexSellFeeTier)
		assert.Equal(t, uint32(500), snap.DexBuyFeeTier)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for snapshot")
	}
}

func TestMarketDataRunner_ContextCancel(t *testing.T) {
	// Setup
	logger, _ := zap.NewDevelopment()
	cfg := &config.Config{Pair: "WETH/USDT"}
	mockCex := &MockCex{Bid: 2999.0, Ask: 3001.0}
	quoteCh := make(chan DexQuotes, 1)
	snapshotCh := make(chan Snapshot, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	// Run the marketdata runner
	done := make(chan struct{})
	go func() {
		Run(ctx, cfg, mockCex, quoteCh, snapshotCh, logger)
		close(done)
	}()

	// Wait for the runner to exit
	select {
	case <-done:
		// Test passed
	case <-time.After(1 * time.Second):
		t.Fatal("marketdata runner did not stop on context cancellation")
	}
}