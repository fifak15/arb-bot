package redisfeed

import (
	"context"

	"github.com/redis/go-redis/v9"
	"github.com/you/arb-bot/internal/config"
	"github.com/you/arb-bot/internal/types"
)

type Publisher struct {
	rdb    *redis.Client
	stream string
	active string
	snapNS string
}

func NewPublisher(cfg *config.Config) *Publisher {
	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.Redis.Addr,
		DB:       cfg.Redis.DB,
		Username: cfg.Redis.Username,
		Password: cfg.Redis.Password,
	})
	return &Publisher{
		rdb:    rdb,
		stream: cfg.Redis.Stream,
		active: cfg.Redis.ActiveKey,
		snapNS: cfg.Redis.SnapNS,
	}
}

func (p *Publisher) UpsertPairMeta(ctx context.Context, pm types.PairMeta, tsMs int64) error {
	metaKey := "pair:meta:" + pm.Symbol
	if err := p.rdb.HSet(ctx, metaKey, map[string]interface{}{
		"symbol": pm.Symbol,
		"base":   pm.Base,
		"addr":   pm.Addr,
		"cg_id":  pm.CgID,
		"ts_ms":  tsMs,
	}).Err(); err != nil {
		return err
	}
	// индекс «активных» пар (можно использовать тот же active ZSET)
	return p.rdb.ZAdd(ctx, "pair:active", redis.Z{
		Score: float64(tsMs), Member: pm.Symbol,
	}).Err()
}
