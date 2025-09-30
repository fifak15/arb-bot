package screener

import (
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

type MiniTick struct {
	Symbol string
	Price  float64
	Vol    float64
	TS     time.Time
}

type PairInfo struct {
	Symbol      string
	Base        string
	Quote       string
	Rank        int
	ContractETH string
	CoinGeckoID string
	Platform    string
}

type VolRanker struct {
	mu sync.RWMutex
	qv map[string]float64 // SYMBOL -> 24h quote volume (USDT)
}

func NewVolRanker() *VolRanker {
	return &VolRanker{qv: make(map[string]float64, 4096)}
}

func (r *VolRanker) Ingest(t MiniTick) {
	if !strings.HasSuffix(strings.ToUpper(t.Symbol), "USDT") {
		return
	}
	if t.Price <= 0 || t.Vol <= 0 || math.IsNaN(t.Price) || math.IsNaN(t.Vol) {
		return
	}
	qv := t.Vol * t.Price
	r.mu.Lock()
	r.qv[strings.ToUpper(t.Symbol)] = qv
	r.mu.Unlock()
}

type VolEntry struct {
	Symbol string
	QV     float64
}

func (r *VolRanker) TopAll() []VolEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]VolEntry, 0, len(r.qv))
	for s, v := range r.qv {
		out = append(out, VolEntry{Symbol: s, QV: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].QV > out[j].QV })
	return out
}
