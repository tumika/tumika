package paths

import (
	"errors"
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
