package servicemgr

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// recorder captures the supervisor calls a manager makes, so the SEQUENCE is
// assertable without a supervisor. Ordering is the interesting part: enabling
// before daemon-reload, or writing a unit before creating the account it names,
// both produce an install that reports success and a service that never runs.
type recorder struct {
	calls  [][]string
	fail   map[string]error
	output map[string]string
}

func newRecorder() *recorder {
	return &recorder{fail: map[string]error{}, output: map[string]string{}}
}

func (r *recorder) run(_ context.Context, name string, args ...string) ([]byte, error) {
	call := append([]string{name}, args...)
	r.calls = append(r.calls, call)

	// Output and error are resolved INDEPENDENTLY, because the real systemctl
	// returns both at once: `is-active` on a stopped service prints "inactive"
	// and exits 3. A recorder that returned one or the other could not express
	// the case these tests exist to check.
	key := strings.Join(call, " ")

	var out []byte
	for prefix, text := range r.output {
		if strings.HasPrefix(key, prefix) {
			out = []byte(text)
		}
	}
	for prefix, err := range r.fail {
		if strings.HasPrefix(key, prefix) {
			if out == nil {
				out = []byte("simulated failure")
			}
			return out, err
		}
	}
	return out, nil
}

func (r *recorder) joined() []string {
	out := make([]string, 0, len(r.calls))
	for _, call := range r.calls {
		out = append(out, strings.Join(call, " "))
	}
	return out
}

func (r *recorder) indexOf(t *testing.T, prefix string) int {
	t.Helper()
	for i, call := range r.joined() {
		if strings.HasPrefix(call, prefix) {
			return i
		}
	}
	t.Fatalf("no call starting with %q; got %v", prefix, r.joined())
	return -1
}

func newTestSystemd(t *testing.T, rec *recorder, opts ...SystemdOption) (*Systemd, string) {
	t.Helper()

	unitDir := t.TempDir()
	base := []SystemdOption{
		withSystemdUnitDir(unitDir),
		withSystemdRunner(rec.run),
		withSystemdEUID(0),
		withSystemdUserLookup(func(string) bool { return true }),
		// The real uid, so the ownership walk is a no-op rather than an EPERM:
		// the test is about ORDER and content, and chowning to root as a normal
		// user would fail for reasons that say nothing about the code.
		withSystemdIDLookup(func(string) (account, error) {
			return account{uid: os.Getuid(), gid: os.Getgid()}, nil
		}),
		withSystemdChown(func(string, int, int) error { return nil }),
	}
	mgr, err := NewSystemd(append(base, opts...)...)
	if err != nil {
		t.Fatalf("NewSystemd: %v", err)
	}
	return mgr, unitDir
}

func testConfig(t *testing.T) Config {
	t.Helper()
	return Config{Binary: "/var/lib/tumika/bin/tumika", Home: t.TempDir(), User: "tumika"}
}

func TestInstallWritesTheUnitAndEnablesIt(t *testing.T) {
	rec := newRecorder()
	mgr, unitDir := newTestSystemd(t, rec)

	if err := mgr.Install(t.Context(), testConfig(t)); err != nil {
		t.Fatalf("Install: %v", err)
	}

	unit, err := os.ReadFile(filepath.Join(unitDir, UnitName))
	if err != nil {
		t.Fatalf("no unit was written: %v", err)
	}
	text := string(unit)

	for _, want := range []string{
		"User=tumika",
		"ExecStart=/var/lib/tumika/bin/tumika serve",
		// always, not on-failure: an update exits ZERO to be relaunched on the
		// new binary, and on-failure would leave a stopped daemon and a
		// successful-looking update.
		"Restart=always",
		"WantedBy=multi-user.target",
		// network-online, not network: a bare network.target is satisfied long
		// before a route exists, which on a Pi booting cold is a failed first
		// start.
		"After=network-online.target",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the unit is missing %q:\n%s", want, text)
		}
	}

	// daemon-reload must come BEFORE enable, or systemd enables the unit it had
	// cached rather than the one just written.
	if rec.indexOf(t, "systemctl daemon-reload") > rec.indexOf(t, "systemctl enable") {
		t.Errorf("enable ran before daemon-reload: %v", rec.joined())
	}
	calls := strings.Join(rec.joined(), "\n")
	if !strings.Contains(calls, "systemctl enable "+UnitName) {
		t.Errorf("the service was not enabled, so it will not survive a reboot: %v", rec.joined())
	}
	// restart, not `enable --now`. `start` is a no-op on an active unit, so the
	// documented upgrade path would leave the daemon on the old binary.
	if !strings.Contains(calls, "systemctl restart "+UnitName) {
		t.Errorf("the service was not restarted, so an upgrade would keep running the old binary: %v",
			rec.joined())
	}
}

// The account has to exist before the unit naming it is loaded, or systemd
// fails at 203/EXEC with Restart=always turning it into a loop.
func TestInstallCreatesTheServiceAccountFirst(t *testing.T) {
	rec := newRecorder()
	mgr, _ := newTestSystemd(t, rec, withSystemdUserLookup(func(string) bool { return false }))

	if err := mgr.Install(t.Context(), testConfig(t)); err != nil {
		t.Fatalf("Install: %v", err)
	}

	useradd := rec.indexOf(t, "useradd")
	if useradd > rec.indexOf(t, "systemctl enable") {
		t.Errorf("the account was created after the service was enabled: %v", rec.joined())
	}

	call := strings.Join(rec.calls[useradd], " ")
	for _, want := range []string{"--system", "--no-create-home", "nologin"} {
		if !strings.Contains(call, want) {
			t.Errorf("useradd is missing %q: %s", want, call)
		}
	}
}

// An existing account is left alone. Recreating one on every install would fail
// on a second run, and on an NSS-backed host would shadow a directory account
// with a local one.
func TestInstallLeavesAnExistingAccountAlone(t *testing.T) {
	rec := newRecorder()
	mgr, _ := newTestSystemd(t, rec, withSystemdUserLookup(func(string) bool { return true }))

	if err := mgr.Install(t.Context(), testConfig(t)); err != nil {
		t.Fatalf("Install: %v", err)
	}
	for _, call := range rec.joined() {
		if strings.HasPrefix(call, "useradd") {
			t.Errorf("an existing account was recreated: %s", call)
		}
	}
}

// THE bug the container found. The layout is created 0700 by root and the unit
// runs unprivileged, so without this the service cannot traverse into the
// directory holding its own binary: 203/EXEC, restart loop, and `is-active`
// reporting "activating" forever rather than "failed".
func TestInstallGivesTheHomeDirectoryToTheServiceAccount(t *testing.T) {
	rec := newRecorder()

	home := t.TempDir()
	nested := filepath.Join(home, "bin")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	binary := filepath.Join(nested, "tumika")
	if err := os.WriteFile(binary, []byte("binary"), 0o700); err != nil {
		t.Fatalf("write: %v", err)
	}

	// The calls are what the test is about: a test cannot give a file away to
	// another account without being root.
	given := map[string][2]int{}
	mgr, _ := newTestSystemd(t, rec,
		withSystemdIDLookup(func(string) (account, error) {
			return account{uid: 4242, gid: 4343}, nil
		}),
		withSystemdChown(func(path string, uid, gid int) error {
			given[path] = [2]int{uid, gid}
			return nil
		}))

	cfg := testConfig(t)
	cfg.Home = home
	if err := mgr.Install(t.Context(), cfg); err != nil {
		t.Fatalf("Install: %v", err)
	}

	// The NESTED binary matters most: that is the exact path systemd failed to
	// execute. Chowning only the top directory would leave the same 203/EXEC.
	for _, path := range []string{home, nested, binary} {
		ids, ok := given[path]
		if !ok {
			t.Errorf("%s was not given to the service account", path)
			continue
		}
		if ids != [2]int{4242, 4343} {
			t.Errorf("%s went to uid/gid %v, want the service account", path, ids)
		}
	}
}

// A failure to hand the tree over is FATAL, not a warning. Carrying on would
// write a unit for a service that cannot execute its own binary.
func TestInstallStopsWhenTheHomeCannotBeHandedOver(t *testing.T) {
	rec := newRecorder()
	mgr, unitDir := newTestSystemd(t, rec,
		withSystemdChown(func(string, int, int) error { return errors.New("operation not permitted") }))

	err := mgr.Install(t.Context(), testConfig(t))
	if err == nil {
		t.Fatal("install reported success despite failing to hand over the home")
	}
	if _, statErr := os.Stat(filepath.Join(unitDir, UnitName)); statErr == nil {
		t.Error("a unit was written for a service that cannot read its own data")
	}
}

// A missing home is created rather than refused: on a first install nothing
// exists yet, and a daemon that cannot write its own parent cannot create it.
func TestInstallCreatesAMissingHome(t *testing.T) {
	rec := newRecorder()
	mgr, _ := newTestSystemd(t, rec)

	cfg := testConfig(t)
	cfg.Home = filepath.Join(t.TempDir(), "does-not-exist-yet")

	if err := mgr.Install(t.Context(), cfg); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, err := os.Stat(cfg.Home); err != nil {
		t.Errorf("the home was not created: %v", err)
	}
}

// The whole point of LoadCredentialEncrypted: systemd decrypts the blob while
// still privileged and hands the plaintext to a service that could never read
// the host key itself.
func TestTheUnitDeclaresTheSealedKeyWhenThereIsOne(t *testing.T) {
	rec := newRecorder()
	mgr, unitDir := newTestSystemd(t, rec)

	cfg := testConfig(t)
	cfg.SealedKey = "/var/lib/tumika/master.cred"

	if err := mgr.Install(t.Context(), cfg); err != nil {
		t.Fatalf("Install: %v", err)
	}

	unit, err := os.ReadFile(filepath.Join(unitDir, UnitName))
	if err != nil {
		t.Fatalf("read the unit: %v", err)
	}
	want := "LoadCredentialEncrypted=tumika-master-key:/var/lib/tumika/master.cred"
	if !strings.Contains(string(unit), want) {
		t.Errorf("the unit does not hand the sealed key over:\n%s", unit)
	}
}

// And no directive at all when there is nothing to hand over — a unit naming a
// blob that does not exist fails to start.
func TestTheUnitOmitsTheCredentialWhenThereIsNoSealedKey(t *testing.T) {
	rec := newRecorder()
	mgr, unitDir := newTestSystemd(t, rec)

	if err := mgr.Install(t.Context(), testConfig(t)); err != nil {
		t.Fatalf("Install: %v", err)
	}

	unit, err := os.ReadFile(filepath.Join(unitDir, UnitName))
	if err != nil {
		t.Fatalf("read the unit: %v", err)
	}
	if strings.Contains(string(unit), "LoadCredentialEncrypted") {
		t.Errorf("the unit names a credential that does not exist:\n%s", unit)
	}
}

// Without root the install would half-happen: no account, and a unit systemd
// tries to run. Refused up front, and the message says the fix.
func TestInstallWithoutRoot(t *testing.T) {
	rec := newRecorder()
	mgr, unitDir := newTestSystemd(t, rec, withSystemdEUID(1000))

	err := mgr.Install(t.Context(), testConfig(t))
	if !errors.Is(err, ErrPrivilegesRequired) {
		t.Fatalf("= %v, want ErrPrivilegesRequired", err)
	}
	if !strings.Contains(err.Error(), "sudo") {
		t.Errorf("the error should name the fix, got: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(unitDir, UnitName)); statErr == nil {
		t.Error("a refused install still wrote a unit")
	}
	if len(rec.calls) != 0 {
		t.Errorf("a refused install still called the supervisor: %v", rec.joined())
	}
}

// systemctl exits non-zero for "inactive" and "disabled", which are ANSWERS.
// Keying on the exit code reports every stopped service as a failure.
func TestStatusReadsTheOutputNotTheExitCode(t *testing.T) {
	tests := []struct {
		isActive  string
		isEnabled string
		want      State
		enabled   bool
	}{
		{"active", "enabled", StateRunning, true},
		// NOT running. This is what a Restart=always crash loop reports
		// forever — it never reaches "failed" — so calling it running is the
		// exact lie that let a dead install print "state running".
		{"activating", "enabled", StateStarting, true},
		{"inactive", "disabled", StateStopped, false},
		{"failed", "enabled", StateFailed, true},
		{"something-new", "enabled", StateUnknown, true},
	}

	for _, tc := range tests {
		rec := newRecorder()
		rec.output["systemctl is-active"] = tc.isActive + "\n"
		rec.output["systemctl is-enabled"] = tc.isEnabled + "\n"
		// Both report failure, as the real systemctl does for these answers.
		rec.fail["systemctl is-active"] = errors.New("exit status 3")
		rec.fail["systemctl is-enabled"] = errors.New("exit status 1")

		mgr, unitDir := newTestSystemd(t, rec)
		if err := os.WriteFile(filepath.Join(unitDir, UnitName), []byte("[Unit]\n"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}

		status, err := mgr.Status(t.Context())
		if err != nil {
			t.Fatalf("Status(%s): %v", tc.isActive, err)
		}
		if status.State != tc.want {
			t.Errorf("is-active %q gave state %q, want %q", tc.isActive, status.State, tc.want)
		}
		if status.Enabled != tc.enabled {
			t.Errorf("is-enabled %q gave enabled=%v, want %v", tc.isEnabled, status.Enabled, tc.enabled)
		}
	}
}

// Nothing installed is a normal answer, not an error: `tumika status` on a fresh
// machine should say so rather than fail.
func TestStatusWithNothingInstalled(t *testing.T) {
	rec := newRecorder()
	mgr, _ := newTestSystemd(t, rec)

	status, err := mgr.Status(t.Context())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.State != StateNotInstalled {
		t.Errorf("state = %q, want %q", status.State, StateNotInstalled)
	}
	if len(rec.calls) != 0 {
		t.Errorf("the supervisor was asked about a service that does not exist: %v", rec.joined())
	}
}

// Uninstall removes the unit and NOT the data. Removing a service is a routine
// part of an upgrade, and making it destroy credentials would turn a reversible
// action into an irreversible one.
func TestUninstallLeavesTheDataAlone(t *testing.T) {
	rec := newRecorder()
	mgr, unitDir := newTestSystemd(t, rec)

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
	if _, err := os.Stat(filepath.Join(unitDir, UnitName)); !errors.Is(err, os.ErrNotExist) {
		t.Error("the unit is still there")
	}
	if _, err := os.Stat(database); err != nil {
		t.Errorf("uninstall destroyed the database: %v", err)
	}
}

// An already-stopped service must not block its own removal — that is exactly
// the half state uninstall exists to clear.
func TestUninstallSucceedsWhenTheServiceIsAlreadyStopped(t *testing.T) {
	rec := newRecorder()
	rec.fail["systemctl disable"] = errors.New("exit status 1")

	mgr, unitDir := newTestSystemd(t, rec)
	if err := os.WriteFile(filepath.Join(unitDir, UnitName), []byte("[Unit]\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := mgr.Uninstall(t.Context()); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, err := os.Stat(filepath.Join(unitDir, UnitName)); !errors.Is(err, os.ErrNotExist) {
		t.Error("the unit survived an uninstall because the service was already stopped")
	}
}

func TestStartAndStopNeedAnInstalledService(t *testing.T) {
	rec := newRecorder()
	mgr, _ := newTestSystemd(t, rec)

	if err := mgr.Start(t.Context()); !errors.Is(err, ErrNotInstalled) {
		t.Errorf("Start = %v, want ErrNotInstalled", err)
	}
	if err := mgr.Stop(t.Context()); !errors.Is(err, ErrNotInstalled) {
		t.Errorf("Stop = %v, want ErrNotInstalled", err)
	}
	if len(rec.calls) != 0 {
		t.Errorf("systemctl was called for a service that does not exist: %v", rec.joined())
	}
}

// A failed supervisor call must carry the supervisor's own words. "exit status
// 1" is not something an operator can act on.
func TestASupervisorFailureCarriesItsOwnOutput(t *testing.T) {
	rec := newRecorder()
	rec.fail["systemctl enable"] = errors.New("exit status 1")
	mgr, _ := newTestSystemd(t, rec)

	err := mgr.Install(t.Context(), testConfig(t))
	if err == nil {
		t.Fatal("a failed enable reported success")
	}
	if !strings.Contains(err.Error(), "simulated failure") {
		t.Errorf("the error lost systemd's own output: %v", err)
	}
}

// The happy paths. Start and Stop on an installed service must reach the
// supervisor and report success.
func TestStartAndStopOnAnInstalledService(t *testing.T) {
	for _, tc := range []struct {
		command string
		call    func(*Systemd) error
	}{
		{"start", func(m *Systemd) error { return m.Start(context.Background()) }},
		{"stop", func(m *Systemd) error { return m.Stop(context.Background()) }},
	} {
		rec := newRecorder()
		mgr, unitDir := newTestSystemd(t, rec)
		if err := os.WriteFile(filepath.Join(unitDir, UnitName), []byte("[Unit]\n"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}

		if err := tc.call(mgr); err != nil {
			t.Fatalf("%s: %v", tc.command, err)
		}
		if !strings.Contains(strings.Join(rec.joined(), "\n"), "systemctl "+tc.command+" "+UnitName) {
			t.Errorf("%s did not reach systemctl: %v", tc.command, rec.joined())
		}
	}
}

// A supervisor that refuses to start or stop must be reported, with its own
// explanation.
func TestStartAndStopReportASupervisorFailure(t *testing.T) {
	for _, command := range []string{"start", "stop"} {
		rec := newRecorder()
		rec.fail["systemctl "+command] = errors.New("exit status 1")

		mgr, unitDir := newTestSystemd(t, rec)
		if err := os.WriteFile(filepath.Join(unitDir, UnitName), []byte("[Unit]\n"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}

		var err error
		if command == "start" {
			err = mgr.Start(t.Context())
		} else {
			err = mgr.Stop(t.Context())
		}
		if err == nil {
			t.Fatalf("a failed %s reported success", command)
		}
		if !strings.Contains(err.Error(), "simulated failure") {
			t.Errorf("%s lost systemd's own output: %v", command, err)
		}
	}
}

// Uninstalling something that is not installed is an error worth naming, not a
// silent success that leaves an operator wondering what happened.
func TestUninstallWithNothingInstalled(t *testing.T) {
	rec := newRecorder()
	mgr, _ := newTestSystemd(t, rec)

	if err := mgr.Uninstall(t.Context()); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("= %v, want ErrNotInstalled", err)
	}
	if len(rec.calls) != 0 {
		t.Errorf("systemctl was called for a service that does not exist: %v", rec.joined())
	}
}

// Removing a unit needs root just as writing one does.
func TestUninstallWithoutRoot(t *testing.T) {
	rec := newRecorder()
	mgr, unitDir := newTestSystemd(t, rec, withSystemdEUID(1000))
	if err := os.WriteFile(filepath.Join(unitDir, UnitName), []byte("[Unit]\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	err := mgr.Uninstall(t.Context())
	if !errors.Is(err, ErrPrivilegesRequired) {
		t.Fatalf("= %v, want ErrPrivilegesRequired", err)
	}
	if _, statErr := os.Stat(filepath.Join(unitDir, UnitName)); statErr != nil {
		t.Error("a refused uninstall removed the unit anyway")
	}
}

// A failure to create the account stops the install, rather than leaving a unit
// naming a user that does not exist.
func TestInstallStopsWhenTheAccountCannotBeCreated(t *testing.T) {
	rec := newRecorder()
	rec.fail["useradd"] = errors.New("exit status 1")

	mgr, unitDir := newTestSystemd(t, rec, withSystemdUserLookup(func(string) bool { return false }))

	if err := mgr.Install(t.Context(), testConfig(t)); err == nil {
		t.Fatal("install reported success despite failing to create the account")
	}
	if _, err := os.Stat(filepath.Join(unitDir, UnitName)); err == nil {
		t.Error("a unit was written naming an account that does not exist")
	}
}

// The account lookup failing is likewise fatal: chowning to a zero uid would
// hand the data to root and leave the service unable to read it.
func TestInstallStopsWhenTheAccountCannotBeResolved(t *testing.T) {
	rec := newRecorder()
	mgr, unitDir := newTestSystemd(t, rec, withSystemdIDLookup(func(string) (account, error) {
		return account{}, errors.New("no such user")
	}))

	if err := mgr.Install(t.Context(), testConfig(t)); err == nil {
		t.Fatal("install reported success despite an unresolvable account")
	}
	if _, err := os.Stat(filepath.Join(unitDir, UnitName)); err == nil {
		t.Error("a unit was written for an account that could not be resolved")
	}
}

// The default account is used when none is asked for, so the unit and the
// account it names cannot disagree.
func TestInstallDefaultsTheServiceAccount(t *testing.T) {
	rec := newRecorder()
	mgr, unitDir := newTestSystemd(t, rec)

	cfg := testConfig(t)
	cfg.User = ""
	if err := mgr.Install(t.Context(), cfg); err != nil {
		t.Fatalf("Install: %v", err)
	}

	unit, err := os.ReadFile(filepath.Join(unitDir, UnitName))
	if err != nil {
		t.Fatalf("read the unit: %v", err)
	}
	if !strings.Contains(string(unit), "User="+DefaultUser) {
		t.Errorf("the unit does not name the default account:\n%s", unit)
	}
}

// A crash loop reports "activating" forever. The restart counter is what tells
// it apart from a genuine first start, and getting this wrong is what let a
// service that had never executed report itself as running.
func TestACrashLoopIsNotReportedAsStarting(t *testing.T) {
	rec := newRecorder()
	rec.output["systemctl is-active"] = "activating\n"
	rec.output["systemctl is-enabled"] = "enabled\n"
	rec.output["systemctl show -p NRestarts"] = "7\n"

	mgr, unitDir := newTestSystemd(t, rec)
	if err := os.WriteFile(filepath.Join(unitDir, UnitName), []byte("[Unit]\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	status, err := mgr.Status(t.Context())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.State != StateFailed {
		t.Errorf("state = %q, want %q for a unit that has restarted 7 times", status.State, StateFailed)
	}
	if status.State == StateRunning {
		t.Error("a service that has never executed is reported as running")
	}
}

// A genuine first start has not restarted, and must not be called failed.
func TestAFirstStartIsStartingNotFailed(t *testing.T) {
	rec := newRecorder()
	rec.output["systemctl is-active"] = "activating\n"
	rec.output["systemctl is-enabled"] = "enabled\n"
	rec.output["systemctl show -p NRestarts"] = "0\n"

	mgr, unitDir := newTestSystemd(t, rec)
	if err := os.WriteFile(filepath.Join(unitDir, UnitName), []byte("[Unit]\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	status, err := mgr.Status(t.Context())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.State != StateStarting {
		t.Errorf("state = %q, want %q", status.State, StateStarting)
	}
}

// The chown is recursive and cannot be undone, so a system directory must be
// refused before anything is handed over.
func TestInstallRefusesToGiveAwayASystemDirectory(t *testing.T) {
	for _, home := range []string{"/", "/home", "/usr", "/var", "/var/lib", "/etc", "/root"} {
		rec := newRecorder()
		var chowned []string
		mgr, unitDir := newTestSystemd(t, rec, withSystemdChown(func(path string, _, _ int) error {
			chowned = append(chowned, path)
			return nil
		}))

		cfg := testConfig(t)
		cfg.Home = home

		if err := mgr.Install(t.Context(), cfg); err == nil {
			t.Errorf("%s was accepted as a home directory", home)
		}
		if len(chowned) != 0 {
			t.Errorf("%s: ownership was changed before the refusal: %v", home, chowned)
		}
		if _, err := os.Stat(filepath.Join(unitDir, UnitName)); err == nil {
			t.Errorf("%s: a unit was written", home)
		}
	}
}

// Lchown protects links INSIDE the tree; a symlinked root would have WalkDir
// following it and chowning whatever it points at.
func TestInstallRefusesASymlinkedHome(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "elsewhere")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(dir, "home")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	rec := newRecorder()
	var chowned []string
	mgr, _ := newTestSystemd(t, rec, withSystemdChown(func(path string, _, _ int) error {
		chowned = append(chowned, path)
		return nil
	}))

	cfg := testConfig(t)
	cfg.Home = link

	if err := mgr.Install(t.Context(), cfg); err == nil {
		t.Fatal("a symlinked home was accepted")
	}
	if len(chowned) != 0 {
		t.Errorf("ownership was changed outside tumika's tree: %v", chowned)
	}
}

// Prepare creates the account and nothing else, so the handover probe has a
// user to run as before any unit is written.
func TestPrepareCreatesOnlyTheAccount(t *testing.T) {
	rec := newRecorder()
	mgr, unitDir := newTestSystemd(t, rec, withSystemdUserLookup(func(string) bool { return false }))

	if err := mgr.Prepare(t.Context(), testConfig(t)); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if _, err := rec.joined(), error(nil); err != nil {
		t.Fatal(err)
	}
	if len(rec.calls) != 1 || rec.calls[0][0] != "useradd" {
		t.Errorf("Prepare did more than create the account: %v", rec.joined())
	}
	if _, err := os.Stat(filepath.Join(unitDir, UnitName)); err == nil {
		t.Error("Prepare wrote a unit")
	}
}

func TestPrepareNeedsRoot(t *testing.T) {
	rec := newRecorder()
	mgr, _ := newTestSystemd(t, rec, withSystemdEUID(1000))

	if err := mgr.Prepare(t.Context(), testConfig(t)); !errors.Is(err, ErrPrivilegesRequired) {
		t.Fatalf("= %v, want ErrPrivilegesRequired", err)
	}
}

// A stop that fails during an uninstall is not fatal — the unit is going away —
// but it must not be silent either: a daemon may survive with nothing left to
// stop it.
func TestAFailedStopDuringUninstallIsReported(t *testing.T) {
	rec := newRecorder()
	rec.fail["systemctl disable"] = errors.New("exit status 1")

	var warnings []string
	originalWarn := Warnf
	Warnf = func(format string, args ...any) {
		warnings = append(warnings, format)
	}
	t.Cleanup(func() { Warnf = originalWarn })

	mgr, unitDir := newTestSystemd(t, rec)
	if err := os.WriteFile(filepath.Join(unitDir, UnitName), []byte("[Unit]\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := mgr.Uninstall(t.Context()); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if len(warnings) == 0 {
		t.Error("a failed stop was swallowed silently")
	}
}

// systemd splits ExecStart= on whitespace and parses Environment= as
// space-separated KEY=VALUE, and `%` introduces a specifier. Each one produces
// an install that reports success and a service that does not work.
//
// validForUnitFile is called DIRECTLY rather than through Install. Going through
// Install passed for the wrong reason: an unwritable path like "/srv/tumika
// data" fails at MkdirAll long before anything inspects it, so the test stayed
// green with the validation removed entirely.
func TestTheUnitRefusesValuesItCannotCarryIntact(t *testing.T) {
	cases := map[string]Config{
		"space in the binary path": {Binary: "/opt/tumika suite/bin/tumika", Home: "/var/lib/tumika"},
		"space in the home":        {Binary: "/usr/local/bin/tumika", Home: "/srv/tumika data"},
		"tab in the home":          {Binary: "/usr/local/bin/tumika", Home: "/srv/tumika\tdata"},
		"specifier in the home":    {Binary: "/usr/local/bin/tumika", Home: "/srv/%h/tumika"},
		"specifier in the binary":  {Binary: "/usr/local/bin/%i/tumika", Home: "/var/lib/tumika"},
		"space in the sealed key":  {Binary: "/usr/local/bin/tumika", Home: "/var/lib/tumika", SealedKey: "/var/lib/my tumika/master.cred"},
	}

	for name, cfg := range cases {
		if err := validForUnitFile(cfg); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}

	ok := Config{Binary: "/usr/local/bin/tumika", Home: "/var/lib/tumika", SealedKey: "/var/lib/tumika/master.cred"}
	if err := validForUnitFile(ok); err != nil {
		t.Errorf("an ordinary config was refused: %v", err)
	}
}

// And Install applies it, before writing anything.
func TestInstallAppliesTheUnitFileRules(t *testing.T) {
	rec := newRecorder()
	mgr, unitDir := newTestSystemd(t, rec)

	cfg := testConfig(t)
	cfg.Binary = "/opt/tumika suite/bin/tumika"

	if err := mgr.Install(t.Context(), cfg); err == nil {
		t.Fatal("a binary path with a space was accepted")
	}
	if _, err := os.Stat(filepath.Join(unitDir, UnitName)); err == nil {
		t.Error("a unit was written anyway")
	}
	if len(rec.calls) != 0 {
		t.Errorf("the supervisor was called anyway: %v", rec.joined())
	}
}

// And the constraint is systemd's alone. macOS's own default home is
// ~/Library/Application Support/tumika — a path with a space that a plist
// handles without complaint — so applying this rule to both would break every
// Mac install to protect Linux.
func TestAPathWithASpaceIsFineOnMacOS(t *testing.T) {
	rec := newRecorder()
	mgr, _ := newTestLaunchd(t, rec)

	home := filepath.Join(t.TempDir(), "Application Support", "tumika")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	cfg := Config{Binary: "/usr/local/bin/tumika", Home: home}
	if err := mgr.Install(t.Context(), cfg); err != nil {
		t.Fatalf("a normal macOS path was refused: %v", err)
	}
}
