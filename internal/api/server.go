package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/adapt2move/parakeet-api/internal/store"
)

const (
	cleanupInterval = 30 * time.Second
	shutdownTimeout = 30 * time.Second
)

// Run locks DATA_DIR, starts the queue and serves until ctx is done, then shuts down gracefully.
func Run(ctx context.Context, cfg Settings, log *slog.Logger) error {
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return fmt.Errorf("create DATA_DIR: %w", err)
	}
	// Fail fast on a second API process. The lock must come before store.New, which wipes audio.
	unlock, err := lockDataDir(cfg.DataDir)
	if err != nil {
		return err
	}
	defer unlock()
	st, err := store.New(cfg.StoreConfig())
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return err
	}
	a := New(cfg, st, log)
	server := &http.Server{
		Handler:           a,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	janitorDone := make(chan struct{})
	janitorCtx, stopJanitor := context.WithCancel(context.Background())
	go func() {
		defer close(janitorDone)
		a.janitor(janitorCtx)
	}()
	defer func() {
		stopJanitor()
		<-janitorDone
	}()

	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	log.Info("api_started", "listen_addr", listener.Addr().String())

	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}
	log.Info("api_stopping")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Warn("api_shutdown_timeout")
		server.Close()
	}
	if err := <-served; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	log.Info("api_stopped")
	return nil
}

// janitor applies retention and age limits right away and then every 30 seconds.
func (a *API) janitor(ctx context.Context) {
	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()
	for {
		if err := a.store.Cleanup(); err != nil {
			a.log.Error("queue_cleanup_failed", "error", err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Healthcheck asks the local server for readiness and returns the process exit code: 0 when
// ready, 1 otherwise. Only the port of listenAddr is used; the check targets the loopback address.
func Healthcheck(listenAddr string) int {
	if listenAddr == "" {
		listenAddr = ":8080"
	}
	_, port, err := net.SplitHostPort(listenAddr)
	if err != nil || port == "" {
		return 1
	}
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: nil}}
	res, err := client.Get("http://" + net.JoinHostPort("127.0.0.1", port) + "/health/ready")
	if err != nil {
		return 1
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
