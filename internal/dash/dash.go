package dash

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/you/arb-bot/internal/marketdata"
)

// Row — теперь одна строка = (Pair, Venue)
type Row struct {
	Pair  string `json:"pair"`
	Base  string `json:"base"`
	Venue string `json:"venue"`

	CEXBid float64 `json:"cexBid"`
	CEXAsk float64 `json:"cexAsk"`

	DEXSellPx float64 `json:"dexSellPx"`
	DEXBuyPx  float64 `json:"dexBuyPx"`

	SpreadC2D float64 `json:"spreadC2D"`
	SpreadD2C float64 `json:"spreadD2C"`

	FeeSell uint32 `json:"feeSell"`
	FeeBuy  uint32 `json:"feeBuy"`

	TS int64 `json:"ts"`
}

type Store struct {
	mu   sync.RWMutex
	rows map[string]Row // key: pair|venue
}

func NewStore() *Store { return &Store{rows: make(map[string]Row, 64)} }

// Update — принимает снапшот и создаёт строки по каждому venue
func (s *Store) Update(pair, base string, snap marketdata.Snapshot, _ float64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(snap.Venues) == 0 {
		// фоллбек: оставим “обобщённую” строку без venue
		key := pair + "|best"
		var dexSellPx, dexBuyPx float64
		if snap.DexOutUSD > 0 && snap.DexSellVenue != "" {
			// в старом формате цена за 1 base не передавалась — на фронт отдадим как есть (0 => "—")
		}
		if snap.DexInUSD > 0 && snap.DexBuyVenue != "" {
		}
		s.rows[key] = Row{
			Pair:      pair,
			Base:      base,
			Venue:     "best",
			CEXBid:    snap.BestBidCEX,
			CEXAsk:    snap.BestAskCEX,
			DEXSellPx: dexSellPx,
			DEXBuyPx:  dexBuyPx,
			SpreadC2D: spreadC2D(dexSellPx, snap.BestAskCEX),
			SpreadD2C: spreadD2C(snap.BestBidCEX, dexBuyPx),
			FeeSell:   snap.DexSellFeeTier,
			FeeBuy:    snap.DexBuyFeeTier,
			TS:        time.Now().UnixMilli(),
		}
		return
	}

	for _, v := range snap.Venues {
		key := pair + "|" + string(v.Venue)
		row := Row{
			Pair:      pair,
			Base:      base,
			Venue:     string(v.Venue),
			CEXBid:    snap.BestBidCEX,
			CEXAsk:    snap.BestAskCEX,
			DEXSellPx: v.SellPxUSD,
			DEXBuyPx:  v.BuyPxUSD,
			SpreadC2D: spreadC2D(v.SellPxUSD, snap.BestAskCEX),
			SpreadD2C: spreadD2C(snap.BestBidCEX, v.BuyPxUSD),
			FeeSell:   v.FeeSell,
			FeeBuy:    v.FeeBuy,
			TS:        time.Now().UnixMilli(),
		}
		s.rows[key] = row
	}
}

func spreadC2D(dexSellPx, cexAsk float64) float64 {
	if dexSellPx > 0 && cexAsk > 0 {
		return dexSellPx/cexAsk - 1
	}
	return 0
}

func spreadD2C(cexBid, dexBuyPx float64) float64 {
	if cexBid > 0 && dexBuyPx > 0 {
		return cexBid/dexBuyPx - 1
	}
	return 0
}

func (s *Store) List() []Row {
	s.mu.RLock()
	out := make([]Row, 0, len(s.rows))
	for _, r := range s.rows {
		out = append(out, r)
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Pair == out[j].Pair {
			return out[i].Venue < out[j].Venue
		}
		return out[i].Pair < out[j].Pair
	})
	return out
}

func StartHTTP(ctx context.Context, s *Store, addr string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/dash", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s.List())
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, indexHTML)
	})

	srv := &http.Server{
		Addr:              addr,
		Handler:           withCORS(mux),
		ReadHeaderTimeout: 3 * time.Second,
	}

	go func() { <-ctx.Done(); _ = srv.Close() }()

	log.Printf("[dash] listening on %s", addr)
	if err := srv.ListenAndServe(); err != nil && !strings.Contains(err.Error(), "Server closed") {
		log.Printf("[dash] http server error: %v", err)
	}
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

const indexHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8"/>
  <meta name="viewport" content="width=device-width, initial-scale=1"/>
  <title>CEX ↔ DEX Monitor</title>
  <style>
    :root { --bg:#f8fafc; --card:#fff; --muted:#6b7280; --chip:#e5e7eb; }
    body{margin:0;background:var(--bg);font:14px/1.4 ui-sans-serif,system-ui,-apple-system,Segoe UI,Roboto,Ubuntu; color:#111827;}
    .wrap{max-width:1080px;margin:24px auto;padding:0 16px;}
    .hdr{display:flex;align-items:flex-end;justify-content:space-between;margin-bottom:12px;}
    .state{font-size:12px;padding:2px 8px;border-radius:999px;background:#d1fae5;color:#065f46;}
    table{width:100%;border-collapse:collapse;background:var(--card);border-radius:16px;overflow:hidden;box-shadow:0 10px 30px rgba(0,0,0,.06);}
    thead{background:#f3f4f6;} th,td{padding:12px 14px;text-align:left;} tbody tr{border-top:1px solid #f3f4f6;}
    .chip{display:inline-block;font-size:12px;padding:2px 8px;background:var(--chip);border-radius:999px;color:#374151;}
    .pct{padding:2px 8px;border-radius:8px;font-size:12px;}
    .pct.ok{background:#dcfce7;color:#166534;} .pct.bad{background:#fee2e2;color:#991b1b;} .pct.dim{background:#f3f4f6;color:#6b7280;}
    .sub{color:var(--muted);font-size:12px;margin:0;}
  </style>
</head>
<body>
<div class="wrap">
  <div class="hdr">
    <div>
      <h1 style="margin:0;font-size:22px;font-weight:600">CEX ↔ DEX Monitor</h1>
      <p class="sub">MEXC vs Multi-DEX (Arbitrum)</p>
    </div>
    <div id="state" class="state">live</div>
  </div>
  <table>
    <thead>
      <tr>
        <th>Pair</th><th>Base</th><th>Venue</th>
        <th>CEX (bid/ask)</th><th>DEX (sell/buy px)</th>
        <th>Spread CEX→DEX</th><th>Spread DEX→CEX</th><th>Fee (sell/buy)</th>
        <th style="text-align:right">Updated</th>
      </tr>
    </thead>
    <tbody id="rows"></tbody>
  </table>
  <p class="sub" style="margin-top:8px">Spreads: CEX→DEX = (DEX sell px / CEX ask) − 1, DEX→CEX = (CEX bid / DEX buy px) − 1. Строк по паре столько, сколько активных DEX-ов.</p>
</div>
<script>
  function usd(x){ return (x==null||isNaN(x)||x===0) ? '—' : ('$'+Number(x).toLocaleString(undefined,{maximumFractionDigits:6})); }
  function pct(x){ return (x==null||isNaN(x)||x===0) ? '—' : ((x*100).toFixed(3)+'%'); }
  function rowHTML(r){
    var bestC2D = Math.abs(r.spreadC2D||0) >= Math.abs(r.spreadD2C||0);
    var c2dPos = (r.spreadC2D||0) > 0;
    var d2cPos = (r.spreadD2C||0) > 0;
    return '<tr>'
      + '<td><strong>' + (r.pair||'') + '</strong></td>'
      + '<td><span class="chip">' + (r.base||'') + '</span></td>'
      + '<td><span class="chip">' + (r.venue||'') + '</span></td>'
      + '<td>' + usd(r.cexBid) + ' <span style="color:#9CA3AF">/</span> ' + usd(r.cexAsk) + '</td>'
      + '<td>' + usd(r.dexSellPx) + ' <span style="color:#9CA3AF">/</span> ' + usd(r.dexBuyPx) + '</td>'
      + '<td><span class="pct ' + (bestC2D ? (c2dPos?'ok':'bad'):'dim') + '">' + pct(r.spreadC2D) + '</span></td>'
      + '<td><span class="pct ' + (!bestC2D ? (d2cPos?'ok':'bad'):'dim') + '">' + pct(r.spreadD2C) + '</span></td>'
      + '<td><span class="chip">' + ((r.feeSell||0)/10000) + '%</span> <span class="chip">' + ((r.feeBuy||0)/10000) + '%</span></td>'
      + '<td style="text-align:right;color:#6B7280;font-size:12px">' + new Date(r.ts||Date.now()).toLocaleTimeString() + '</td>'
      + '</tr>';
  }
  async function tick(){
    try{
      var res = await fetch('/api/dash', {cache:'no-store'});
      if(!res.ok) throw new Error('status '+res.status);
      var data = await res.json();
      document.getElementById('state').textContent = 'live';
      document.getElementById('rows').innerHTML = data.map(rowHTML).join('');
    }catch(e){
      document.getElementById('state').textContent = 'demo';
    }
  }
  tick(); setInterval(tick, 1000);
</script>
</body>
</html>`
