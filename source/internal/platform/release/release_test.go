package release

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGitHub serves the two things the updater fetches: the /releases/latest
// redirect, and a release's assets.
type fakeGitHub struct {
	latest string
	// binary is the published asset body, keyed by version.
	binary map[string]string
	// checksum overrides the published digest, to simulate a corrupt or
	// substituted download.
	checksum   map[string]string
	noRelease  bool
	omitAsset  bool
	serverErr  bool
	requests   []string
	assetName  string
	truncateTo int
}

func newFakeGitHub(latest string) *fakeGitHub {
	return &fakeGitHub{
		latest:   latest,
		binary:   map[string]string{},
		checksum: map[string]string{},
	}
}

func (f *fakeGitHub) serve(t *testing.T) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		f.requests = append(f.requests, r.URL.Path)
		if f.noRelease {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if f.serverErr {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/releases/tag/v"+f.latest, http.StatusFound)
	})

	mux.HandleFunc("/releases/download/", func(w http.ResponseWriter, r *http.Request) {
		f.requests = append(f.requests, r.URL.Path)
		name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]

		if name == "checksums.txt" {
			if f.omitAsset {
				_, _ = w.Write([]byte("deadbeef  something-else\n"))
				return
			}
			var b strings.Builder
			for version, body := range f.binary {
				sum := sha256.Sum256([]byte(body))
				digest := hex.EncodeToString(sum[:])
				if override, ok := f.checksum[version]; ok {
					digest = override
				}
				asset := f.assetName
				if asset == "" {
					asset = fmt.Sprintf("tumika_%s_linux_arm64", version)
				}
				fmt.Fprintf(&b, "%s  %s\n", digest, asset)
			}
			_, _ = w.Write([]byte(b.String()))
			return
		}

		for version, body := range f.binary {
			if strings.Contains(name, version) {
				if f.truncateTo > 0 && f.truncateTo < len(body) {
					body = body[:f.truncateTo]
				}
				_, _ = w.Write([]byte(body))
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func newTestSource(t *testing.T, f *fakeGitHub) *GitHub {
	t.Helper()
	server := f.serve(t)
	return NewGitHub(
		WithBaseURL(server.URL),
		WithHTTPClient(server.Client()),
		WithPlatform("linux", "arm64"),
	)
}

func TestLatestReadsTheRedirect(t *testing.T) {
	f := newFakeGitHub("1.2.3")
	src := newTestSource(t, f)

	got, err := src.Latest(context.Background())
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if got != "1.2.3" {
		t.Errorf("Latest = %q, want 1.2.3 (without the leading v)", got)
	}
}

// A repository with no releases is a NORMAL state before the first tag, not a
// failure — the runner logs it at debug and carries on.
func TestLatestOnARepositoryWithNoReleases(t *testing.T) {
	f := newFakeGitHub("")
	f.noRelease = true
	src := newTestSource(t, f)

	if _, err := src.Latest(context.Background()); !errors.Is(err, ErrNoRelease) {
		t.Fatalf("= %v, want ErrNoRelease", err)
	}
}

// Anything else is a real failure and must not be mistaken for "no release",
// which would silently stop a fleet from ever updating.
func TestLatestOnAServerError(t *testing.T) {
	f := newFakeGitHub("")
	f.serverErr = true
	src := newTestSource(t, f)

	_, err := src.Latest(context.Background())
	if err == nil {
		t.Fatal("a 500 was accepted")
	}
	if errors.Is(err, ErrNoRelease) {
		t.Error("a server error was reported as 'no release', which would stop updates silently")
	}
}

func TestFetchVerifiesAndInstalls(t *testing.T) {
	f := newFakeGitHub("1.2.3")
	f.binary["1.2.3"] = "the new binary"
	src := newTestSource(t, f)

	dest := filepath.Join(t.TempDir(), "tumika")
	if err := src.Fetch(context.Background(), "1.2.3", dest); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	body, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(body) != "the new binary" {
		t.Errorf("downloaded %q", body)
	}

	// It is about to be executed.
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Errorf("mode = %#o, which is not executable", info.Mode().Perm())
	}
}

// THE property. A substituted or corrupted download must never reach the binary
// path — this is the only thing standing between a compromised mirror and a
// binary the daemon will execute as a service account.
func TestFetchRefusesAChecksumMismatch(t *testing.T) {
	f := newFakeGitHub("1.2.3")
	f.binary["1.2.3"] = "the new binary"
	f.checksum["1.2.3"] = strings.Repeat("0", 64)
	src := newTestSource(t, f)

	dest := filepath.Join(t.TempDir(), "tumika")
	err := src.Fetch(context.Background(), "1.2.3", dest)
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("= %v, want ErrChecksumMismatch", err)
	}
	if _, statErr := os.Stat(dest); !errors.Is(statErr, os.ErrNotExist) {
		t.Error("a binary that failed verification was written to the destination")
	}
}

// A truncated download hashes differently, so the checksum catches it — but the
// point is what is left behind, because the caller is about to exec this path.
func TestFetchLeavesNothingAfterATruncatedDownload(t *testing.T) {
	f := newFakeGitHub("1.2.3")
	f.binary["1.2.3"] = "the new binary, in full"
	f.truncateTo = 6
	src := newTestSource(t, f)

	dir := t.TempDir()
	dest := filepath.Join(dir, "tumika")
	if err := src.Fetch(context.Background(), "1.2.3", dest); !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("= %v, want ErrChecksumMismatch", err)
	}
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Error("a truncated binary was installed")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the directory: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".tumika-update-") {
			t.Errorf("a staging file was left behind: %s", entry.Name())
		}
	}
}

// A release that published nothing for this platform is a clear error, not a
// mystery. It happens when a build target is dropped.
func TestFetchWithNoAssetForThisPlatform(t *testing.T) {
	f := newFakeGitHub("1.2.3")
	f.binary["1.2.3"] = "the new binary"
	f.omitAsset = true
	src := newTestSource(t, f)

	dest := filepath.Join(t.TempDir(), "tumika")
	if err := src.Fetch(context.Background(), "1.2.3", dest); !errors.Is(err, ErrNoAsset) {
		t.Fatalf("= %v, want ErrNoAsset", err)
	}
}

// The asset name is a CONTRACT with .goreleaser.yml and with install.sh. It is
// asserted here so a rename breaks a test rather than every deployed daemon's
// next update.
func TestAssetNameMatchesTheReleaseTemplate(t *testing.T) {
	src := NewGitHub(WithPlatform("linux", "arm64"))
	if got := src.AssetName("1.2.3"); got != "tumika_1.2.3_linux_arm64" {
		t.Errorf("AssetName = %q, want tumika_1.2.3_linux_arm64", got)
	}
	src = NewGitHub(WithPlatform("darwin", "amd64"))
	if got := src.AssetName("0.1.0"); got != "tumika_0.1.0_darwin_amd64" {
		t.Errorf("AssetName = %q, want tumika_0.1.0_darwin_amd64", got)
	}
}

// Strictly greater, and semver-aware: string comparison gets 0.10.0 vs 0.9.0
// exactly wrong, which is the version where a fleet would stop updating.
func TestNewer(t *testing.T) {
	newer := [][2]string{
		{"0.2.0", "0.1.0"},
		{"0.10.0", "0.9.0"},
		{"1.0.0", "0.99.99"},
		{"v0.2.0", "0.1.0"},
		{"0.1.1", "0.1.0"},
	}
	for _, pair := range newer {
		if !Newer(pair[0], pair[1]) {
			t.Errorf("Newer(%s, %s) = false, want true", pair[0], pair[1])
		}
	}

	notNewer := [][2]string{
		{"0.1.0", "0.1.0"},
		{"0.1.0", "0.2.0"},
		{"0.9.0", "0.10.0"},
		// A development build is not a version, so nothing is newer than it —
		// self-update is disabled there anyway, and pretending otherwise would
		// have every dev build trying to replace itself.
		{"0.1.0", "dev"},
		{"dev", "0.1.0"},
		{"", "0.1.0"},
		{"not-a-version", "0.1.0"},
	}
	for _, pair := range notNewer {
		if Newer(pair[0], pair[1]) {
			t.Errorf("Newer(%s, %s) = true, want false", pair[0], pair[1])
		}
	}
}

// A prerelease tag must not be offered to a daemon on a stable version.
// goreleaser's `prerelease: auto` keeps GitHub from resolving /releases/latest
// to one, but the comparison has to agree.
func TestNewerTreatsAPrereleaseAsOlderThanItsRelease(t *testing.T) {
	if Newer("1.0.0-rc1", "1.0.0") {
		t.Error("an RC was reported as newer than its own release")
	}
}

// A redirect that does not end in a version tag means GitHub answered something
// unexpected, and guessing would install whatever the path happened to end in.
func TestLatestRefusesAnUnparseableRedirect(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/somewhere/else", http.StatusFound)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	src := NewGitHub(WithBaseURL(server.URL), WithHTTPClient(server.Client()))
	if _, err := src.Latest(context.Background()); err == nil {
		t.Fatal("an unparseable redirect was accepted")
	}
}

// The tag has to be a real version, not merely start with "v".
func TestLatestRefusesANonSemverTag(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/releases/tag/vNext", http.StatusFound)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	src := NewGitHub(WithBaseURL(server.URL), WithHTTPClient(server.Client()))
	if _, err := src.Latest(context.Background()); err == nil {
		t.Fatal("a non-semver tag was accepted")
	}
}

// A missing checksums.txt must stop the update. Downloading a binary and
// installing it unverified is the one thing this package exists to prevent.
func TestFetchWithNoChecksumsFile(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/download/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "checksums.txt") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte("a binary nobody verified"))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	src := NewGitHub(WithBaseURL(server.URL), WithHTTPClient(server.Client()),
		WithPlatform("linux", "arm64"))

	dest := filepath.Join(t.TempDir(), "tumika")
	if err := src.Fetch(context.Background(), "1.2.3", dest); err == nil {
		t.Fatal("a release with no checksums.txt was installed")
	}
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Error("an unverified binary was written")
	}
}

// A 404 on the binary itself, with the checksum present — a release whose asset
// upload failed.
func TestFetchWithAMissingBinary(t *testing.T) {
	f := newFakeGitHub("1.2.3")
	f.binary["1.2.3"] = "the new binary"
	src := newTestSource(t, f)

	dest := filepath.Join(t.TempDir(), "tumika")
	// Ask for a version the fake serves no binary for, but whose checksum line
	// is absent too — the checksum lookup fails first, which is the right order:
	// nothing is downloaded that cannot be verified.
	if err := src.Fetch(context.Background(), "9.9.9", dest); err == nil {
		t.Fatal("a missing binary was installed")
	}
}

// An unwritable destination directory is an error, not a silent no-op.
func TestFetchIntoAnUnwritableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the permission bits this relies on")
	}

	f := newFakeGitHub("1.2.3")
	f.binary["1.2.3"] = "the new binary"
	src := newTestSource(t, f)

	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	if err := src.Fetch(context.Background(), "1.2.3", filepath.Join(dir, "tumika")); err == nil {
		t.Fatal("staging into an unwritable directory reported success")
	}
}

// A cancelled context stops the download rather than running to completion.
func TestFetchRespectsCancellation(t *testing.T) {
	f := newFakeGitHub("1.2.3")
	f.binary["1.2.3"] = "the new binary"
	src := newTestSource(t, f)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	dest := filepath.Join(t.TempDir(), "tumika")
	if err := src.Fetch(ctx, "1.2.3", dest); err == nil {
		t.Fatal("a cancelled fetch reported success")
	}
}

// A 5xx on the binary itself, with a valid checksum entry — a CDN failing
// mid-release. Nothing must be installed.
func TestFetchWithAServerErrorOnTheBinary(t *testing.T) {
	body := "the new binary"
	sum := sha256.Sum256([]byte(body))

	mux := http.NewServeMux()
	mux.HandleFunc("/releases/download/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "checksums.txt") {
			_, _ = fmt.Fprintf(w, "%s  tumika_1.2.3_linux_arm64\n", hex.EncodeToString(sum[:]))
			return
		}
		w.WriteHeader(http.StatusBadGateway)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	src := NewGitHub(WithBaseURL(server.URL), WithHTTPClient(server.Client()),
		WithPlatform("linux", "arm64"))

	dest := filepath.Join(t.TempDir(), "tumika")
	if err := src.Fetch(context.Background(), "1.2.3", dest); err == nil {
		t.Fatal("a 502 on the binary was accepted")
	}
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Error("something was installed despite the download failing")
	}
}

// An unreachable host is an error, not an empty answer.
func TestLatestWithAnUnreachableHost(t *testing.T) {
	src := NewGitHub(WithBaseURL("http://127.0.0.1:1"))
	if _, err := src.Latest(context.Background()); err == nil {
		t.Fatal("an unreachable host reported a version")
	}
}
