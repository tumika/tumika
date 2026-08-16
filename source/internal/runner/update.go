// Package runner holds the supervised long-lived processes the daemon owns.
//
// A runner depends on SERVICES, never on repositories — see
// .agents/rules/runners-depend-on-services-never-repositories.md. It is the
// scheduling and lifecycle half; every decision belongs to the service it calls.
package runner

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/tumika/tumika/source/internal/platform/release"
	"github.com/tumika/tumika/source/internal/service"
)

// Runner is a supervised long-lived process.
type Runner interface {
	Name() string
	// Start blocks until ctx is cancelled or the runner fails.
	Start(ctx context.Context) error
	// Stop releases anything Start acquired. Called after Start returns.
	Stop(ctx context.Context) error
}

// Update checks for a newer release on a schedule and, when configured to,
// applies it.
//
// Applying an update ends with the process EXITING so the supervisor relaunches
// it on the new binary (ADR-0003). That exit is not this runner's to perform:
// it signals through Exit, and the daemon decides when — only the daemon knows
// whether a request is still in flight.
type Update struct {
	updates service.UpdateService
	// config is narrowed to the one method this runner uses. ConfigService also
	// exposes ReadSecret and WriteSecret, and a runner whose only question is
	// "is auto-apply on" has no business holding the interface that reaches
	// stored credentials.
	config   service.SettingGetter
	log      *slog.Logger
	interval time.Duration
	// exit is closed once an update has been installed and the process should
	// restart onto it.
	exit chan struct{}
	// exitOnce guards the close. The daemon drains for up to its shutdown grace
	// after an update lands, so a tick can fire in that window and reach the
	// close a second time — which panics the whole process, not just the runner.
	exitOnce sync.Once
	// jitter offsets the first check. Without it every tumika on earth would
	// ask GitHub at the same moment after a coordinated restart.
	jitter time.Duration
}

// NewUpdate builds the runner.
func NewUpdate(
	updates service.UpdateService,
	config service.SettingGetter,
	log *slog.Logger,
	interval, jitter time.Duration,
) *Update {
	return &Update{
		updates:  updates,
		config:   config,
		log:      log,
		interval: interval,
		exit:     make(chan struct{}),
		jitter:   jitter,
	}
}

func (u *Update) Name() string { return "update" }

// Exit is closed when an update has been installed and the daemon should stop
// so the supervisor relaunches it.
func (u *Update) Exit() <-chan struct{} { return u.exit }

func (u *Update) Start(ctx context.Context) error {
	if u.jitter > 0 {
		select {
		case <-time.After(u.jitter):
		case <-ctx.Done():
			return nil
		}
	}

	ticker := time.NewTicker(u.interval)
	defer ticker.Stop()

	// Checked once at startup as well as on the tick: a daemon that was down
	// for a week should not wait another interval to learn it is behind.
	u.once(ctx)

	for {
		// Checked at the TOP of every iteration, including immediately after the
		// startup check above. Only testing it after a tick meant an update
		// applied at startup left the loop running, so the next tick applied a
		// second time — destroying the rollback fallback — and then closed an
		// already-closed channel. Deterministic whenever the check interval is
		// shorter than the daemon's drain, which is an operator-settable value.
		select {
		case <-u.exit:
			return nil
		default:
		}

		select {
		case <-ctx.Done():
			return nil
		case <-u.exit:
			return nil
		case <-ticker.C:
			u.once(ctx)
		}
	}
}

func (u *Update) Stop(context.Context) error { return nil }

// once runs a single check, and applies if configured to.
//
// Every failure here is logged and swallowed. An update check is a background
// convenience: GitHub being unreachable, rate-limiting, or having no release
// yet must never take down a daemon whose actual job is running workflows.
func (u *Update) once(ctx context.Context) {
	available, newer, err := u.updates.Check(ctx)
	if err != nil {
		if errors.Is(err, release.ErrNoRelease) {
			// Normal before the first tag. Debug, not warn — a project without
			// releases should not log a warning every six hours.
			u.log.DebugContext(ctx, "no release published yet")
			return
		}
		u.log.WarnContext(ctx, "update check failed", "err", err)
		return
	}
	if !newer {
		u.log.DebugContext(ctx, "up to date", "available", available)
		return
	}

	auto, err := service.Bool(ctx, u.config, service.KeyUpdateAutoApply)
	if err != nil {
		u.log.WarnContext(ctx, "reading the auto-apply setting failed", "err", err)
		return
	}
	if !auto {
		// Told, not acted on. An operator who turned auto-apply off still wants
		// to know there is something to apply.
		u.log.InfoContext(ctx, "an update is available", "version", available,
			"apply", "POST /v1/update/apply")
		return
	}

	u.log.InfoContext(ctx, "applying update", "version", available)
	if err := u.updates.Apply(ctx, available); err != nil {
		u.log.ErrorContext(ctx, "update failed; staying on the current version",
			"version", available, "err", err)
		return
	}

	u.log.InfoContext(ctx, "update installed; exiting so the supervisor restarts onto it",
		"version", available)
	u.exitOnce.Do(func() { close(u.exit) })
}
