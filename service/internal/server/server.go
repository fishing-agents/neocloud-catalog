// SPDX-License-Identifier: Apache-2.0

// Package server runs the API port (probes and /v1) and the metrics port.
package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/fishman/neocloud-catalog/service/internal/config"
)

const readyTimeout = 2 * time.Second

// Handler serves /healthz, /readyz (database ping) and api under /v1/.
func Handler(ping func(context.Context) error, api http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/v1/", api)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
		defer cancel()
		if err := ping(ctx); err != nil {
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

// MetricsHandler serves /metrics with the Go runtime and process collectors.
func MetricsHandler() http.Handler {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	return mux
}

// Serve runs the API and metrics servers until ctx ends or one fails, then
// shuts both down.
func Serve(ctx context.Context, cfg *config.Config, api http.Handler) error {
	servers := []*http.Server{
		{Addr: cfg.Server.Addr, Handler: api, ReadTimeout: cfg.Server.ReadTimeout, WriteTimeout: cfg.Server.WriteTimeout},
		{Addr: cfg.Telemetry.MetricsAddr, Handler: MetricsHandler(), ReadTimeout: cfg.Server.ReadTimeout, WriteTimeout: cfg.Server.WriteTimeout},
	}
	errCh := make(chan error, len(servers))
	for _, s := range servers {
		go func() { errCh <- s.ListenAndServe() }()
	}
	var err error
	select {
	case <-ctx.Done():
	case err = <-errCh:
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.WriteTimeout)
	defer cancel()
	for _, s := range servers {
		err = errors.Join(err, s.Shutdown(shutdownCtx))
	}
	return err
}
