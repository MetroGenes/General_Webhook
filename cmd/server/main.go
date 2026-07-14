package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/MetroGenes/General_Webhook/internal/config"
	"github.com/MetroGenes/General_Webhook/internal/handler"
	"github.com/MetroGenes/General_Webhook/internal/logger"
	"github.com/MetroGenes/General_Webhook/internal/queue"
	"github.com/MetroGenes/General_Webhook/internal/server"
	"github.com/MetroGenes/General_Webhook/internal/store"
)

const totalShutdownBudget = 35 * time.Second

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
}

func run() error {
	cfgPath := flag.String("config", "configs/webhooks.yaml", "config file path")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	log := logger.New(cfg.Log.Level)

	st, err := store.Open(cfg.Store.Path)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	reg := handler.NewRegistry(
		handler.NewHTTP(),
		handler.NewExec(),
	)

	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	workerCtx, cancelWorker := context.WithCancel(context.Background())
	defer cancelWorker()

	q := queue.New(cfg.Queue, reg, st, cfg)
	q.Start(workerCtx)

	go retentionLoop(workerCtx, st, cfg.Store.Retention.Duration, log)

	api := server.New(cfg, q, st)
	srv := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       90 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("server starting", "addr", cfg.Server.Addr, "sources", len(cfg.Sources))
		err := srv.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-sigCtx.Done():
		log.Info("shutting down")
	case err := <-errCh:
		if err != nil {
			log.Error("server error", "err", err)
			api.SetStopping()
			cancelWorker()
			_ = q.Stop(context.Background())
			return fmt.Errorf("server error: %w", err)
		}
	}

	api.SetStopping()
	deadline := time.Now().Add(totalShutdownBudget)
	shutCtx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()

	if err := srv.Shutdown(shutCtx); err != nil {
		log.Error("shutdown http", "err", err)
		_ = srv.Close()
	}

	log.Info("draining queue")
	drainCtx, cancelDrain := context.WithDeadline(context.Background(), deadline)
	defer cancelDrain()
	if err := q.Stop(drainCtx); err != nil {
		log.Error("drain queue", "err", err)
		cancelWorker()
		q.Wait()
	}
	cancelWorker()
	log.Info("bye")
	return nil
}

func retentionLoop(ctx context.Context, st *store.Store, retention time.Duration, log *slog.Logger) {
	if retention <= 0 {
		return
	}
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	purge := func() {
		cutoff := time.Now().Add(-retention)
		n, err := st.PurgeOlderThan(ctx, cutoff)
		if err != nil {
			log.Error("purge old events", "err", err)
			return
		}
		if n > 0 {
			log.Info("purged old events", "count", n)
		}
	}
	purge()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			purge()
		}
	}
}
