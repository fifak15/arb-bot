package redisfeed

import (
	"context"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/you/arb-bot/internal/config"
	"github.com/you/arb-bot/internal/types"
)

type Consumer struct {
	rdb        *redis.Client
	activeKey  string // по умолчанию: "pair:active"
	metaNS     string // по умолчанию: "pair:meta:"
	streamName string // по умолчанию: "pair:stream"
}

// NewConsumer инициализирует клиент и подставляет разумные дефолты для ключей пар.
func NewConsumer(cfg *config.Config) *Consumer {
	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.Redis.Addr,
		DB:       cfg.Redis.DB,
		Username: cfg.Redis.Username,
		Password: cfg.Redis.Password,
	})

	active := cfg.Redis.ActiveKey
	if active == "" || active == "book:active" {
		active = "pair:active"
	}
	metaNS := cfg.Redis.SnapNS
	if metaNS == "" || metaNS == "book:snap:" {
		metaNS = "pair:meta:"
	}
	stream := cfg.Redis.Stream
	if stream == "" || stream == "book:stream" {
		stream = "pair:stream"
	}

	return &Consumer{
		rdb:        rdb,
		activeKey:  active,
		metaNS:     metaNS,
		streamName: stream,
	}
}

// ReadPairMeta читает HASH pair:meta:<SYMBOL> и возвращает метаданные пары.
func (c *Consumer) ReadPairMeta(ctx context.Context, symbol string) (types.PairMeta, error) {
	m, err := c.rdb.HGetAll(ctx, c.metaNS+symbol).Result()
	if err != nil {
		return types.PairMeta{}, err
	}
	if len(m) == 0 {
		return types.PairMeta{}, redis.Nil
	}
	return types.PairMeta{
		Symbol: m["symbol"],
		Base:   m["base"],
		Addr:   m["addr"],
		CgID:   m["cg_id"],
	}, nil
}

// RecentPairSymbols возвращает список символов из ZSET pair:active новее sinceMs.
func (c *Consumer) RecentPairSymbols(ctx context.Context, sinceMs int64) ([]string, error) {
	return c.rdb.ZRangeByScore(ctx, c.activeKey, &redis.ZRangeBy{
		Min: strconv.FormatInt(sinceMs, 10),
		Max: "+inf",
	}).Result()
}

// StreamConsumePairs — чтение событий метаданных пар из Redis Streams (consumer group).
// Создать группу один раз:  XGROUP CREATE pair:stream feed $ MKSTREAM
func (c *Consumer) StreamConsumePairs(ctx context.Context, group, consumer string, out chan<- types.PairMeta) error {
	for {
		streams, err := c.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group:    group,
			Consumer: consumer,
			Streams:  []string{c.streamName, ">"},
			Count:    200,
			Block:    time.Second,
		}).Result()
		if err == redis.Nil {
			continue
		}
		if err != nil {
			// можно логировать и ретраить
			time.Sleep(200 * time.Millisecond)
			continue
		}
		for _, s := range streams {
			for _, m := range s.Messages {
				pm := types.PairMeta{}
				if v, ok := m.Values["symbol"].(string); ok {
					pm.Symbol = v
				}
				if v, ok := m.Values["base"].(string); ok {
					pm.Base = v
				}
				if v, ok := m.Values["addr"].(string); ok {
					pm.Addr = v
				}
				if v, ok := m.Values["cg_id"].(string); ok {
					pm.CgID = v
				}
				// отправляем только валидные записи
				if pm.Symbol != "" && pm.Addr != "" {
					out <- pm
				}
				_ = c.rdb.XAck(ctx, c.streamName, group, m.ID).Err()
			}
		}
	}
}
