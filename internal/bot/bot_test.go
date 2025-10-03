package bot

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/you/arb-bot/internal/config"
	"go.uber.org/zap"
)

func TestNewBot(t *testing.T) {
	cfg := &config.Config{}
	logger := zap.NewNop()
	bot := New(cfg, logger)

	assert.NotNil(t, bot)
	assert.Equal(t, cfg, bot.cfg)
	assert.Equal(t, logger, bot.log)
	assert.NotNil(t, bot.discovery)
}

func TestBookCache_SetAndGet(t *testing.T) {
	cache := NewBookCache()
	symbol := "ETHUSDT"
	bid, ask := 2500.5, 2500.6

	cache.Set(symbol, bid, ask)

	gotBid, gotAsk, err := cache.BestBidAsk(symbol)
	assert.NoError(t, err)
	assert.Equal(t, bid, gotBid)
	assert.Equal(t, ask, gotAsk)
}

func TestBookCache_GetEmpty(t *testing.T) {
	cache := NewBookCache()
	symbol := "ETHUSDT"

	_, _, err := cache.BestBidAsk(symbol)
	assert.Error(t, err)
}

func TestBookCache_Has(t *testing.T) {
	cache := NewBookCache()
	symbol := "ETHUSDT"

	assert.False(t, cache.Has(symbol))

	cache.Set(symbol, 2500.5, 2500.6)
	assert.True(t, cache.Has(symbol))
}

func TestBookCache_ConcurrentAccess(t *testing.T) {
	cache := NewBookCache()
	symbol := "ETHUSDT"
	var wg sync.WaitGroup

	// Concurrent writes
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cache.Set(symbol, 2500.0+float64(i), 2501.0+float64(i))
		}(i)
	}

	// Concurrent reads
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _ = cache.BestBidAsk(symbol)
		}()
	}

	wg.Wait()
}

func TestWaitWSBootstrap(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	book := NewBookCache()
	symbols := []string{"BTCUSDT", "ETHUSDT"}
	logger := zap.NewNop()

	// Test case 1: All symbols are available before timeout
	go func() {
		time.Sleep(10 * time.Millisecond)
		book.Set("BTCUSDT", 50000, 50001)
		time.Sleep(10 * time.Millisecond)
		book.Set("ETHUSDT", 2500, 2501)
	}()

	missing := waitWSBootstrap(ctx, book, symbols, 1*time.Second, logger)
	assert.Empty(t, missing, "Expected no missing symbols")

	// Test case 2: Timeout occurs with missing symbols
	book = NewBookCache()
	missing = waitWSBootstrap(ctx, book, symbols, 50*time.Millisecond, logger)
	assert.ElementsMatch(t, []string{"BTCUSDT", "ETHUSDT"}, missing, "Expected missing symbols")

	// Test case 3: Context is canceled
	ctx, cancel2 := context.WithCancel(context.Background())
	cancel2() // Immediately cancel the context
	book = NewBookCache()
	missing = waitWSBootstrap(ctx, book, symbols, 1*time.Second, logger)
	// The function is expected to return nil if the context is canceled.
	assert.Nil(t, missing, "Expected nil when context is canceled")
}

func TestWsCEX_BestBidAsk(t *testing.T) {
	book := NewBookCache()
	wscex := &wsCEX{book: book}
	symbol := "ETHUSDT"
	bid, ask := 2500.5, 2500.6

	// Test when book is empty
	_, _, err := wscex.BestBidAsk(symbol)
	assert.Error(t, err, "Expected an error for an empty book")

	// Set values and test again
	book.Set(symbol, bid, ask)
	gotBid, gotAsk, err := wscex.BestBidAsk(symbol)
	assert.NoError(t, err)
	assert.Equal(t, bid, gotBid)
	assert.Equal(t, ask, gotAsk)
}

func TestNewLogger(t *testing.T) {
	logger, err := NewLogger()
	assert.NoError(t, err)
	assert.NotNil(t, logger)

	// Test if the logger works
	assert.NotPanics(t, func() {
		logger.Info("test message")
	})
}