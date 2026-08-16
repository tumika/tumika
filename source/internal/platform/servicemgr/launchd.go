package servicemgr

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Launchd supervises tumika with a per-user LaunchAgent.
//
// An AGENT, not a LaunchDaemon. Key custody on macOS is the login Keychain
// (ADR-0002), and a LaunchDaemon runs outside any login session — so it would
// start before the Keychain is unlocked and fail to open a single credential.
// The cost is real and worth stating: the daemon runs only while the operator
// is logged in. TUMIKA_MASTER_KEY is the answer for anyone who needs otherwise,
// and a Mac is not the deployment this project is aimed at.
type Launchd struct {
	agentDir string
	run      runner
	uid      int
}

// LaunchdOption configures the manager, for tests.
type LaunchdOption func(*Launchd)

func withLaunchdAgentDir(dir string) LaunchdOption {
	return func(l *Launchd) { l.agentDir = dir }
}

func withLaunchdRunner(r runner) LaunchdOption {
	return func(l *Launchd) { l.run = r }
}

func withLaunchdUID(uid int) LaunchdOption {
	return func(l *Launchd) { l.uid = uid }
}

// NewLaunchd builds the macOS manager.
func NewLaunchd(opts ...LaunchdOption) (*Launchd, error) {
	l := &Launchd{run: execRunner, uid: os.Getuid()}
	for _, opt := range opts {
		opt(l)
	}

	if l.agentDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("locate the home directory for the LaunchAgent: %w", err)
		}
		l.agentDir = filepath.Join(home, "Library", "LaunchAgents")
	}
	return l, nil
}

func (l *Launchd) plistPath() string {
	return filepath.Join(l.agentDir, Label+".plist")
}

// target is the service's address in launchd's domain syntax.
func (l *Launchd) target() string {
	return "gui/" + strconv.Itoa(l.uid) + "/" + Label
}

func (l *Launchd) domain() string { return "gui/" + strconv.Itoa(l.uid) }

// Install writes the plist and bootstraps it.
//
// bootstrap/bootout, not the load/unload pair. The older commands are
// deprecated, they report success for things they did not do, and on current
// macOS they are a compatibility shim over exactly these.
func (l *Launchd) Install(ctx context.Context, cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}

	plist, err := renderPlist(cfg)
	if err != nil {
		return err
	}

	// 0755 matches what macOS itself creates ~/Library/LaunchAgents as. The
	// plist holds no secret, and tightening a directory the OS owns would be a
	// surprise to everything else that writes agents there.
	if err := os.MkdirAll(l.agentDir, 0o755); err != nil { // #nosec G301 -- matches the mode macOS gives ~/Library/LaunchAgents; the plist holds no secret
		return fmt.Errorf("create %s: %w", l.agentDir, err)
	}
	if err := os.WriteFile(l.plistPath(), plist, 0o644); err != nil { // #nosec G306 -- a LaunchAgent plist is read by launchd and holds no secret
		return fmt.Errorf("write %s: %w", l.plistPath(), err)
	}

	// Bootstrapping over an existing service fails rather than replacing, so an
	// install onto a running service — which is what an upgrade is — has to boot
	// the old one out first. Its failure is ignored on purpose: "it was not
	// loaded" is the expected answer on a first install.
	if out, err := l.run(ctx, "launchctl", "bootout", l.target()); err != nil {
		_ = out
	}

	if out, err := l.run(ctx, "launchctl", "bootstrap", l.domain(), l.plistPath()); err != nil {
		return commandError("launchctl bootstrap "+l.plistPath(), out, err)
	}
	// RunAtLoad starts it now; enable makes the choice survive an explicit
	// `launchctl disable` from an earlier install, which bootstrap alone does
	// not undo.
	if out, err := l.run(ctx, "launchctl", "enable", l.target()); err != nil {
		return commandError("launchctl enable "+l.target(), out, err)
	}
	return nil
}

// Uninstall boots the service out and removes the plist. Data is left alone.
func (l *Launchd) Uninstall(ctx context.Context) error {
	if _, err := os.Stat(l.plistPath()); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: no LaunchAgent at %s", ErrNotInstalled, l.plistPath())
	}

	if out, err := l.run(ctx, "launchctl", "bootout", l.target()); err != nil {
		_ = out
	}
	if err := os.Remove(l.plistPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", l.plistPath(), err)
	}
	return nil
}

func (l *Launchd) Start(ctx context.Context) error {
	if err := l.requireInstalled(); err != nil {
		return err
	}
	if out, err := l.run(ctx, "launchctl", "kickstart", l.target()); err != nil {
		return commandError("launchctl kickstart "+l.target(), out, err)
	}
	return nil
}

// Stop kills the running process without unloading the service.
//
// `launchctl kill` rather than bootout: booting out would also remove it from
// the domain, so a later Start would fail with "service not found" and the
// operator would have to reinstall to undo a stop.
func (l *Launchd) Stop(ctx context.Context) error {
	if err := l.requireInstalled(); err != nil {
		return err
	}
	if out, err := l.run(ctx, "launchctl", "kill", "SIGTERM", l.target()); err != nil {
		return commandError("launchctl kill "+l.target(), out, err)
	}
	return nil
}

func (l *Launchd) requireInstalled() error {
	if _, err := os.Stat(l.plistPath()); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: no LaunchAgent at %s", ErrNotInstalled, l.plistPath())
	}
	return nil
}

// Status reads `launchctl print`, which is the only place launchd reports both
// whether a service is loaded and whether it currently has a process.
func (l *Launchd) Status(ctx context.Context) (Status, error) {
	status := Status{Manager: "launchd", Path: l.plistPath(), State: StateNotInstalled}

	if _, err := os.Stat(l.plistPath()); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return status, nil
		}
		return status, fmt.Errorf("stat %s: %w", l.plistPath(), err)
	}

	// The plist exists, so it is installed even if launchd has never loaded it.
	status.Enabled = true

	out, err := l.run(ctx, "launchctl", "print", l.target())
	if err != nil {
		// Not loaded into the domain. Installed but stopped — not a failure of
		// the status call.
		status.State = StateStopped
		status.Detail = "not loaded"
		return status, nil
	}

	text := string(out)
	switch {
	case pidLine(text) != "":
		status.State = StateRunning
		status.Detail = "pid " + pidLine(text)
	case strings.Contains(text, "last exit code = 0"):
		status.State = StateStopped
	case strings.Contains(text, "last exit code ="):
		status.State = StateFailed
		status.Detail = strings.TrimSpace(field(text, "last exit code ="))
	default:
		status.State = StateStopped
	}
	return status, nil
}

// pidLine extracts the running pid, if launchd reports one.
func pidLine(text string) string {
	return strings.TrimSpace(field(text, "pid ="))
}

func field(text, key string) string {
	for line := range strings.SplitSeq(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(trimmed, key); ok {
			return after
		}
	}
	return ""
}

// renderPlist builds the LaunchAgent.
//
// encoding/xml does the escaping rather than a string template. Every value
// here is validated first, so this is belt and braces — but a path with an
// ampersand in it is an ordinary thing on a Mac, and a plist that fails to parse
// is a service that silently never starts.
func renderPlist(cfg Config) ([]byte, error) {
	type entry struct {
		key   string
		value any
	}

	entries := []entry{
		{"Label", Label},
		{"ProgramArguments", []string{cfg.Binary, "serve"}},
		{"EnvironmentVariables", map[string]string{"TUMIKA_HOME": cfg.Home}},
		{"RunAtLoad", true},
		// KeepAlive, because an update exits ZERO to be relaunched on the new
		// binary (ADR-0003). SuccessfulExit=false — the launchd spelling of
		// "restart even on a clean exit" — would refuse exactly that relaunch.
		{"KeepAlive", true},
		{"ProcessType", "Background"},
		{"StandardOutPath", filepath.Join(cfg.Home, "logs", "tumika.log")},
		{"StandardErrorPath", filepath.Join(cfg.Home, "logs", "tumika.err.log")},
		{"WorkingDirectory", cfg.Home},
	}

	var b strings.Builder
	b.WriteString(xml.Header)
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString("<plist version=\"1.0\">\n<dict>\n")

	for _, e := range entries {
		if err := writePlistKey(&b, e.key, e.value); err != nil {
			return nil, err
		}
	}

	b.WriteString("</dict>\n</plist>\n")
	return []byte(b.String()), nil
}

func writePlistKey(b *strings.Builder, key string, value any) error {
	if err := writeEscaped(b, "\t<key>", key, "</key>\n"); err != nil {
		return err
	}

	switch v := value.(type) {
	case string:
		return writeEscaped(b, "\t<string>", v, "</string>\n")
	case bool:
		if v {
			b.WriteString("\t<true/>\n")
		} else {
			b.WriteString("\t<false/>\n")
		}
		return nil
	case []string:
		b.WriteString("\t<array>\n")
		for _, item := range v {
			if err := writeEscaped(b, "\t\t<string>", item, "</string>\n"); err != nil {
				return err
			}
		}
		b.WriteString("\t</array>\n")
		return nil
	case map[string]string:
		b.WriteString("\t<dict>\n")
		// Sorted so the file is byte-identical run to run: an install that
		// rewrote the plist with the same content in a different order would
		// look like a change to anything watching it.
		for _, name := range sortedKeys(v) {
			if err := writeEscaped(b, "\t\t<key>", name, "</key>\n"); err != nil {
				return err
			}
			if err := writeEscaped(b, "\t\t<string>", v[name], "</string>\n"); err != nil {
				return err
			}
		}
		b.WriteString("\t</dict>\n")
		return nil
	default:
		return fmt.Errorf("cannot render %T into a plist", value)
	}
}

func writeEscaped(b *strings.Builder, prefix, value, suffix string) error {
	b.WriteString(prefix)
	if err := xml.EscapeText(b, []byte(value)); err != nil {
		return fmt.Errorf("escape %q for the plist: %w", value, err)
	}
	b.WriteString(suffix)
	return nil
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
