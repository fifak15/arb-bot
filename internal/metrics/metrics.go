package metrics

import (
	"context"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
)

// Serve запускает HTTP-сервер метрик и health-check.
// Если reg == nil — используется глобальный Gatherer.
func Serve(ctx context.Context, addr string, reg *prometheus.Registry, log *zap.Logger) {
	if addr == "" {
		log.Info("metrics disabled: empty addr")
		return
	}

	mux := http.NewServeMux()

	// health endpoint — удобно для k8s/docker-compose
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// /metrics с опциями и отдельным реестром при необходимости
	var h http.Handler
	if reg != nil {
		h = promhttp.HandlerFor(reg, promhttp.HandlerOpts{
			EnableOpenMetrics: true,
			ErrorHandling:     promhttp.ContinueOnError, // не 500 при частичных ошибках
		})
	} else {
		h = promhttp.Handler()
	}
	// (опционально можно завернуть в InstrumentMetricHandler, если хотите свои http_* метрики)
	mux.Handle("/metrics", h)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadTimeout:       5 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		log.Info("metrics server starting", zap.String("addr", addr))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("metrics server error", zap.Error(err))
		}
	}()

	// graceful shutdown по ctx.Done()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Warn("metrics server shutdown error", zap.Error(err))
		} else {
			log.Info("metrics server stopped")
		}
	}()
}
