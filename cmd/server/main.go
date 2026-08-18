package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	"github.com/MetroGenes/General_Webhook/internal/heartbeat"
	"github.com/MetroGenes/General_Webhook/internal/logger"
	"github.com/MetroGenes/General_Webhook/internal/queue"
	"github.com/MetroGenes/General_Webhook/internal/server"
	"github.com/MetroGenes/General_Webhook/internal/store"
)

// totalShutdownBudget stops accepting work and waits briefly for in-flight jobs.
// Long exec actions are not guaranteed to finish; canceled jobs are requeued as pending.
const totalShutdownBudget = 35 * time.Second

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
}

func run() error {
	cfgPath := flag.String("config", "configs/webhooks.yaml", "config file path")
	checkConfig := flag.Bool("check-config", false, "validate configuration and exit")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if *checkConfig {
		return nil
	}

	log := logger.New(cfg.Log.Level)

	st, err := store.Open(cfg.Store.Path)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	replacements := config.ActionSecretReplacements(cfg)
	if len(replacements) > 0 {
		n, err := st.RewriteActionSnapshots(context.Background(), func(raw string) (string, error) {
			return config.ScrubSnapshotSecrets(raw, replacements)
		})
		if err != nil {
			return fmt.Errorf("scrub stored action credentials: %w", err)
		}
		if n > 0 {
			log.Warn("scrubbed credentials from stored action snapshots", "events", n)
		}
	}

	ownerID, err := newOwnerID()
	if err != nil {
		return fmt.Errorf("owner id: %w", err)
	}

	// Single-instance Compose: reset any leftover processing leases from a prior crash.
	if n, err := st.RequeueAllProcessing(context.Background()); err != nil {
		return fmt.Errorf("requeue processing: %w", err)
	} else if n > 0 {
		log.Info("requeued leftover processing events", "count", n, "owner", ownerID)
	}

	reg := handler.NewRegistry(
		handler.NewHTTP(),
		handler.NewExec(),
	)

	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	workerCtx, cancelWorker := context.WithCancel(context.Background())
	defer cancelWorker()

	q := queue.New(cfg.Queue, reg, st, cfg, ownerID)
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

	// Deadman heartbeat: pushes liveness outward when at least one target is
	// configured. Stopped at the start of shutdown so a graceful restart is
	// absorbed by the monitor grace period instead of firing a false alert.
	var stopHeartbeat context.CancelFunc = func() {}
	if len(cfg.Heartbeat.Targets) > 0 {
		hbCtx, hbCancel := context.WithCancel(context.Background())
		stopHeartbeat = hbCancel
		hb := heartbeat.New(cfg.Heartbeat, st,
			func() bool { return !api.Stopping() && q.Ready() },
			q.DropSummary)
		api.SetHeartbeat(hb)
		hb.Start(hbCtx)
		log.Info("heartbeat enabled", "targets", len(cfg.Heartbeat.Targets),
			"interval", cfg.Heartbeat.Interval.Duration.String(), "host", cfg.Heartbeat.Host)
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("server starting", "addr", cfg.Server.Addr, "sources", len(cfg.Sources), "owner", ownerID)
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
			stopHeartbeat()
			cancelWorker()
			_ = q.Stop(context.Background())
			return fmt.Errorf("server error: %w", err)
		}
	}

	api.SetStopping()
	stopHeartbeat()
	deadline := time.Now().Add(totalShutdownBudget)
	shutCtx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()

	// Stop claiming durable work immediately while allowing any action already
	// in flight to finish within the same shutdown budget as the HTTP server.
	log.Info("draining queue")
	queueDone := make(chan error, 1)
	go func() {
		queueDone <- q.Stop(shutCtx)
	}()

	if err := srv.Shutdown(shutCtx); err != nil {
		log.Error("shutdown http", "err", err)
		_ = srv.Close()
	}

	if err := <-queueDone; err != nil {
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

func newOwnerID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
