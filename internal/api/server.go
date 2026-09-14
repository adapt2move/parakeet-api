package api

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/adapt2move/parakeet-api/internal/store"
)

// Run locks DATA_DIR, starts the queue and serves until ctx is done, then shuts down gracefully.
func Run(ctx context.Context, cfg Settings, log *slog.Logger) error {
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return fmt.Errorf("create DATA_DIR: %s", logCause(err))
	}
	// The lock must come before store.New, which wipes the audio of the process holding it.
	unlock, err := lockDataDir(cfg.DataDir)
	if err != nil {
		return err
	}
	defer unlock()
	st, err := store.New(cfg.Config)
	if err != nil {
		return errors.New(logCause(err))
	}
	listener, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return err
	}
	a := New(cfg, st, log)
	server := a.server()

	janitorCtx, stopJanitor := context.WithCancel(context.Background())
	var janitor sync.WaitGroup
	janitor.Go(func() { a.janitor(janitorCtx) })
	defer janitor.Wait()
	defer stopJanitor()

	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	log.Info("api_started", "listen_addr", listener.Addr().String())
	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}
	log.Info("api_stopping")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Warn("api_shutdown_timeout")
		server.Close()
	}
	if err := <-served; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	log.Info("api_stopped")
	return nil
}

func (a *API) server() *http.Server {
	return &http.Server{
		Handler:           a,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    64 << 10,
		// The built-in handler for "OPTIONS *" would read its body without any deadline.
		DisableGeneralOptionsHandler: true,
		ErrorLog:                     slog.NewLogLogger(a.log.Handler(), slog.LevelWarn),
	}
}

// janitor applies the retention and age limits right away and then every 30 seconds.
func (a *API) janitor(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		if err := a.store.Cleanup(); err != nil {
			a.log.Error("queue_cleanup_failed", "error", logCause(err))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Healthcheck asks the local server for readiness and returns the exit code: 0 when ready,
// 1 otherwise. Only the port of listenAddr is used; the check targets the loopback address.
func Healthcheck(listenAddr string) int {
	_, port, err := net.SplitHostPort(cmp.Or(strings.TrimSpace(listenAddr), ":8080"))
	if err != nil || port == "" {
		return 1
	}
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{}}
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
