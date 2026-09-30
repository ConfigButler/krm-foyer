// Command krm-foyer serves browser login, Kubernetes API access and live krm-stream
// resources on one origin. See docs/design.md for the service contract.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ConfigButler/krm-foyer/internal/server"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	listen := flag.String("listen", ":8080", "address to listen on")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(*listen, logger); err != nil {
		logger.Error("krm-foyer stopped", "err", err)
		os.Exit(1)
	}
}

func run(listen string, logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{
		Addr:              listen,
		Handler:           server.New(server.Config{Version: version}),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errs := make(chan error, 1)
	go func() {
		logger.Info("krm-foyer listening", "addr", listen, "version", version)
		errs <- srv.ListenAndServe()
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}

	// Kubernetes sends SIGTERM and waits terminationGracePeriodSeconds (30s by
	// default) before killing the pod, so finish in-flight requests well inside that.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	if err := <-errs; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	logger.Info("krm-foyer shut down cleanly")
	return nil
}
