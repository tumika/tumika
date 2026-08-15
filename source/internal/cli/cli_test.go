package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/tumika/tumika/source/internal/platform/paths"
)

// run executes the root command with args and returns stdout and any error.
func run(t *testing.T, args ...string) (string, string, error) {
	t.Helper()

	cmd := newRootCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)

	err := cmd.Execute()
	return out.String(), errOut.String(), err
}

func TestVersionReportsBuildInfo(t *testing.T) {
	t.Setenv(paths.HomeEnv, "/var/lib/tumika")

	out, _, err := run(t, "version")
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	for _, want := range []string{"tumika ", "claude-code ", "home: /var/lib/tumika"} {
		if !strings.Contains(out, want) {
			t.Errorf("version output missing %q:\n%s", want, out)
		}
	}
}

// The updater execs `<staged> version` to assert the semver before replacing the
// live binary (ADR-0003), and a supervised daemon may have no HOME. If this
// command can fail for want of a home directory, an update cannot pre-flight.
func TestVersionSurvivesAnUnresolvableHome(t *testing.T) {
	t.Setenv(paths.HomeEnv, "")
	t.Setenv("HOME", "")
	t.Setenv("XDG_STATE_HOME", "")

	out, _, err := run(t, "version")
	if err != nil {
		t.Fatalf("version must not fail when the home directory cannot be resolved: %v", err)
	}
	if !strings.Contains(out, "tumika ") {
		t.Errorf("version must still report the build:\n%s", out)
	}
	if !strings.Contains(out, "unresolved") {
		t.Errorf("version should say the home directory is unresolved:\n%s", out)
	}
}

func TestVersionJSONIsParseable(t *testing.T) {
	t.Setenv(paths.HomeEnv, "/var/lib/tumika")

	out, _, err := run(t, "version", "--json")
	if err != nil {
		t.Fatalf("version --json: %v", err)
	}

	var info struct {
		Version   string `json:"version"`
		Platform  string `json:"platform"`
		ClaudeCLI string `json:"claude_cli"`
	}
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		t.Fatalf("version --json emitted invalid JSON (%v):\n%s", err, out)
	}
	if info.Version == "" || info.Platform == "" || info.ClaudeCLI == "" {
		t.Errorf("version --json is missing fields: %+v", info)
	}
}

func TestPathsAreResolvedOnceAndReused(t *testing.T) {
	t.Setenv(paths.HomeEnv, "/var/lib/tumika")

	g := &globals{home: "/explicit"}

	first, err := g.Paths()
	if err != nil {
		t.Fatalf("Paths: %v", err)
	}
	if first.Home != "/explicit" {
		t.Errorf("Home = %q, want the explicit override", first.Home)
	}

	// Changing the environment after the first call must not change the answer:
	// a command that resolved paths once should not see them move underneath it.
	t.Setenv(paths.HomeEnv, "/somewhere/else")
	second, err := g.Paths()
	if err != nil {
		t.Fatalf("Paths again: %v", err)
	}
	if second.Home != first.Home {
		t.Errorf("Paths returned %q then %q; resolution should be memoised", first.Home, second.Home)
	}
}

func TestPathsErrorIsMemoisedToo(t *testing.T) {
	t.Setenv(paths.HomeEnv, "")
	t.Setenv("HOME", "")
	t.Setenv("XDG_STATE_HOME", "")

	g := &globals{}
	if _, err := g.Paths(); err == nil {
		t.Fatal("expected an error with no home directory available")
	}
	if _, err := g.Paths(); err == nil {
		t.Fatal("the error must be reported consistently on every call")
	}
}

func TestInvalidLogLevelIsRejected(t *testing.T) {
	if _, _, err := run(t, "--log-level", "shout", "version"); err == nil {
		t.Fatal("an unknown log level must be an error")
	}
}

func TestInvalidLogFormatIsRejected(t *testing.T) {
	if _, _, err := run(t, "--log-format", "yaml", "version"); err == nil {
		t.Fatal("an unknown log format must be an error")
	}
}

// A cancelled context is how a graceful shutdown ends. It must produce neither a
// non-zero exit nor an "Error:" line: a journal that logs one on every clean
// stop teaches operators to ignore the word, and log-scraping alerts fire on
// every restart.
func TestCancellationIsNotReportedAsAFailure(t *testing.T) {
	var errOut bytes.Buffer
	cmd := &cobra.Command{
		Use:           "fake",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          func(*cobra.Command, []string) error { return context.Canceled },
	}
	cmd.SetOut(io.Discard)
	cmd.SetErr(&errOut)

	if code := execute(context.Background(), cmd); code != 0 {
		t.Errorf("exit code = %d, want 0 for a cancelled context", code)
	}
	if errOut.Len() != 0 {
		t.Errorf("cancellation printed to stderr: %q", errOut.String())
	}
}

// A real failure still has to be both reported and non-zero — the previous
// behaviour relied on cobra printing, which is what caused the cancellation
// case above to print too.
func TestRealErrorsAreReportedAndNonZero(t *testing.T) {
	var errOut bytes.Buffer
	cmd := &cobra.Command{
		Use:           "fake",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          func(*cobra.Command, []string) error { return errors.New("the disk is full") },
	}
	cmd.SetOut(io.Discard)
	cmd.SetErr(&errOut)

	if code := execute(context.Background(), cmd); code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "the disk is full") {
		t.Errorf("stderr = %q, want the error reported", errOut.String())
	}
}

func TestRootCommandSilencesCobrasOwnErrorPrinting(t *testing.T) {
	if !newRootCmd().SilenceErrors {
		t.Error("cobra must not print errors itself; Execute decides what is a failure")
	}
}
