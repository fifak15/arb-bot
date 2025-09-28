package metrics

import (
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
	"net/http"
)

func Serve(addr string, log *zap.Logger) {
	if addr == "" {
		return
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	go func() { log.Info("metrics", zap.String("addr", addr)); _ = http.ListenAndServe(addr, mux) }()
}
