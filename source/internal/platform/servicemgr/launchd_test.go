package servicemgr

import (
	"context"
	"encoding/xml"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestLaunchd(t *testing.T, rec *recorder, opts ...LaunchdOption) (*Launchd, string) {
	t.Helper()

	agentDir := t.TempDir()
	base := []LaunchdOption{
		withLaunchdAgentDir(agentDir),
		withLaunchdRunner(rec.run),
		withLaunchdUID(501),
	}
	mgr, err := NewLaunchd(append(base, opts...)...)
	if err != nil {
		t.Fatalf("NewLaunchd: %v", err)
	}
	return mgr, agentDir
}

func TestLaunchdInstallWritesAPlistAndBootstrapsIt(t *testing.T) {
	rec := newRecorder()
	mgr, agentDir := newTestLaunchd(t, rec)

	cfg := testConfig(t)
	if err := mgr.Install(t.Context(), cfg); err != nil {
		t.Fatalf("Install: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(agentDir, Label+".plist"))
	if err != nil {
		t.Fatalf("no plist was written: %v", err)
	}

	// It has to be a plist launchd can parse. A file that fails to parse is a
	// service that silently never starts, which no string check would catch.
	var parsed any
	if err := xml.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("the plist is not well-formed XML: %v\n%s", err, raw)
	}

	text := string(raw)
	for _, want := range []string{
		"<key>Label</key>",
		Label,
		cfg.Binary,
		"<key>RunAtLoad</key>",
		// KeepAlive, because an update exits ZERO to be relaunched on the new
		// binary — the launchd spelling of "restart only on failure" would
		// refuse exactly that.
		"<key>KeepAlive</key>",
		"TUMIKA_HOME",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the plist is missing %q:\n%s", want, text)
		}
	}

	// bootout before bootstrap: bootstrapping over a loaded service fails
	// rather than replacing, so an upgrade would break without it.
	bootout := rec.indexOf(t, "launchctl bootout")
	if bootout > rec.indexOf(t, "launchctl bootstrap") {
		t.Errorf("bootstrap ran before bootout: %v", rec.joined())
	}
	if !strings.Contains(strings.Join(rec.joined(), "\n"), "gui/501") {
		t.Errorf("the calls do not target the user's gui domain: %v", rec.joined())
	}
}

// A first install has nothing to boot out, and launchctl says so. Treating that
// as a failure would make every fresh install fail.
func TestLaunchdInstallToleratesNothingToBootOut(t *testing.T) {
	rec := newRecorder()
	rec.fail["launchctl bootout"] = errors.New("exit status 3")
	mgr, _ := newTestLaunchd(t, rec)

	if err := mgr.Install(t.Context(), testConfig(t)); err != nil {
		t.Fatalf("a first install failed because there was nothing to unload: %v", err)
	}
}

// XML escaping is not optional: an ampersand in a path is an ordinary thing on
// a Mac, and an unescaped one makes the plist unparseable.
func TestLaunchdEscapesPathsIntoThePlist(t *testing.T) {
	rec := newRecorder()
	mgr, agentDir := newTestLaunchd(t, rec)

	home := filepath.Join(t.TempDir(), "Music & Video <archive>")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	cfg := testConfig(t)
	cfg.Home = home
	if err := mgr.Install(t.Context(), cfg); err != nil {
		t.Fatalf("Install: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(agentDir, Label+".plist"))
	if err != nil {
		t.Fatalf("read the plist: %v", err)
	}
	if strings.Contains(string(raw), "& Video") {
		t.Errorf("an ampersand went in unescaped:\n%s", raw)
	}

	var parsed any
	if err := xml.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("the plist is not well-formed with an awkward path: %v\n%s", err, raw)
	}
}

// Stop has to actually stop it.
//
// `launchctl kill` looks gentler and stops nothing: the plist sets KeepAlive, so
// launchd relaunches the process a moment later while `tumika stop` reports
// success. KeepAlive is not negotiable — an update exits zero to be relaunched
// on the new binary — so Stop unloads instead.
func TestLaunchdStopUnloadsTheServiceRatherThanKillingIt(t *testing.T) {
	rec := newRecorder()
	mgr, agentDir := newTestLaunchd(t, rec)
	if err := os.WriteFile(filepath.Join(agentDir, Label+".plist"), []byte("<plist/>"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := mgr.Stop(t.Context()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	calls := strings.Join(rec.joined(), "\n")
	if !strings.Contains(calls, "launchctl bootout") {
		t.Errorf("Stop did not unload the service, so KeepAlive would relaunch it: %v", rec.joined())
	}
	// The plist stays, so a later Start needs no reinstall.
	if _, err := os.Stat(filepath.Join(agentDir, Label+".plist")); err != nil {
		t.Error("Stop removed the plist, so starting again would need a reinstall")
	}
}

// And Start has to load it back, since Stop unloaded it. A bare kickstart would
// fail with "service not found" after a stop.
func TestLaunchdStartReloadsAfterAStop(t *testing.T) {
	rec := newRecorder()
	mgr, agentDir := newTestLaunchd(t, rec)
	if err := os.WriteFile(filepath.Join(agentDir, Label+".plist"), []byte("<plist/>"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := mgr.Stop(t.Context()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := mgr.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	calls := strings.Join(rec.joined(), "\n")
	if !strings.Contains(calls, "launchctl bootstrap") {
		t.Errorf("Start did not load the service back into the domain: %v", rec.joined())
	}
	if !strings.Contains(calls, "launchctl kickstart") {
		t.Errorf("Start did not start the service: %v", rec.joined())
	}
}

func TestLaunchdStatus(t *testing.T) {
	tests := []struct {
		name    string
		out     string
		err     error
		want    State
		enabled bool
	}{
		{"running", "state = running\n\tpid = 4242\n", nil, StateRunning, true},
		{"clean exit", "\tlast exit code = 0\n", nil, StateStopped, true},
		{"failed", "\tlast exit code = 1\n", nil, StateFailed, true},
		{"not loaded", "", errors.New("exit status 113"), StateStopped, true},
	}

	for _, tc := range tests {
		rec := newRecorder()
		rec.output["launchctl print"] = tc.out
		if tc.err != nil {
			rec.fail["launchctl print"] = tc.err
		}

		mgr, agentDir := newTestLaunchd(t, rec)
		if err := os.WriteFile(filepath.Join(agentDir, Label+".plist"), []byte("<plist/>"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}

		status, err := mgr.Status(t.Context())
		if err != nil {
			t.Fatalf("%s: Status: %v", tc.name, err)
		}
		if status.State != tc.want {
			t.Errorf("%s: state = %q, want %q", tc.name, status.State, tc.want)
		}
		if status.Enabled != tc.enabled {
			t.Errorf("%s: enabled = %v, want %v", tc.name, status.Enabled, tc.enabled)
		}
	}
}

func TestLaunchdUninstallLeavesTheDataAlone(t *testing.T) {
	rec := newRecorder()
	mgr, agentDir := newTestLaunchd(t, rec)

	cfg := testConfig(t)
	database := filepath.Join(cfg.Home, "tumika.db")
	if err := os.WriteFile(database, []byte("sealed credentials"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := mgr.Install(t.Context(), cfg); err != nil {
		t.Fatalf("Install: %v", err)
	}

	if err := mgr.Uninstall(t.Context()); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, err := os.Stat(filepath.Join(agentDir, Label+".plist")); !errors.Is(err, os.ErrNotExist) {
		t.Error("the plist is still there")
	}
	if _, err := os.Stat(database); err != nil {
		t.Errorf("uninstall destroyed the database: %v", err)
	}
}

func TestLaunchdStartAndStopNeedAnInstalledService(t *testing.T) {
	rec := newRecorder()
	mgr, _ := newTestLaunchd(t, rec)

	if err := mgr.Start(t.Context()); !errors.Is(err, ErrNotInstalled) {
		t.Errorf("Start = %v, want ErrNotInstalled", err)
	}
	if err := mgr.Stop(t.Context()); !errors.Is(err, ErrNotInstalled) {
		t.Errorf("Stop = %v, want ErrNotInstalled", err)
	}
}

// Every one of these values is interpolated into a unit file that runs as root,
// or into a plist. A newline would append a directive of someone else's
// choosing.
func TestConfigValidation(t *testing.T) {
	valid := Config{Binary: "/usr/local/bin/tumika", Home: "/var/lib/tumika", User: "tumika"}
	if err := valid.Validate(); err != nil {
		t.Fatalf("a valid config was rejected: %v", err)
	}

	invalid := map[string]Config{
		"no binary":            {Home: "/var/lib/tumika"},
		"relative binary":      {Binary: "tumika", Home: "/var/lib/tumika"},
		"no home":              {Binary: "/usr/local/bin/tumika"},
		"relative home":        {Binary: "/usr/local/bin/tumika", Home: "tumika"},
		"relative sealed key":  {Binary: "/usr/local/bin/tumika", Home: "/var/lib/tumika", SealedKey: "master.cred"},
		"newline in binary":    {Binary: "/usr/local/bin/tumika\nExecStartPost=/bin/sh", Home: "/var/lib/tumika"},
		"newline in home":      {Binary: "/usr/local/bin/tumika", Home: "/var/lib/tumika\nUser=root"},
		"newline in user":      {Binary: "/usr/local/bin/tumika", Home: "/var/lib/tumika", User: "tumika\nUser=root"},
		"uppercase user":       {Binary: "/usr/local/bin/tumika", Home: "/var/lib/tumika", User: "Tumika"},
		"user with a space":    {Binary: "/usr/local/bin/tumika", Home: "/var/lib/tumika", User: "tumika daemon"},
		"user starting with -": {Binary: "/usr/local/bin/tumika", Home: "/var/lib/tumika", User: "-rf"},
	}

	for name, cfg := range invalid {
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// A refused config must not have written anything: a half-installed service is
// worse than a refused one, because the supervisor tries to run it and the
// failure lands in a journal rather than in the operator's terminal.
func TestAnInvalidConfigWritesNothing(t *testing.T) {
	rec := newRecorder()
	mgr, agentDir := newTestLaunchd(t, rec)

	err := mgr.Install(t.Context(), Config{Binary: "relative", Home: "/var/lib/tumika"})
	if err == nil {
		t.Fatal("an invalid config was accepted")
	}
	entries, readErr := os.ReadDir(agentDir)
	if readErr != nil {
		t.Fatalf("read the agent directory: %v", readErr)
	}
	if len(entries) != 0 {
		t.Errorf("a refused install wrote %d file(s)", len(entries))
	}
	if len(rec.calls) != 0 {
		t.Errorf("a refused install called launchctl: %v", rec.joined())
	}
}

func TestLaunchdStartOnAnInstalledService(t *testing.T) {
	rec := newRecorder()
	mgr, agentDir := newTestLaunchd(t, rec)
	if err := os.WriteFile(filepath.Join(agentDir, Label+".plist"), []byte("<plist/>"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := mgr.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !strings.Contains(strings.Join(rec.joined(), "\n"), "launchctl kickstart") {
		t.Errorf("Start did not kickstart the service: %v", rec.joined())
	}
}

func TestLaunchdReportsSupervisorFailures(t *testing.T) {
	for _, tc := range []struct {
		name string
		fail string
		call func(*Launchd) error
	}{
		{"bootstrap", "launchctl bootstrap", func(m *Launchd) error {
			return m.Install(context.Background(), Config{Binary: "/usr/local/bin/tumika", Home: "/tmp"})
		}},
		{"kickstart", "launchctl kickstart", func(m *Launchd) error { return m.Start(context.Background()) }},
		{"bootout", "launchctl bootout", func(m *Launchd) error { return m.Stop(context.Background()) }},
	} {
		rec := newRecorder()
		rec.fail[tc.fail] = errors.New("exit status 1")

		mgr, agentDir := newTestLaunchd(t, rec)
		if err := os.WriteFile(filepath.Join(agentDir, Label+".plist"), []byte("<plist/>"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}

		err := tc.call(mgr)
		if err == nil {
			t.Errorf("%s: a failure reported success", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), "simulated failure") {
			t.Errorf("%s: launchctl's own output was lost: %v", tc.name, err)
		}
	}
}

func TestLaunchdUninstallWithNothingInstalled(t *testing.T) {
	rec := newRecorder()
	mgr, _ := newTestLaunchd(t, rec)

	if err := mgr.Uninstall(t.Context()); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("= %v, want ErrNotInstalled", err)
	}
}

func TestLaunchdStatusWithNothingInstalled(t *testing.T) {
	rec := newRecorder()
	mgr, _ := newTestLaunchd(t, rec)

	status, err := mgr.Status(t.Context())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.State != StateNotInstalled {
		t.Errorf("state = %q, want %q", status.State, StateNotInstalled)
	}
	if status.Enabled {
		t.Error("a service that does not exist is reported as enabled")
	}
}

// The plist is byte-identical run to run: an install that rewrote it with the
// same content in a different order would look like a change to anything
// watching the file.
func TestThePlistIsStableAcrossRenders(t *testing.T) {
	cfg := Config{Binary: "/usr/local/bin/tumika", Home: "/var/lib/tumika", User: "tumika"}

	first, err := renderPlist(cfg)
	if err != nil {
		t.Fatalf("renderPlist: %v", err)
	}
	for range 5 {
		again, err := renderPlist(cfg)
		if err != nil {
			t.Fatalf("renderPlist: %v", err)
		}
		if string(again) != string(first) {
			t.Fatal("the plist is not stable across renders")
		}
	}
}

// Enabled means "will start at boot", and printStatus suppresses the "it will
// not come back after a restart" warning on the strength of it. Assuming true
// whenever a plist exists reported a disabled agent as enabled — suppressing
// exactly the warning that exists to catch it.
func TestLaunchdEnabledReflectsWhetherLaunchdWillStartIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
		want bool
	}{
		{"disabled", "{\n\t\"" + Label + "\" => true\n}\n", false},
		{"explicitly enabled", "{\n\t\"" + Label + "\" => false\n}\n", true},
		{"never disabled", "{\n\t\"com.example.other\" => true\n}\n", true},
	} {
		rec := newRecorder()
		rec.output["launchctl print-disabled"] = tc.out
		rec.output["launchctl print "] = "\tlast exit code = 0\n"

		mgr, agentDir := newTestLaunchd(t, rec)
		if err := os.WriteFile(filepath.Join(agentDir, Label+".plist"), []byte("<plist/>"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}

		status, err := mgr.Status(t.Context())
		if err != nil {
			t.Fatalf("%s: Status: %v", tc.name, err)
		}
		if status.Enabled != tc.want {
			t.Errorf("%s: enabled = %v, want %v", tc.name, status.Enabled, tc.want)
		}
	}
}
