// Package daemon is the composition root.
//
// This is the only package that knows which concrete implementation sits behind
// each interface. Repositories are constructed here and handed to exactly one
// service each; services are handed to the API and, later, to runners. Every
// other package depends on interfaces, which is what keeps the layering
// enforceable and the business logic testable without a database.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/tumika/tumika/source/internal/api"
	"github.com/tumika/tumika/source/internal/platform/paths"
	"github.com/tumika/tumika/source/internal/repository/sqlite"
	"github.com/tumika/tumika/source/internal/service"
)

// shutdownGrace is how long in-flight requests get to finish before the server
// is closed out from under them.
const shutdownGrace = 10 * time.Second

// Options configures a daemon.
type Options struct {
	Paths  paths.Paths
	Logger *slog.Logger
	// Listen overrides the configured address. Empty means use the
	// server.listen setting, which is where the value normally comes from.
	Listen string
}

// Daemon owns the process-wide resources: the database and the HTTP server.
type Daemon struct {
	opts   Options
	store  *sqlite.Store
	config service.ConfigService
	log    *slog.Logger
}

// New opens the database, migrates it, and wires the object graph.
//
// Migration happens here rather than lazily on first use so that a schema
// problem stops the daemon at startup, where it is visible, rather than
// surfacing as a failed request much later.
func New(ctx context.Context, opts Options) (*Daemon, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	if err := opts.Paths.MkdirAll(); err != nil {
		return nil, err
	}

	store, err := sqlite.Open(ctx, opts.Paths.DB)
	if err != nil {
		return nil, err
	}

	if err := sqlite.Migrate(ctx, store); err != nil {
		// Closing here matters: New returning an error means the caller has no
		// handle to close, and the writer connection holds a lock on the file.
		_ = store.Close()
		return nil, err
	}

	version, err := sqlite.SchemaVersion(ctx, store)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	opts.Logger.InfoContext(ctx, "database ready", "path", opts.Paths.DB, "schema_version", version)

	// Each repository is constructed once and handed to exactly one service.
	// This is the only place that rule can be broken, so it is the place to
	// check it in review.
	configRepo := sqlite.NewConfigRepo(store)

	return &Daemon{
		opts:   opts,
		store:  store,
		config: service.NewConfigService(configRepo, store),
		log:    opts.Logger,
	}, nil
}

// Close releases the database handles.
func (d *Daemon) Close() error { return d.store.Close() }

// ConfigService exposes the config service for the CLI's in-process uses.
func (d *Daemon) ConfigService() service.ConfigService { return d.config }

// Serve runs the HTTP API until ctx is cancelled, then drains.
func (d *Daemon) Serve(ctx context.Context) error {
	addr := d.opts.Listen
	if addr == "" {
		var err error
		if addr, err = service.String(ctx, d.config, service.KeyServerListen); err != nil {
			return fmt.Errorf("resolve listen address: %w", err)
		}
	}

	// Listen before announcing, so "listening" in the log means the port is
	// actually bound rather than that we were about to try.
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}

	return d.ServeListener(ctx, listener)
}

// ServeListener runs the API on a listener the caller owns, until ctx is
// cancelled, then drains.
//
// Separate from Serve so the caller can supply the listener: tests need an
// ephemeral port whose number they can discover, and socket activation — which
// systemd will hand us as an inherited file descriptor — needs the same seam.
func (d *Daemon) ServeListener(ctx context.Context, listener net.Listener) error {
	addr := listener.Addr().String()
	d.log.InfoContext(ctx, "listening", "addr", addr)
	warnIfExposed(ctx, d.log, addr)

	srv := &http.Server{
		Handler: api.NewRouter(api.Deps{Config: d.config, Logger: d.log}),
		// A client that opens a connection and sends nothing must not hold a
		// slot indefinitely.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	serveErr := make(chan error, 1)
	go func() {
		err := srv.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()

	select {
	case err := <-serveErr:
		return err

	case <-ctx.Done():
		d.log.Info("shutting down")

		// Deliberately context.Background: ctx is already cancelled, and passing
		// it would abort the drain immediately — turning a graceful shutdown
		// into an abrupt one at exactly the moment gracefulness is wanted.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()

		if err := srv.Shutdown(shutdownCtx); err != nil {
			d.log.Error("graceful shutdown timed out; closing connections", "err", err)
			return errors.Join(err, srv.Close())
		}
		return <-serveErr
	}
}

// warnIfExposed flags a bind beyond loopback.
//
// The API is plain HTTP by design for now (D8): TLS and device pairing are
// deferred, so the bearer token and any credential submitted through it cross
// the network in clear text. That is acceptable on loopback or a tailnet and
// nowhere else, and it is not the kind of thing an operator should discover
// later.
func warnIfExposed(ctx context.Context, log *slog.Logger, addr string) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return
	}

	ip := net.ParseIP(host)
	if ip != nil && ip.IsLoopback() {
		return
	}

	log.WarnContext(ctx, "listening beyond loopback on plain HTTP; the API token and any submitted credential cross the network in clear text — keep this on a trusted network until TLS lands",
		"addr", addr)
}
