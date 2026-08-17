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
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/tumika/tumika/source/internal/api"
	"github.com/tumika/tumika/source/internal/platform/buildinfo"
	"github.com/tumika/tumika/source/internal/platform/paths"
	"github.com/tumika/tumika/source/internal/platform/provider"
	"github.com/tumika/tumika/source/internal/platform/provider/anthropicapi"
	"github.com/tumika/tumika/source/internal/platform/provider/claudecode"
	"github.com/tumika/tumika/source/internal/platform/release"
	"github.com/tumika/tumika/source/internal/platform/secrets"
	"github.com/tumika/tumika/source/internal/repository/sqlite"
	"github.com/tumika/tumika/source/internal/runner"
	"github.com/tumika/tumika/source/internal/service"
)

// ErrRestartRequired means the daemon has finished and the supervisor should
// start it again — after an update was installed, or after one was rolled back.
//
// A distinct error rather than an os.Exit deep in the daemon: exiting from a
// library makes the whole thing untestable, and the caller is the only place
// that knows an exit code is even meaningful.
var ErrRestartRequired = errors.New("restart required")

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
	// Providers overrides the driver set. Empty means the real ones.
	//
	// A seam for tests, which need a driver pointed at a fake endpoint rather
	// than at Anthropic — and the same seam a future deployment pointing at a
	// gateway would use.
	Providers []provider.Provider

	// Updates overrides the self-update service.
	//
	// The same kind of seam as Providers. Without it the update paths are
	// unreachable in a test: a test binary is a development build, so New leaves
	// updates nil and ConfirmBoot, Confirm and the restart-on-update path are
	// never exercised until a real release runs them on someone's machine.
	Updates service.UpdateService
}

// Daemon owns the process-wide resources: the database and the HTTP server.
type Daemon struct {
	opts         Options
	store        *sqlite.Store
	config       service.ConfigService
	auth         service.AuthService
	providers    service.ProviderService
	sealer       secrets.Sealer
	updates      service.UpdateService
	updateRunner *runner.Update
	// appliedByAPI is closed when POST /v1/update/apply installs an update, so
	// the serve loop shuts down the same way the scheduled path does.
	appliedByAPI chan struct{}
	applyOnce    sync.Once
	health       service.HealthService
	log          *slog.Logger
	started      time.Time
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

	// The previous boot is resolved FIRST — before migrations, before key
	// custody, before the provider registry.
	//
	// Everything below this point is something a new binary can fail at: a bad
	// migration, a changed key backend, a driver whose descriptor no longer
	// validates. Resolving the update afterwards meant those failures never
	// incremented the boot counter, so the rollback never fired and
	// Restart=always looped forever — the exact "daemon that cannot start"
	// outcome the design exists to prevent. The pre-flight cannot catch them
	// either: `tumika version` never opens the database.
	updates, err := newUpdateService(ctx, opts, store)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	if updates != nil {
		rolledBack, bootErr := updates.ConfirmBoot(ctx)
		if bootErr != nil {
			// Not fatal on its own: a daemon that cannot read its update row can
			// still do its job, and refusing to start would turn a bookkeeping
			// problem into an outage. On a FRESH database the table does not
			// exist yet, which is this same path and is entirely normal.
			opts.Logger.WarnContext(ctx, "resolving the previous update failed", "err", bootErr)
		}
		if rolledBack {
			_ = store.Close()
			opts.Logger.WarnContext(ctx, "rolled back to the previous binary; exiting so the supervisor restarts onto it")
			return nil, ErrRestartRequired
		}
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

	config := service.NewConfigService(configRepo, store)
	// AuthService reaches settings through ConfigService rather than taking the
	// repository: it is owned there, and a second writer would bypass the rules
	// that live with it.
	auth := service.NewAuthService(config)

	// Key custody is resolved at startup, not lazily: a daemon that cannot seal
	// is a daemon that cannot store a credential, and finding that out on the
	// first login attempt is finding out too late.
	keyStore, err := secrets.OpenKeyStore(opts.Paths.MasterKey)
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("open key custody: %w", err)
	}
	sealer, err := secrets.New(keyStore)
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("initialise sealing: %w", err)
	}
	opts.Logger.InfoContext(ctx, "credential key custody ready", "backend", sealer.Backend())

	// The registry validates every driver's descriptor against the interfaces it
	// actually implements, so a driver that lies about its capabilities stops
	// the daemon here rather than producing a client that offers a flow the
	// daemon will reject.
	// nil means "use the real drivers"; an explicitly empty slice means "no
	// drivers", which a test filtering its list down to nothing is entitled to
	// ask for. Treating both alike would have quietly pointed such a test at
	// api.anthropic.com.
	drivers := opts.Providers
	if drivers == nil {
		// The pin is passed IN rather than read inside the driver, so the
		// version number keeps living in exactly one place.
		claude, err := claudecode.NewDriver(
			opts.Paths.Providers, opts.Paths.ClaudeConfig, buildinfo.PinnedClaudeCodeVersion)
		if err != nil {
			_ = store.Close()
			return nil, fmt.Errorf("build the Claude Code driver: %w", err)
		}
		// Claude Code first: it is the provider tumika exists to drive, and the
		// order is what a client renders.
		drivers = []provider.Provider{claude, anthropicapi.New()}
	}
	registry, err := provider.NewRegistry(drivers...)
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("build the provider registry: %w", err)
	}

	providers := service.NewProviderService(
		registry, sqlite.NewProviderRepo(store), sqlite.NewCredentialRepo(store), sealer, config, store)

	if err := providers.Seed(ctx); err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("seed providers: %w", err)
	}

	// Self-update is disabled for a dev build and inside a container.
	//
	// A dev build has no release to update FROM — buildinfo.IsDev() is the
	// check. A container has no supervisor to relaunch it and the image is the
	// unit of deployment, so a container that rewrote its own binary would no
	// longer match its tag (ADR-0003). Both leave updates nil, and the API says
	// so rather than answering 404.
	// The runner is built here rather than beside the service above, because it
	// needs the resolved check interval and therefore the config service — which
	// does not exist until after migrations.
	var updateRunner *runner.Update
	if updates != nil {
		interval, err := service.Duration(ctx, config, service.KeyUpdateCheckInterval)
		if err != nil {
			_ = store.Close()
			return nil, fmt.Errorf("resolve the update check interval: %w", err)
		}
		binary, err := service.BinaryPath()
		if err != nil {
			_ = store.Close()
			return nil, err
		}
		// A jitter derived from the binary's own path, so a fleet restarted
		// together does not ask GitHub in lockstep — and so the same host picks
		// the same offset each boot rather than a new one.
		updateRunner = runner.NewUpdate(updates, config, opts.Logger, interval, jitterFor(binary, interval))
	}

	schemaVersion := func(ctx context.Context) (int64, error) {
		return sqlite.SchemaVersion(ctx, store)
	}

	return &Daemon{
		opts:         opts,
		store:        store,
		config:       config,
		auth:         auth,
		providers:    providers,
		sealer:       sealer,
		updates:      updates,
		updateRunner: updateRunner,
		appliedByAPI: make(chan struct{}),
		health: service.NewHealthService(
			buildinfo.Version(), time.Now(), schemaVersion, auth, sealer.Backend()),
		log:     opts.Logger,
		started: time.Now(),
	}, nil
}

// Close releases the database handles.
func (d *Daemon) Close() error { return d.store.Close() }

// ConfigService exposes the config service for the CLI's in-process uses.
func (d *Daemon) ConfigService() service.ConfigService { return d.config }

// AuthService exposes token management to `tumika token`, which has to work
// before a daemon is running — on a fresh install there is no token, so there is
// nothing to authenticate an HTTP call with.
func (d *Daemon) AuthService() service.AuthService { return d.auth }

// UpdateService exposes the updater to `tumika update`, which must work when
// the daemon is DOWN — an operator whose service is crash-looping is exactly
// who needs to update it, and an API client cannot help them.
//
// Nil for a development build and inside a container, where self-update is
// disabled.
func (d *Daemon) UpdateService() service.UpdateService { return d.updates }

// ProviderService exposes providers to the CLI's in-process uses.
func (d *Daemon) ProviderService() service.ProviderService { return d.providers }

// Sealer exposes credential sealing. ProviderService takes it at the next step;
// it is held here because key custody is a process-wide resource, resolved once.
func (d *Daemon) Sealer() secrets.Sealer { return d.sealer }

// Serve runs the HTTP API until ctx is cancelled, then drains.
func (d *Daemon) Serve(ctx context.Context) error {
	addr := d.opts.Listen
	if addr == "" {
		var err error
		if addr, err = service.String(ctx, d.config, service.KeyServerListen); err != nil {
			return fmt.Errorf("resolve listen address: %w", err)
		}
	}

	// Refuse to serve without a token, rather than listening unauthenticated.
	//
	// The alternative — minting one automatically at startup — would have to put
	// the plaintext somewhere the operator can read it, which means a log line
	// or a file. Both create a copy of a full-access credential that nobody
	// asked for. Failing with an instruction is worse UX for exactly one command
	// and better for everything after it.
	configured, err := d.auth.Configured(ctx)
	if err != nil {
		return fmt.Errorf("check the API token: %w", err)
	}
	if !configured {
		return fmt.Errorf("%w: run `tumika token rotate` to create one", service.ErrNoToken)
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
		Handler: api.NewRouter(api.Deps{
			Config:    d.config,
			Providers: d.providers,
			Health:    d.health,
			Auth:      d.auth,
			Updates:   d.updates,
			UpdateApplied: func() {
				// Signalled after the response is written. Closing the same
				// channel the runner uses means one shutdown path, whether the
				// update came from the schedule or from an operator.
				d.applyOnce.Do(func() { close(d.appliedByAPI) })
			},
			Logger:       d.log,
			AllowedHosts: allowedHosts(listener.Addr().String()),
			// No browser origin is accepted. The API is not called from a web
			// page, and there are no CORS headers anywhere to make one work.
			AllowedOrigins: nil,
		}),
		// A client that opens a connection and sends nothing must not hold a
		// slot indefinitely.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	// Confirmed once the listener is bound and the server is about to accept.
	//
	// Precisely what that proves, and does not: everything up to here has
	// succeeded — migrations, key custody, the provider registry, seeding,
	// resolving the listen address, and binding the port. That is the large
	// majority of what a bad release breaks. It does NOT prove the binary can
	// serve a REQUEST; a panic on first request is confirmed and its fallback
	// deleted. Waiting for a request instead would mean a daemon nobody happens
	// to call keeps a stale .old forever, which is its own failure.
	if d.updates != nil {
		if err := d.updates.Confirm(ctx); err != nil {
			// A stale .old is worth saying out loud: the next Apply refuses to
			// overwrite it, so an update would be blocked until it is removed.
			d.log.WarnContext(ctx, "the update was confirmed but its fallback remains", "err", err)
		}
	}

	// The update runner shares the server's lifetime. Cancelling runnerCtx on
	// return stops it whether the server exited cleanly or not.
	// DEFER ORDER MATTERS, and getting it wrong deadlocks the shutdown.
	//
	// Defers unwind last-in-first-out, so the wait is declared FIRST in order to
	// run LAST: cancel, then wait. Declaring the wait after the cancel — the
	// order that reads naturally — makes it run first, blocking on a runner that
	// has not yet been told to stop. That hung the shutdown until the test
	// timed out.
	var runners sync.WaitGroup
	defer runners.Wait()

	runnerCtx, stopRunners := context.WithCancel(ctx)
	defer stopRunners()

	// One shutdown signal, fed by both paths: the scheduled runner and an
	// operator calling POST /v1/update/apply. Two selects would mean two
	// shutdown implementations that could drift.
	applied := make(chan struct{})
	var appliedOnce sync.Once
	signalApplied := func() { appliedOnce.Do(func() { close(applied) }) }

	go func() {
		select {
		case <-d.appliedByAPI:
			signalApplied()
		case <-runnerCtx.Done():
		}
	}()

	// The runner is JOINED on the way out (see the WaitGroup above), not merely
	// cancelled. It can be inside Apply, between the rename that sets the old
	// binary aside and the rename that installs the new one — and exiting the
	// process in that window leaves NO file at the binary path, so the
	// supervisor has nothing to start.
	if d.updateRunner != nil {
		runners.Add(1)
		go func() {
			defer runners.Done()
			if err := d.updateRunner.Start(runnerCtx); err != nil {
				d.log.ErrorContext(runnerCtx, "the update runner stopped", "err", err)
			}
		}()
		go func() {
			select {
			case <-d.updateRunner.Exit():
				signalApplied()
			case <-runnerCtx.Done():
			}
		}()
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

	case <-applied:
		// An update was installed. Drain, then tell the caller to exit so the
		// supervisor relaunches onto the new binary.
		d.log.Info("restarting onto the updated binary")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			d.log.Error("graceful shutdown timed out; closing connections", "err", err)
			_ = srv.Close()
		}
		<-serveErr
		return ErrRestartRequired

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

// newUpdateService builds the updater, or nil where self-update does not apply.
//
// Separated from New's body so it can run immediately after the store opens —
// the boot resolution has to happen before anything a new binary might fail at.
func newUpdateService(ctx context.Context, opts Options, store *sqlite.Store) (service.UpdateService, error) {
	switch {
	case opts.Updates != nil:
		return opts.Updates, nil
	case buildinfo.IsDev():
		opts.Logger.InfoContext(ctx, "self-update disabled", "reason", "development build")
		return nil, nil
	case paths.InContainer():
		opts.Logger.InfoContext(ctx, "self-update disabled", "reason", "running in a container")
		return nil, nil
	}

	binary, err := service.BinaryPath()
	if err != nil {
		return nil, err
	}
	return service.NewUpdateService(
		sqlite.NewUpdateStateRepo(store), release.NewGitHub(), store,
		buildinfo.Version(), binary), nil
}

// jitterFor spreads a fleet's update checks out.
//
// Derived from the binary path rather than random, so the same host picks the
// same offset on every boot: a random one would re-roll on each restart, and a
// daemon that restarts often would check far more eagerly than the interval
// suggests. Bounded to a tenth of the interval — enough to break lockstep after
// a coordinated restart, not enough to matter.
func jitterFor(binaryPath string, interval time.Duration) time.Duration {
	span := interval / 10
	if span <= 0 {
		return 0
	}

	sum := sha256.Sum256([]byte(binaryPath))

	// The modulo happens in uint64 and the conversion afterwards, not the other
	// way round. Converting first can produce a NEGATIVE Duration whenever the
	// hash's high bit is set — which is half of all inputs — and a negative
	// jitter would fire the timer immediately, silently removing the spread this
	// exists to create. The result is below span, so it always fits.
	return time.Duration(binary.BigEndian.Uint64(sum[:8]) % uint64(span)) // #nosec G115 -- reduced modulo span first, so the value is bounded by span
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

// allowedHosts is the set of Host header values accepted for a name-based
// request.
//
// Literal IPs are accepted unconditionally by the middleware, so this only has
// to cover names. "localhost" is the one that matters: it is what a browser and
// most clients send for a loopback address, and it is also the name an attacker
// would have to get past to mount a DNS-rebinding attack.
func allowedHosts(addr string) []string {
	hosts := []string{"localhost"}

	if host, _, err := net.SplitHostPort(addr); err == nil && net.ParseIP(host) == nil && host != "" {
		hosts = append(hosts, host)
	}
	return hosts
}
