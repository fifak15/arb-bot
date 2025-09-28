package metrics

import "github.com/prometheus/client_golang/prometheus"

var (
	CEXMid = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "arb_cex_mid_usd",
		Help: "CEX mid price (USD) for current pair",
	})

	DexOutUSD = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "arb_dex_out_usd",
		Help: "DEX out (USD) for base qty",
	})

	GasUSD = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "arb_gas_usd",
		Help: "Estimated gas cost in USD",
	})

	QuoterErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "arb_quoter_errors_total",
		Help: "Number of quoter failures",
	})

	QuoteLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "arb_quoter_latency_seconds",
		Help:    "Time to obtain a DEX quote",
		Buckets: prometheus.DefBuckets, // можно настроить под себя
	})
)

func init() {
	prometheus.MustRegister(
		CEXMid,
		DexOutUSD,
		GasUSD,
		QuoterErrors,
		QuoteLatency,
	)
}
