package discovery

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/you/arb-bot/internal/config"
	"github.com/you/arb-bot/internal/connectors/redisfeed"
	"github.com/you/arb-bot/internal/screener"
	"github.com/you/arb-bot/internal/types"
	"go.uber.org/zap"
)

// Service handles the discovery of new trading pairs.
type Service struct {
	cfg *config.Config
	log *zap.Logger
	pub *redisfeed.Publisher
}

// NewService creates a new discovery service.
func NewService(cfg *config.Config, log *zap.Logger) *Service {
	return &Service{
		cfg: cfg,
		log: log,
		pub: redisfeed.NewPublisher(cfg),
	}
}

// Run fetches, filters, and stores trading pairs in Redis.
func (s *Service) Run(ctx context.Context) error {
	s.log.Info("starting pair discovery")

	// 1) Fetch 24-hour ticker data from MEXC.
	s.log.Info("fetching 24-hour ticker data from MEXC")
	tickers, err := s.fetchTicker24h(ctx)
	if err != nil {
		return fmt.Errorf("failed to fetch tickers: %w", err)
	}
	if len(tickers) == 0 {
		return fmt.Errorf("empty response on tickers")
	}

	// 2) Filter for /USDT pairs, sort by quote volume, and select a ranked window.
	type row struct {
		Sym         string
		QV, LP, Vol float64
	}
	rows := make([]row, 0, len(tickers))
	for _, r := range tickers {
		sym := strings.ToUpper(strings.TrimSpace(r.Symbol))
		if !strings.HasSuffix(sym, "USDT") {
			continue
		}
		lp := toF(r.LastPrice)
		vol := toF(r.Volume)
		qv := toF(r.QuoteVolume)
		if !isFinite(qv) && lp > 0 && vol > 0 {
			qv = lp * vol
		}
		if qv <= 0 {
			continue
		}
		rows = append(rows, row{Sym: sym, QV: qv, LP: lp, Vol: vol})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].QV > rows[j].QV })

	fromRank := s.cfg.Discovery.FromRank
	toRank := s.cfg.Discovery.ToRank
	if fromRank < 1 {
		fromRank = 1
	}
	start := fromRank - 1
	if start > len(rows) {
		start = len(rows)
	}
	end := toRank
	if end > len(rows) {
		end = len(rows)
	}
	window := rows[start:end]
	s.log.Info("rank window", zap.Int("from", fromRank), zap.Int("to", toRank), zap.Int("total_pairs", len(window)))
	if len(window) == 0 {
		return nil
	}

	// 3) Build the arbitrum-one index from CoinGecko.
	cgKey := s.cfg.Discovery.CoinGeckoKey
	if cgKey == "" {
		s.log.Warn("coingecko api key not set - 429s are possible")
	}
	screener.CGVerbose = s.cfg.Discovery.CoinGeckoVerbose
	s.log.Info("building arbitrum-one index")
	idx, err := screener.FetchArbitrumIndex(ctx, cgKey)
	if err != nil {
		return fmt.Errorf("failed to fetch coingecko index: %w", err)
	}

	// 4) Enrich pairs with contract addresses and filter for those on Arbitrum.
	pairs := make([]screener.PairInfo, 0, len(window))
	for i, r := range window {
		base := strings.TrimSuffix(r.Sym, "USDT")
		pairs = append(pairs, screener.PairInfo{
			Symbol: r.Sym, Base: base, Quote: "USDT", Rank: fromRank + i,
		})
	}
	enriched := screener.EnrichPairsFromIndex(pairs, idx)
	withArb := enriched[:0]
	for _, p := range enriched {
		addr := strings.TrimSpace(p.ContractETH)
		if addr != "" {
			withArb = append(withArb, p)
		}
	}
	s.log.Info("filtered for arbitrum-one", zap.Int("with_arb_addr", len(withArb)), zap.Int("total_enriched", len(enriched)))
	if len(withArb) == 0 {
		return nil
	}

	// 5) Pick a random sample and publish to Redis.
	pick := s.cfg.Discovery.Pick
	if pick > len(withArb) {
		pick = len(withArb)
	}
	sample := cryptShuffle(withArb)[:pick]

	nowMs := time.Now().UnixMilli()
	s.log.Info("publishing pairs to redis", zap.Int("count", len(sample)))
	for _, p := range sample {
		pm := types.PairMeta{
			Symbol: p.Symbol,
			Base:   p.Base,
			Addr:   strings.TrimSpace(p.ContractETH),
			CgID:   strings.TrimSpace(p.CoinGeckoID),
		}
		if err := s.pub.UpsertPairMeta(ctx, pm, nowMs); err != nil {
			s.log.Warn("failed to upsert pair meta", zap.String("symbol", p.Symbol), zap.Error(err))
			continue
		}
		s.log.Info("published pair", zap.String("symbol", pm.Symbol), zap.String("base", pm.Base), zap.String("addr", pm.Addr), zap.String("cg_id", pm.CgID))
	}

	s.log.Info("pair discovery finished")
	return nil
}

type t24 struct {
	Symbol      string `json:"symbol"`
	LastPrice   string `json:"lastPrice"`
	Volume      string `json:"volume"`
	QuoteVolume string `json:"quoteVolume"`
}

func (s *Service) fetchTicker24h(ctx context.Context) ([]t24, error) {
	baseURL := s.cfg.MEXC.RestURL
	if baseURL == "" {
		baseURL = "https://api.mexc.com"
	}
	fullURL, err := url.JoinPath(baseURL, "/api/v3/ticker/24hr")
	if err != nil {
		return nil, fmt.Errorf("failed to create mexc api url: %w", err)
	}

	req, _ := http.NewRequestWithContext(ctx, "GET", fullURL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var arr []t24
	if err := json.NewDecoder(resp.Body).Decode(&arr); err != nil {
		return nil, err
	}
	return arr, nil
}

func toF(s string) float64    { f, _ := strconv.ParseFloat(s, 64); return f }
func isFinite(f float64) bool { return !((f != f) || (f > 1e308) || (f < -1e308)) }

func cryptShuffle[T any](in []T) []T {
	out := make([]T, len(in))
	copy(out, in)
	for i := len(out) - 1; i > 0; i-- {
		j := crandInt(i + 1)
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func crandInt(n int) int {
	if n <= 1 {
		return 0
	}
	bi, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0
	}
	return int(bi.Int64())
}