package servicemgr

import (
	"context"
	"errors"
	"os/user"
	"runtime"
	"strconv"
	"testing"
)

// New must return the platform's manager, and a clear refusal elsewhere rather
// than a nil that panics three layers down.
func TestNewReturnsTheManagerForThisPlatform(t *testing.T) {
	mgr, err := New()

	switch runtime.GOOS {
	case "darwin", "linux":
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if mgr == nil {
			t.Fatal("New returned no manager and no error")
		}
	default:
		if !errors.Is(err, ErrUnsupportedPlatform) {
			t.Fatalf("= %v, want ErrUnsupportedPlatform", err)
		}
	}
}

// resolveUser is what turns a name into the ids the ownership handover needs.
// Looked up rather than assumed, so an NSS-backed directory answers for itself.
func TestResolveUserFindsARealAccount(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Skipf("no current user: %v", err)
	}

	mgr, err := NewSystemd()
	if err != nil {
		t.Fatalf("NewSystemd: %v", err)
	}

	got, err := mgr.resolveUser(me.Username)
	if err != nil {
		t.Fatalf("resolveUser: %v", err)
	}
	wantUID, _ := strconv.Atoi(me.Uid)
	if got.uid != wantUID {
		t.Errorf("uid = %d, want %d", got.uid, wantUID)
	}
}

// An account that does not exist is an error, not a zero uid — chowning the
// whole data tree to uid 0 would hand it to root and leave the service unable
// to read any of it.
func TestResolveUserRefusesAnAccountThatDoesNotExist(t *testing.T) {
	mgr, err := NewSystemd()
	if err != nil {
		t.Fatalf("NewSystemd: %v", err)
	}

	got, err := mgr.resolveUser("definitely-not-an-account-on-this-box")
	if err == nil {
		t.Fatalf("a missing account resolved to %+v", got)
	}
	if got.uid != 0 || got.gid != 0 {
		t.Errorf("a failed lookup returned ids: %+v", got)
	}
}

// execRunner returns the command's own output, because "exit status 1" is not
// something an operator can act on.
func TestExecRunnerReturnsCombinedOutput(t *testing.T) {
	out, err := execRunner(context.Background(), "sh", "-c", "echo to-stdout; echo to-stderr >&2")
	if err != nil {
		t.Fatalf("execRunner: %v", err)
	}
	for _, want := range []string{"to-stdout", "to-stderr"} {
		if !contains(string(out), want) {
			t.Errorf("output is missing %q: %s", want, out)
		}
	}
}

func TestExecRunnerReportsAFailureWithItsOutput(t *testing.T) {
	out, err := execRunner(context.Background(), "sh", "-c", "echo why-it-failed >&2; exit 3")
	if err == nil {
		t.Fatal("a failing command reported success")
	}
	if !contains(string(out), "why-it-failed") {
		t.Errorf("the command's explanation was lost: %s", out)
	}
}

// A failure with no output still has to name what was being attempted.
func TestCommandErrorWithoutOutput(t *testing.T) {
	err := commandError("systemctl start tumika.service", nil, errors.New("exit status 1"))
	if !contains(err.Error(), "systemctl start") {
		t.Errorf("the error does not say what failed: %v", err)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	}()
}
