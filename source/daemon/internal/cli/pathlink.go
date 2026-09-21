package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/tumika/tumika/source/daemon/internal/platform/paths"
)

// Indirections for finding the tumika a shell would run. Variables so the
// linking decision can be exercised on any platform, without touching the real
// PATH or depending on where the test binary happens to live.
var (
	lookPath       = exec.LookPath
	selfExecutable = os.Executable
)

// linkPATHBinary points the tumika on the operator's PATH at the managed copy.
//
// An update replaces the daemon-owned binary under the home directory and
// nothing else, so a second, independent copy on PATH keeps serving the version
// it was installed at: the daemon moves on and `tumika version` reports the old
// build forever. A symlink makes the two the same file.
//
// Only the copy the operator actually ran is touched, and only when it is a
// plain file. A binary invoked from a download directory, from `go run`, or
// through a link somebody else manages is left exactly as it is — install is
// not licensed to rewrite an arbitrary path just because it carries the right
// name.
//
// It never fails the install. The link is a convenience, and a /usr/local/bin
// the operator cannot write is a message, not a reason to leave a machine
// without a service.
func linkPATHBinary(cmd *cobra.Command, p paths.Paths, managed string) {
	// macOS only. On Linux the managed directory is 0700 and owned by the
	// service account, so a link into it from a PATH directory resolves to a
	// binary the operator cannot execute; and the documented install there is
	// `sudo <absolute path> install`, which is never the tumika on PATH.
	if installGOOS != "darwin" {
		printf(cmd, "  path    a tumika on PATH is left as it is; %s is readable only by the service account\n", p.Bin)
		return
	}

	found, err := lookPath("tumika")
	if err != nil {
		printf(cmd, "  path    no tumika on PATH; add %s to PATH so updates are picked up\n", p.Bin)
		return
	}

	self, err := selfExecutable()
	if err != nil {
		printf(cmd, "  path    %s is left as it is: this process cannot locate its own binary (%v)\n", found, err)
		return
	}
	// Both sides are resolved before they are compared, so a PATH entry that is
	// already a link to the binary being run counts as that binary rather than
	// as a stranger.
	invoked, err := filepath.EvalSymlinks(self)
	if err != nil {
		printf(cmd, "  path    %s is left as it is: %v\n", found, err)
		return
	}
	target, err := filepath.EvalSymlinks(found)
	if err != nil {
		printf(cmd, "  path    %s is left as it is: %v\n", found, err)
		return
	}
	if target != invoked {
		printf(cmd, "  path    %s is not the binary you ran; it stays unmanaged and keeps its own version\n", found)
		return
	}

	managedReal, err := filepath.EvalSymlinks(managed)
	if err != nil {
		printf(cmd, "  path    %s is left as it is: %v\n", found, err)
		return
	}
	// The managed copy under p.Bin is what an update rewrites; a link there
	// would point at itself, and there would be nothing left to execute.
	if target == managedReal {
		printf(cmd, "  path    %s already resolves to the managed binary\n", found)
		return
	}

	info, err := os.Lstat(found)
	if err != nil {
		printf(cmd, "  path    %s is left as it is: %v\n", found, err)
		return
	}
	if !info.Mode().IsRegular() {
		printf(cmd, "  path    %s is a link somebody else manages; left as it is\n", found)
		return
	}

	if err := replaceWithLink(found, managed); err != nil {
		warnPATHLink(cmd, found, managed, err)
		return
	}
	printf(cmd, "  path    %s -> %s\n", found, managed)
}

// replaceWithLink makes found a symlink to managed, atomically.
//
// Staged beside the destination and renamed over it, because found is the
// binary this very process is executing: unlinking it first leaves a window in
// which `tumika` is not a command at all, and a failure inside that window
// leaves the machine without one.
func replaceWithLink(found, managed string) error {
	tmp := filepath.Join(filepath.Dir(found), fmt.Sprintf(".tumika.link.%d", os.Getpid()))
	if err := os.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clear %s: %w", tmp, err)
	}
	if err := os.Symlink(managed, tmp); err != nil {
		return fmt.Errorf("stage a link in %s: %w", filepath.Dir(found), err)
	}
	if err := os.Rename(tmp, found); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace %s: %w", found, err)
	}
	return nil
}

// warnPATHLink reports a PATH copy that could not be linked, and says what it
// costs. It goes to stderr and carries the command that fixes it by hand: an
// unwritable /usr/local/bin is the ordinary case, and the install itself
// succeeded.
func warnPATHLink(cmd *cobra.Command, found, managed string, err error) {
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
		"Warning: %s could not be pointed at the managed binary: %v\n", found, err)
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
		"That copy keeps its own version when tumika updates itself. To link it by hand:\n  sudo ln -sfn %s %s\n",
		managed, found)
}
