// Command ipsupport-airouter is the gateway service entrypoint: it loads
// config, opens the stores, applies migrations, and serves HTTP.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rromenskyi/ipsupport-airouter/internal/config"
	"github.com/rromenskyi/ipsupport-airouter/internal/httpapi"
	"github.com/rromenskyi/ipsupport-airouter/internal/providers"
	"github.com/rromenskyi/ipsupport-airouter/internal/seed"
	"github.com/rromenskyi/ipsupport-airouter/internal/store"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg)
	if err != nil {
		return err
	}
	defer st.Close()

	if err := st.Migrate(ctx); err != nil {
		return err
	}

	// Local-mock convenience: seed demo data + a fixed dev API key.
	if cfg.Env == "dev" && cfg.AuthMode == "mock" {
		token, err := seed.Dev(ctx, st)
		if err != nil {
			return err
		}
		slog.Warn("dev mock seeded; using a fixed, non-secret API key", "token", token)
	}

	// Build the provider registry from the DB. All providers are mock-backed
	// for the local mock; real provider kinds are wired on the k8s deploy.
	names, err := st.ProviderNames(ctx)
	if err != nil {
		return err
	}
	reg := providers.NewRegistry()
	for _, n := range names {
		reg.Register(providers.NewMock(n))
	}
	if _, ok := reg.Get("mock"); !ok {
		reg.Register(providers.NewMock("mock"))
	}

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           httpapi.NewServer(cfg, st, reg),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		slog.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}
