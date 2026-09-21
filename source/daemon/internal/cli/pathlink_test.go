package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/tumika/tumika/source/daemon/internal/platform/paths"
	"github.com/tumika/tumika/source/daemon/internal/platform/servicemgr"
)

// realTempDir is a temporary directory with its symlinks resolved. macOS hands
// out /var/folders/..., which is a link to /private/var/folders/..., and every
// decision here compares resolved paths.
func realTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	return dir
}

// usePathStubs pins the platform, the PATH lookup and this process's own
// executable, so the linking decision is exercised without touching the real
// PATH of the machine running the tests.
func usePathStubs(t *testing.T, goos, found, self string) {
	t.Helper()

	originalGOOS, originalLook, originalSelf := installGOOS, lookPath, selfExecutable
	installGOOS = goos
	lookPath = func(string) (string, error) {
		if found == "" {
			return "", errors.New("executable file not found in $PATH")
		}
		return found, nil
	}
	selfExecutable = func() (string, error) { return self, nil }
	t.Cleanup(func() {
		installGOOS, lookPath, selfExecutable = originalGOOS, originalLook, originalSelf
	})
}

// managedBinary stands in for the daemon-owned copy install has just staged.
func managedBinary(t *testing.T, home string) (paths.Paths, string) {
	t.Helper()

	p, err := paths.Resolve(home)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if err := os.MkdirAll(p.Bin, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	managed := filepath.Join(p.Bin, "tumika")
	if err := os.WriteFile(managed, []byte("managed binary"), 0o755); err != nil { //nolint:gosec // a test fixture standing in for an executable
		t.Fatalf("write: %v", err)
	}
	return p, managed
}

// writeExecutable puts a stand-in binary at path.
func writeExecutable(t *testing.T, path string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte("a copy on PATH"), 0o755); err != nil { //nolint:gosec // a test fixture standing in for an executable
		t.Fatalf("write: %v", err)
	}
	return path
}

// linkOutput runs the linking step against a throwaway command and returns what
// it printed to stdout and stderr.
func linkOutput(t *testing.T, p paths.Paths, managed string) (string, string) {
	t.Helper()

	var out, errOut strings.Builder
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)

	linkPATHBinary(cmd, p, managed)
	return out.String(), errOut.String()
}

// assertNoStagedLinks checks that the staging name is not left behind, whatever
// the outcome was.
func assertNoStagedLinks(t *testing.T, dir string) {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the directory: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".tumika.link.") {
			t.Errorf("a staged link was left behind: %s", entry.Name())
		}
	}
}

// An update replaces the managed copy and nothing else, so a plain file on PATH
// would keep serving the version it was installed at forever.
func TestInstallLinksThePATHCopyToTheManagedBinary(t *testing.T) {
	p, managed := managedBinary(t, realTempDir(t))
	pathDir := realTempDir(t)
	found := writeExecutable(t, filepath.Join(pathDir, "tumika"))

	usePathStubs(t, "darwin", found, found)

	out, errOut := linkOutput(t, p, managed)

	info, err := os.Lstat(found)
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s is still a plain file; an update will never reach it", found)
	}
	dest, err := os.Readlink(found)
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if dest != managed {
		t.Errorf("link = %q, want %q", dest, managed)
	}
	// Staged and renamed: the destination is the binary this process is
	// executing, and unlinking it first leaves a window with no `tumika` at all.
	assertNoStagedLinks(t, pathDir)
	if !strings.Contains(out, found) {
		t.Errorf("the link was not reported:\n%s", out)
	}
	if errOut != "" {
		t.Errorf("stderr = %q, want nothing", errOut)
	}
}

// Running the managed copy itself must not replace it with a link to itself —
// that leaves nothing to execute.
func TestPathLinkLeavesTheManagedBinaryAlone(t *testing.T) {
	p, managed := managedBinary(t, realTempDir(t))
	usePathStubs(t, "darwin", managed, managed)

	out, _ := linkOutput(t, p, managed)

	info, err := os.Lstat(managed)
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if !info.Mode().IsRegular() {
		t.Fatal("the managed binary was replaced by a link")
	}
	if !strings.Contains(out, "already resolves") {
		t.Errorf("the no-op was not reported:\n%s", out)
	}
}

// A PATH entry that is already the link install would make is left untouched,
// and so is the second run of an install that made it.
func TestPathLinkIsIdempotent(t *testing.T) {
	p, managed := managedBinary(t, realTempDir(t))
	pathDir := realTempDir(t)
	found := writeExecutable(t, filepath.Join(pathDir, "tumika"))

	usePathStubs(t, "darwin", found, found)

	if _, errOut := linkOutput(t, p, managed); errOut != "" {
		t.Fatalf("the first run failed: %s", errOut)
	}
	out, errOut := linkOutput(t, p, managed)

	dest, err := os.Readlink(found)
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if dest != managed {
		t.Errorf("link = %q, want it unchanged at %q", dest, managed)
	}
	assertNoStagedLinks(t, pathDir)
	if !strings.Contains(out, "already resolves") {
		t.Errorf("the second run did not report a no-op:\n%s", out)
	}
	if errOut != "" {
		t.Errorf("stderr = %q, want nothing", errOut)
	}
}

// A binary run from a download directory or from `go run` is not the copy on
// PATH, and install is not licensed to rewrite an arbitrary path just because
// it carries the right name.
func TestPathLinkLeavesAnUnrelatedPATHCopyAlone(t *testing.T) {
	p, managed := managedBinary(t, realTempDir(t))
	pathDir := realTempDir(t)
	found := writeExecutable(t, filepath.Join(pathDir, "tumika"))
	elsewhere := writeExecutable(t, filepath.Join(realTempDir(t), "tumika"))

	usePathStubs(t, "darwin", found, elsewhere)

	out, _ := linkOutput(t, p, managed)

	info, err := os.Lstat(found)
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if !info.Mode().IsRegular() {
		t.Error("a copy on PATH that was not the binary being run was rewritten")
	}
	if !strings.Contains(out, "unmanaged") {
		t.Errorf("the operator was not told that the PATH copy keeps its own version:\n%s", out)
	}
}

// Linux links nothing: the managed directory is 0700 and owned by the service
// account, so a link into it from a PATH directory resolves to a binary the
// operator cannot execute.
func TestPathLinkDoesNothingOnLinux(t *testing.T) {
	p, managed := managedBinary(t, realTempDir(t))
	found := writeExecutable(t, filepath.Join(realTempDir(t), "tumika"))

	usePathStubs(t, "linux", found, found)

	out, _ := linkOutput(t, p, managed)

	info, err := os.Lstat(found)
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if !info.Mode().IsRegular() {
		t.Error("a symlink was made on Linux")
	}
	if !strings.Contains(out, p.Bin) {
		t.Errorf("the operator was not told where the managed binary lives:\n%s", out)
	}
}

// A PATH directory the operator cannot write is a message, not a reason to
// leave the machine without a service.
func TestInstallReportsAnUnwritablePATHDirectoryAndStillSucceeds(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the permission bits this relies on")
	}
	useTestKeyCustody(t)

	home := realTempDir(t)
	pathDir := realTempDir(t)
	found := writeExecutable(t, filepath.Join(pathDir, "tumika"))
	if err := os.Chmod(pathDir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(pathDir, 0o700) })

	usePathStubs(t, "darwin", found, found)
	mgr := &fakeManager{status: servicemgr.Status{Manager: "launchd", State: servicemgr.StateRunning}}
	useFakeManager(t, mgr)

	_, errOut, err := run(t, "--home", home, "install")
	if err != nil {
		t.Fatalf("an unlinkable PATH copy failed the install: %v", err)
	}
	if !strings.Contains(errOut, found) || !strings.Contains(errOut, "ln -sfn") {
		t.Errorf("stderr does not report the failure and how to fix it:\n%s", errOut)
	}
	info, statErr := os.Lstat(found)
	if statErr != nil {
		t.Fatalf("lstat: %v", statErr)
	}
	if !info.Mode().IsRegular() {
		t.Error("the copy on PATH was replaced despite the reported failure")
	}
}

// An explicit --binary names a path the operator manages themselves, so
// nothing on PATH is repointed at a binary tumika does not update.
func TestInstallWithAnExplicitBinaryLinksNothing(t *testing.T) {
	useTestKeyCustody(t)

	home := realTempDir(t)
	found := writeExecutable(t, filepath.Join(realTempDir(t), "tumika"))
	usePathStubs(t, "darwin", found, found)

	mgr := &fakeManager{status: servicemgr.Status{Manager: "launchd", State: servicemgr.StateRunning}}
	useFakeManager(t, mgr)

	if _, _, err := run(t, "--home", home, "install", "--binary", "/opt/tumika/bin/tumika"); err != nil {
		t.Fatalf("install: %v", err)
	}
	info, err := os.Lstat(found)
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if !info.Mode().IsRegular() {
		t.Error("a copy on PATH was linked to a binary the operator manages themselves")
	}
}
