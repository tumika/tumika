package claudecode

import (
	"context"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"time"
)

// minimalPath is the PATH handed to a spawned claude.
//
// Not the daemon's own PATH: that is whatever the operator's shell, systemd unit
// or container image happened to export, and it is a lookup surface tumika does
// not control. The binary itself is executed by absolute path, so this exists
// only for the handful of standard tools a Node program may shell out to.
const minimalPath = "/usr/local/bin:/usr/bin:/bin"

// deniedEnv is every variable that can redirect Claude Code away from the
// subscription token tumika supplies.
//
// The env is built from a fixed list rather than filtered from os.Environ(), so
// none of these can arrive by accident. The list still exists — and is still
// applied, in scrub — because the failure it guards is a LATER edit: the day
// someone adds an option to pass a variable through, or copies the daemon's
// environment in for a debugging session, this is what keeps the guarantee.
//
// Precedence is:
//
//	cloud vars → ANTHROPIC_AUTH_TOKEN → ANTHROPIC_API_KEY → apiKeyHelper → CLAUDE_CODE_OAUTH_TOKEN
//
// tumika's token is LAST: every one of these outranks it, which is why the list
// is long rather than the two obvious ones. It is NOT a proof of completeness —
// Claude Code can add a variable tomorrow, and this file would not know. The
// guarantee comes from building the environment from a fixed list; this is the
// backstop for when that stops being true. See
// .agents/rules/every-spawned-claude-process-is-credential-isolated.md.
var deniedEnv = []string{
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_AUTH_TOKEN",
	"ANTHROPIC_BASE_URL",
	"ANTHROPIC_PROFILE",
	"ANTHROPIC_MODEL",
	// Can carry an Authorization header, which reroutes billing outright
	// without touching any of the variables above.
	"ANTHROPIC_CUSTOM_HEADERS",
	"ANTHROPIC_BEDROCK_BASE_URL",
	"ANTHROPIC_VERTEX_BASE_URL",
	"CLAUDE_CODE_USE_BEDROCK",
	"CLAUDE_CODE_USE_VERTEX",
	"CLAUDE_CODE_USE_FOUNDRY",
	"AWS_ACCESS_KEY_ID",
	"AWS_SECRET_ACCESS_KEY",
	"AWS_SESSION_TOKEN",
	"AWS_PROFILE",
	"AWS_REGION",
	"AWS_BEARER_TOKEN_BEDROCK",
	"GOOGLE_APPLICATION_CREDENTIALS",
	"GOOGLE_CLOUD_PROJECT",
	"CLOUD_ML_REGION",
	"AZURE_OPENAI_API_KEY",
}

// command builds the one kind of claude process tumika ever spawns.
//
// This is the ONLY place a *exec.Cmd for the vendored binary is constructed.
// Today that means Verify's two stages; the PTY login will come through here
// too, and a PTY does not exempt it. Preflight deliberately spawns nothing — it
// only stats the binary.
//
// Nothing mechanical enforces the "only place" property: it is a review
// obligation, and the reason the constructor is unexported and takes the token
// as a parameter is to make going around it look as odd as it is.
//
// The isolation measures are ONE policy, and any one of them missing puts the
// operator on API billing with no symptom at all: everything still works, and
// the difference arrives weeks later on an invoice.
//
// The token is passed per call rather than held on the driver, because a
// credential's lifetime is the call it was opened for.
func (d *Driver) command(ctx context.Context, token string, args ...string) *exec.Cmd {
	// --setting-sources '' comes first so no caller can forget it, and so it
	// cannot be shadowed by a later positional.
	//
	// This is the measure that is easy to omit and impossible to notice.
	// apiKeyHelper is a SETTINGS-FILE key, not an environment variable: it
	// outranks the token tumika injects, it is invisible to `env`, and a
	// perfectly scrubbed environment does not touch it. Refusing to load
	// settings files at all is the only thing that closes it.
	args = append([]string{"--setting-sources", ""}, args...)

	// Absolute path, pinned version. No PATH lookup to win and no launcher
	// symlink to repoint, so the binary that runs is the one that was verified
	// against Anthropic's signed manifest.
	//
	// Both scanners flag the variable path, and the audit they are asking for is
	// this: the path is <providersRoot>/claude-code/<version>/claude, where the
	// root comes from tumika's own layout and the version is d.version — the pin,
	// which NewDriver put through validateVersion, so it cannot contain a
	// separator or escape the root. It is never the version a caller passes to
	// Install: that one reaches the installer and never this function. Every
	// element of args is a constant in this package.
	//
	// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	cmd := exec.CommandContext(ctx, d.installer.Path(d.version), args...) // #nosec G204 -- see the audit above: the path is the validated pin under tumika's own root, and args are package constants

	cmd.Env = scrub([]string{
		"PATH=" + minimalPath,
		// HOME is tumika's isolated Claude directory, not the operator's. A
		// Node program that writes a cache beside its config should write it
		// inside the tree tumika owns.
		"HOME=" + d.configDir,
		"CLAUDE_CONFIG_DIR=" + d.configDir,
		// Claude Code updates itself by default. The login parser is written
		// against an exact version, so a silent self-update is a silent break —
		// which is the whole reason the pin exists.
		"DISABLE_AUTOUPDATER=1",
		// Never --bare: it does not read this variable, and falls through the
		// precedence chain to whatever is next.
		"CLAUDE_CODE_OAUTH_TOKEN=" + token,
		// Ink draws a TUI when it thinks it has one. Everything here is
		// parsed, so the redraws are noise.
		"NO_COLOR=1",
		"TERM=dumb",
	})

	cmd.Dir = d.configDir

	// Its own process group, and the whole group is what gets killed.
	//
	// Claude Code is a Node program that spawns children. exec.CommandContext
	// signals only the process it started, so a timeout would leave those
	// children running — and this is a daemon, so they accumulate for as long as
	// it is up. Putting the child in its own group means the negative-PID signal
	// below reaches all of them.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	// Bounds the wait for a child that ignores the signal, or holds stdout open
	// after exiting. Without it, cmd.Output can block past its own deadline.
	cmd.WaitDelay = 5 * time.Second

	return cmd
}

// scrub drops any denied variable from an assembled environment.
//
// Redundant against the fixed list above, and deliberately so: the list is a
// property of today's code, and this is a property of the function. It protects
// what is passed THROUGH it and nothing else — a later `cmd.Env = append(...)`
// after the call below would go straight round it. That is the case to watch
// for in review; scrub cannot catch it.
func scrub(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, ok := strings.Cut(kv, "=")
		if ok && slices.Contains(deniedEnv, name) {
			continue
		}
		out = append(out, kv)
	}
	return out
}
