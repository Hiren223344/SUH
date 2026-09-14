// Command router runs the OpenAI-compatible distribution router: a single
// process that accepts client traffic on a small set of public model
// names and fans it out across configured upstreams by token share.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"router/internal/config"
	"router/internal/proxy"
	"router/internal/router"
	"router/internal/stats"
	"router/internal/upstream"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to config file")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	mgr, err := config.NewManager(*configPath, logger)
	if err != nil {
		logger.Error("failed to load config", "path", *configPath, "error", err)
		os.Exit(1)
	}

	reg := router.NewRegistry(mgr, upstream.DefaultBreakerConfig())
	rec := stats.NewRecorder()
	h := &proxy.Handlers{Registry: reg, Stats: rec, Logger: logger}

	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Post("/v1/chat/completions", h.ChatCompletions)
	r.Get("/v1/models", h.Models)
	r.Get("/v1/models/{id}", h.ModelByID)
	r.Get("/healthz", h.Healthz)
	r.Get("/internal/stats", stats.Handler(reg, rec))

	cfg := mgr.Get()
	srv := &http.Server{
		Addr:    cfg.Server.Listen,
		Handler: r,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go mgr.Watch(ctx)

	go func() {
		logger.Info("router listening", "addr", cfg.Server.Listen)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
	}
}
