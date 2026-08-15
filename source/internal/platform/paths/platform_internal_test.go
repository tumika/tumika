package paths

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Container detection must not answer for an unsupported platform. With the
// checks the other way round, a /.dockerenv marker made ErrUnsupportedPlatform
// unreachable — so tumika would build its whole state tree on a platform it does
// not support, in the environment where a wrong answer is hardest to notice.
func TestPlatformIsCheckedBeforeContainerDetection(t *testing.T) {
	tests := []struct {
		goos        string
		inContainer bool
		want        string
		wantErr     bool
	}{
		{goos: "linux", inContainer: true, want: "/var/lib/tumika"},
		{goos: "darwin", inContainer: true, want: "/var/lib/tumika"},
		{goos: "windows", inContainer: true, wantErr: true},
		{goos: "windows", inContainer: false, wantErr: true},
		{goos: "plan9", inContainer: true, wantErr: true},
	}

	for _, tc := range tests {
		name := tc.goos
		if tc.inContainer {
			name += "/container"
		}
		t.Run(name, func(t *testing.T) {
			got, err := platformHome(tc.goos, tc.inContainer)

			if tc.wantErr {
				if !errors.Is(err, ErrUnsupportedPlatform) {
					t.Fatalf("platformHome(%q, %v) = %q, %v; want ErrUnsupportedPlatform",
						tc.goos, tc.inContainer, got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("platformHome(%q, %v): %v", tc.goos, tc.inContainer, err)
			}
			if got != tc.want {
				t.Errorf("platformHome(%q, %v) = %q, want %q", tc.goos, tc.inContainer, got, tc.want)
			}
		})
	}
}

func TestPlatformHomeOutsideAContainer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	t.Run("linux honours XDG_STATE_HOME", func(t *testing.T) {
		t.Setenv("XDG_STATE_HOME", "/xdg/state")

		got, err := platformHome("linux", false)
		if err != nil {
			t.Fatalf("platformHome: %v", err)
		}
		if got != "/xdg/state/tumika" {
			t.Errorf("= %q, want /xdg/state/tumika", got)
		}
	})

	t.Run("linux falls back to the XDG default", func(t *testing.T) {
		t.Setenv("XDG_STATE_HOME", "")

		got, err := platformHome("linux", false)
		if err != nil {
			t.Fatalf("platformHome: %v", err)
		}
		if want := filepath.Join(home, ".local", "state", "tumika"); got != want {
			t.Errorf("= %q, want %q", got, want)
		}
	})

	t.Run("darwin uses Application Support", func(t *testing.T) {
		got, err := platformHome("darwin", false)
		if err != nil {
			t.Fatalf("platformHome: %v", err)
		}
		if want := filepath.Join(home, "Library", "Application Support", "tumika"); got != want {
			t.Errorf("= %q, want %q", got, want)
		}
	})
}

// With no HOME there is nowhere to put the state tree, and guessing would be
// worse than saying so — `tumika version` tolerates this, which is why path
// resolution is lazy.
func TestPlatformHomeWithoutAHomeDirectory(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_STATE_HOME", "")

	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			if _, err := platformHome(goos, false); err == nil {
				t.Error("expected an error when the home directory cannot be located")
			}
		})
	}
}

// MkdirAll must report a path it cannot create rather than proceeding as though
// the tree exists.
func TestMkdirAllReportsAnUncreatableTree(t *testing.T) {
	blocked := filepath.Join(t.TempDir(), "in-the-way")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	p, err := Resolve(blocked)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if err := p.MkdirAll(); err == nil {
		t.Error("MkdirAll succeeded with a file where the home directory should be")
	}
}
