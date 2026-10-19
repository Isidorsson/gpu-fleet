package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"gpu-fleet/controlplane/internal/api"
)

const readHeaderTimeout = 5 * time.Second

// listeners returns the full API on CONTROLPLANE_ADDR first and, when
// PUBLIC_ADDR is set, a read-only copy for a public showcase deployment.
func listeners(full http.Handler) []*http.Server {
	addr := os.Getenv("CONTROLPLANE_ADDR")
	if addr == "" {
		addr = defaultAddr
	}
	servers := []*http.Server{{Addr: addr, Handler: full, ReadHeaderTimeout: readHeaderTimeout}}
	if public := os.Getenv("PUBLIC_ADDR"); public != "" {
		servers = append(servers, &http.Server{Addr: public, Handler: api.ReadOnly(full), ReadHeaderTimeout: readHeaderTimeout})
	}
	return servers
}

// serve runs every server until ctx ends or one of them fails, then shuts
// all of them down.
func serve(ctx context.Context, servers []*http.Server, log *slog.Logger) error {
	errc := make(chan error, len(servers))
	for i, srv := range servers {
		go func() {
			log.Info("controlplane listening", "addr", srv.Addr, "read_only", i > 0)
			errc <- srv.ListenAndServe()
		}()
	}

	var serveErr error
	select {
	case err := <-errc:
		serveErr = fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	for _, srv := range servers {
		if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr = errors.Join(serveErr, fmt.Errorf("shutdown %s: %w", srv.Addr, err))
		}
	}
	return serveErr
}
