// Package release finds and fetches tumika's own releases.
//
// The seam exists so UpdateService can be tested without GitHub: the whole
// update path — download, verify, pre-flight, replace, roll back — is exactly
// the code that must not be exercised for the first time in production.
package release

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

// DefaultBaseURL is tumika's repository.
const DefaultBaseURL = "https://github.com/tumika/tumika"

// maxBinaryBytes bounds a download. The binary is ~20 MB; anything serving
// orders of magnitude more is not a release.
const maxBinaryBytes = 512 << 20

// maxChecksumBytes bounds checksums.txt, which is a handful of lines.
const maxChecksumBytes = 1 << 20

// Errors callers distinguish.
var (
	// ErrNoRelease means the repository has no published release yet.
	ErrNoRelease = errors.New("no release found")
	// ErrChecksumMismatch means the download is not what checksums.txt
	// describes. Never recoverable by retrying the same asset.
	ErrChecksumMismatch = errors.New("downloaded binary does not match the published checksum")
	// ErrNoAsset means the release has nothing built for this platform.
	ErrNoAsset = errors.New("no release asset for this platform")
)

// Source finds and fetches releases.
type Source interface {
	// Latest is the newest published version, without a leading "v".
	Latest(ctx context.Context) (string, error)
	// Fetch downloads that version's binary for this platform to dest,
	// verifying it against the release's checksums.txt before returning.
	Fetch(ctx context.Context, version, dest string) error
}

// GitHub fetches releases from a GitHub repository.
type GitHub struct {
	baseURL string
	client  *http.Client
	// goos and goarch name the asset. Fields rather than runtime constants so a
	// test can ask for a platform it is not running on.
	goos, goarch string
}

// Option configures the source.
type Option func(*GitHub)

// WithBaseURL points at a different repository, for tests.
func WithBaseURL(url string) Option {
	return func(g *GitHub) { g.baseURL = strings.TrimSuffix(url, "/") }
}

// WithHTTPClient replaces the HTTP client.
func WithHTTPClient(c *http.Client) Option {
	return func(g *GitHub) { g.client = c }
}

// WithPlatform overrides the asset platform.
func WithPlatform(goos, goarch string) Option {
	return func(g *GitHub) { g.goos, g.goarch = goos, goarch }
}

// NewGitHub builds the source.
func NewGitHub(opts ...Option) *GitHub {
	g := &GitHub{
		baseURL: DefaultBaseURL,
		// Generous: a Pi on a domestic connection downloading ~20 MB.
		client: &http.Client{Timeout: 10 * time.Minute},
		goos:   runtime.GOOS,
		goarch: runtime.GOARCH,
	}
	for _, opt := range opts {
		opt(g)
	}
	return g
}

// AssetName is the raw binary published for a platform.
//
// It matches .goreleaser.yml's raw archive template exactly, and
// scripts/verify-release-assets.sh checks that the release actually ships this
// name — so a template change breaks CI rather than every installed daemon's
// next update.
func (g *GitHub) AssetName(version string) string {
	return fmt.Sprintf("tumika_%s_%s_%s", version, g.goos, g.goarch)
}

func (g *GitHub) assetURL(version, asset string) string {
	return fmt.Sprintf("%s/releases/download/v%s/%s", g.baseURL, version, asset)
}

// Latest follows the /releases/latest redirect.
//
// The redirect rather than the API: it needs no token, no rate limit applies,
// and it is the same URL install.sh uses — so both agree on what "latest" means.
// GitHub resolves it to the newest non-prerelease, which is why .goreleaser.yml
// sets `prerelease: auto`.
func (g *GitHub) Latest(ctx context.Context) (string, error) {
	// A client that does NOT follow the redirect: the Location header is the
	// answer, and following it would fetch an HTML page to parse instead.
	noRedirect := *g.client
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.baseURL+"/releases/latest", nil)
	if err != nil {
		return "", fmt.Errorf("build the latest-release request: %w", err)
	}

	resp, err := noRedirect.Do(req)
	if err != nil {
		return "", fmt.Errorf("reach %s: %w", g.baseURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 300 || resp.StatusCode >= 400 {
		// 404 is the honest answer for a repository with no releases yet, and
		// it is a NORMAL state for a project before its first tag — not a
		// failure worth waking anyone about.
		if resp.StatusCode == http.StatusNotFound {
			return "", fmt.Errorf("%w at %s", ErrNoRelease, g.baseURL)
		}
		return "", fmt.Errorf("expected a redirect from %s, got %s", req.URL, resp.Status)
	}

	location := resp.Header.Get("Location")
	tag := location[strings.LastIndex(location, "/")+1:]
	if !strings.HasPrefix(tag, "v") || !semver.IsValid(tag) {
		return "", fmt.Errorf("cannot read a version from the redirect to %q", location)
	}
	return strings.TrimPrefix(tag, "v"), nil
}

// Fetch downloads the binary and verifies it before returning.
//
// dest is written only if the checksum matches: the caller gets a file that is
// either correct or absent, never a partially-written binary it might go on to
// execute.
func (g *GitHub) Fetch(ctx context.Context, version, dest string) error {
	asset := g.AssetName(version)

	want, err := g.checksum(ctx, version, asset)
	if err != nil {
		return err
	}

	// Staged beside dest so the publishing rename cannot cross a filesystem —
	// a rename is atomic, and a cross-device move degrades to copy-then-truncate,
	// which is how a running daemon's binary gets destroyed mid-write.
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".tumika-update-*")
	if err != nil {
		return fmt.Errorf("stage an update next to %s: %w", dest, err)
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()

	got, err := g.download(ctx, g.assetURL(version, asset), tmp)
	if err != nil {
		return err
	}
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("%w: %s hashes to %s, the release says %s",
			ErrChecksumMismatch, asset, got, want)
	}

	// Flushed before the rename publishes it. On the Pi + SD card this is
	// written for, a power cut can otherwise leave the rename durable and the
	// contents not — and the next boot would exec a truncated binary.
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("flush %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpPath, err)
	}
	// CreateTemp makes the file 0600, and it is about to become a binary
	// something has to execute.
	if err := os.Chmod(tmpPath, 0o755); err != nil { // #nosec G302 -- an executable binary must stay executable
		return fmt.Errorf("set permissions on %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, dest); err != nil {
		return fmt.Errorf("install %s: %w", dest, err)
	}
	return nil
}

// checksum reads the release's checksums.txt and returns the asset's digest.
func (g *GitHub) checksum(ctx context.Context, version, asset string) (string, error) {
	url := g.assetURL(version, "checksums.txt")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("build the checksum request: %w", err)
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch %s: %s", url, resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxChecksumBytes))
	if err != nil {
		return "", fmt.Errorf("read %s: %w", url, err)
	}

	for line := range strings.SplitSeq(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == asset {
			return fields[0], nil
		}
	}
	// The release exists but has nothing for this machine — a dropped build
	// target, or a platform that was never published.
	return "", fmt.Errorf("%w: %s is not in %s", ErrNoAsset, asset, url)
}

// download streams the asset into w and returns its SHA-256.
//
// Hashed WHILE streaming, so the binary is never held in memory and the digest
// covers exactly the bytes written.
func (g *GitHub) download(ctx context.Context, url string, w io.Writer) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("build the download request: %w", err)
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch %s: %s", url, resp.Status)
	}

	digest := sha256.New()
	if _, err := io.Copy(io.MultiWriter(w, digest), io.LimitReader(resp.Body, maxBinaryBytes)); err != nil {
		return "", fmt.Errorf("download %s: %w", url, err)
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// Newer reports whether candidate is a strictly greater version than current.
//
// Strictly greater, so a daemon never "updates" sideways onto the version it is
// already running, and never downgrades because someone deleted a release.
func Newer(candidate, current string) bool {
	c, cur := "v"+strings.TrimPrefix(candidate, "v"), "v"+strings.TrimPrefix(current, "v")
	if !semver.IsValid(c) || !semver.IsValid(cur) {
		return false
	}
	return semver.Compare(c, cur) > 0
}
