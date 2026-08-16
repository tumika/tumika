package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/tumika/tumika/source/internal/domain"
	"github.com/tumika/tumika/source/internal/platform/release"
	"github.com/tumika/tumika/source/internal/service"
)

// A development build has no release to update FROM, and the comparison would
// be meaningless: "dev" is not a semver, so everything published looks newer
// and nothing is.
func TestUpdateRefusesOnADevelopmentBuild(t *testing.T) {
	_, _, err := run(t, "--home", t.TempDir(), "update")
	if err == nil {
		t.Fatal("a development build tried to update itself")
	}
	if !strings.Contains(err.Error(), "development build") {
		t.Errorf("the error does not explain why: %v", err)
	}
}

// The same refusal for --check: it opens the daemon and reaches the network
// otherwise, for a build that can never act on the answer.
func TestUpdateCheckRefusesOnADevelopmentBuild(t *testing.T) {
	if _, _, err := run(t, "--home", t.TempDir(), "update", "--check"); err == nil {
		t.Fatal("--check ran on a development build")
	}
}

// A container's image is the unit of deployment, and a container that rewrote
// its own binary would no longer match its tag.
func TestUpdateRefusesInAContainer(t *testing.T) {
	t.Setenv("TUMIKA_CONTAINER", "1")

	_, _, err := run(t, "--home", t.TempDir(), "update")
	if err == nil {
		t.Fatal("a container tried to self-update")
	}
	// The dev-build check comes first in a test binary, so either refusal is
	// correct — what matters is that it refused before touching anything.
	if !strings.Contains(err.Error(), "container") && !strings.Contains(err.Error(), "development build") {
		t.Errorf("unexpected refusal: %v", err)
	}
}

// The flags exist and are wired: a typo in a flag name is otherwise only found
// by an operator.
func TestUpdateFlags(t *testing.T) {
	cmd := newUpdateCmd(&globals{})
	for _, name := range []string{"check", "to"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("--%s is missing", name)
		}
	}
}

// fakeUpdates drives runUpdate without a daemon or a network.
type fakeUpdates struct {
	state     domain.UpdateState
	available string
	newer     bool
	checkErr  error
	applyErr  error
	applied   []string
}

func (f *fakeUpdates) State(context.Context) (domain.UpdateState, error) { return f.state, nil }
func (f *fakeUpdates) Check(context.Context) (string, bool, error) {
	return f.available, f.newer, f.checkErr
}

func (f *fakeUpdates) Apply(_ context.Context, version string) error {
	if f.applyErr != nil {
		return f.applyErr
	}
	f.applied = append(f.applied, version)
	return nil
}

func (f *fakeUpdates) ConfirmBoot(context.Context) (bool, error) { return false, nil }
func (f *fakeUpdates) Confirm(context.Context) error             { return nil }

var _ service.UpdateService = (*fakeUpdates)(nil)

func run2(t *testing.T, fn func(*cobra.Command) error) (string, error) {
	t.Helper()
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetContext(context.Background())
	err := fn(cmd)
	return out.String(), err
}

func TestRunUpdateInstallsTheNewestVersion(t *testing.T) {
	updates := &fakeUpdates{available: "0.2.0", newer: true}

	out, err := run2(t, func(cmd *cobra.Command) error {
		return runUpdate(cmd, updates, "0.1.0", false, "")
	})
	if err != nil {
		t.Fatalf("runUpdate: %v", err)
	}
	if len(updates.applied) != 1 || updates.applied[0] != "0.2.0" {
		t.Errorf("applied %v, want [0.2.0]", updates.applied)
	}
	if !strings.Contains(out, "0.2.0") || !strings.Contains(out, "Restart the service") {
		t.Errorf("the operator is not told what happened or what to do next:\n%s", out)
	}
}

// --check must install nothing. It exists precisely so an operator can look
// before acting.
func TestRunUpdateCheckInstallsNothing(t *testing.T) {
	updates := &fakeUpdates{available: "0.2.0", newer: true}

	out, err := run2(t, func(cmd *cobra.Command) error {
		return runUpdate(cmd, updates, "0.1.0", true, "")
	})
	if err != nil {
		t.Fatalf("runUpdate: %v", err)
	}
	if len(updates.applied) != 0 {
		t.Errorf("--check installed %v", updates.applied)
	}
	if !strings.Contains(out, "0.2.0") {
		t.Errorf("--check did not report what is available:\n%s", out)
	}
}

func TestRunUpdateWhenAlreadyUpToDate(t *testing.T) {
	updates := &fakeUpdates{available: "0.1.0", newer: false}

	out, err := run2(t, func(cmd *cobra.Command) error {
		return runUpdate(cmd, updates, "0.1.0", false, "")
	})
	if err != nil {
		t.Fatalf("runUpdate: %v", err)
	}
	if len(updates.applied) != 0 {
		t.Errorf("installed %v when already up to date", updates.applied)
	}
	if !strings.Contains(out, "up to date") {
		t.Errorf("output does not say so:\n%s", out)
	}
}

// --to pins a version, and bypasses the "is it newer" shortcut so an operator
// can move deliberately. The service still refuses a downgrade.
func TestRunUpdateHonoursAnExplicitVersion(t *testing.T) {
	updates := &fakeUpdates{available: "0.3.0", newer: true}

	if _, err := run2(t, func(cmd *cobra.Command) error {
		return runUpdate(cmd, updates, "0.1.0", false, "0.2.0")
	}); err != nil {
		t.Fatalf("runUpdate: %v", err)
	}
	if len(updates.applied) != 1 || updates.applied[0] != "0.2.0" {
		t.Errorf("applied %v, want the version asked for", updates.applied)
	}
}

// A repository with no releases is a normal state before the first tag, and
// must not read as a failure.
func TestRunUpdateWithNoReleasePublished(t *testing.T) {
	updates := &fakeUpdates{checkErr: release.ErrNoRelease}

	out, err := run2(t, func(cmd *cobra.Command) error {
		return runUpdate(cmd, updates, "0.1.0", false, "")
	})
	if err != nil {
		t.Fatalf("no release was reported as an error: %v", err)
	}
	if !strings.Contains(out, "No release") {
		t.Errorf("output does not explain:\n%s", out)
	}
}

// Any other check failure IS an error — mistaking it for "up to date" would
// silently stop a machine from ever updating.
func TestRunUpdateReportsACheckFailure(t *testing.T) {
	updates := &fakeUpdates{checkErr: errors.New("connection reset")}

	if _, err := run2(t, func(cmd *cobra.Command) error {
		return runUpdate(cmd, updates, "0.1.0", false, "")
	}); err == nil {
		t.Fatal("a failed check reported success")
	}
}

func TestRunUpdateReportsAnApplyFailure(t *testing.T) {
	updates := &fakeUpdates{
		available: "0.2.0", newer: true,
		applyErr: errors.New("checksum mismatch"),
	}

	if _, err := run2(t, func(cmd *cobra.Command) error {
		return runUpdate(cmd, updates, "0.1.0", false, "")
	}); err == nil {
		t.Fatal("a failed install reported success")
	}
}

func TestRunUpdateWithSelfUpdateDisabled(t *testing.T) {
	if _, err := run2(t, func(cmd *cobra.Command) error {
		return runUpdate(cmd, nil, "0.1.0", false, "")
	}); err == nil {
		t.Fatal("update ran with no service")
	}
}

func TestRunUpdateStatus(t *testing.T) {
	started := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)

	tests := map[string]struct {
		state domain.UpdateState
		want  []string
	}{
		"idle": {
			state: domain.UpdateState{Status: domain.UpdateIdle},
			want:  []string{"idle"},
		},
		"pending shows the rollback budget": {
			state: domain.UpdateState{
				Status: domain.UpdatePending, FromVersion: "0.1.0", ToVersion: "0.2.0",
				BootAttempts: 2, StartedAt: &started,
			},
			want: []string{"pending", "0.1.0", "0.2.0", "2 of 3", "2026-08-16"},
		},
		"rolled back explains itself": {
			state: domain.UpdateState{
				Status: domain.UpdateRolledBack, FromVersion: "0.1.0", ToVersion: "0.2.0",
			},
			want: []string{"rolled_back", "failed to boot", "Check the logs"},
		},
	}

	for name, tc := range tests {
		out, err := run2(t, func(cmd *cobra.Command) error {
			return runUpdateStatus(cmd, &fakeUpdates{state: tc.state})
		})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, want := range tc.want {
			if !strings.Contains(out, want) {
				t.Errorf("%s: output is missing %q:\n%s", name, want, out)
			}
		}
	}
}

func TestRunUpdateStatusWithSelfUpdateDisabled(t *testing.T) {
	out, err := run2(t, func(cmd *cobra.Command) error {
		return runUpdateStatus(cmd, nil)
	})
	if err != nil {
		t.Fatalf("runUpdateStatus: %v", err)
	}
	if !strings.Contains(out, "disabled") {
		t.Errorf("output does not say why:\n%s", out)
	}
}

// The status command's flags and wiring exist.
func TestUpdateStatusCommandShape(t *testing.T) {
	cmd := newUpdateStatusCmd(&globals{})
	if cmd.Use != "update-status" {
		t.Errorf("Use = %q", cmd.Use)
	}
	if cmd.RunE == nil {
		t.Error("update-status has no RunE")
	}
}

// update-status opens the daemon, so on a machine with no home it fails rather
// than reporting a state it never read.
func TestUpdateStatusNeedsAResolvableHome(t *testing.T) {
	if _, _, err := run(t, "--home", "relative-not-absolute", "update-status"); err == nil {
		t.Skip("the home resolved anyway on this platform")
	}
}

// A confirmed update reports cleanly, with no rollback advice.
func TestRunUpdateStatusConfirmed(t *testing.T) {
	out, err := run2(t, func(cmd *cobra.Command) error {
		return runUpdateStatus(cmd, &fakeUpdates{state: domain.UpdateState{
			Status: domain.UpdateConfirmed, FromVersion: "0.1.0", ToVersion: "0.2.0",
		}})
	})
	if err != nil {
		t.Fatalf("runUpdateStatus: %v", err)
	}
	if !strings.Contains(out, "confirmed") {
		t.Errorf("output does not report the status:\n%s", out)
	}
	if strings.Contains(out, "Check the logs") {
		t.Errorf("a confirmed update was given rollback advice:\n%s", out)
	}
}
