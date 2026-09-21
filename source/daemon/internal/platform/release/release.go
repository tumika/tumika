// Package release finds and fetches tumika's own releases.
//
// Discovery is a signed bill of materials served as static JSON: a channel
// document names the release that channel currently offers, and the release's
// own document names every component's version and asset. Nothing is trusted
// before its detached signature verifies against a compiled-in release key.
//
// The seam exists so UpdateService can be tested without a release host: the
// whole update path — download, verify, pre-flight, replace, roll back — is
// exactly the code that must not be exercised for the first time in production.
package release

import (
	"context"
	"crypto/ecdsa"
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

// DefaultBaseURL is the host serving the channel and release documents.
const DefaultBaseURL = "https://get.tumika.org"

// DaemonComponent is the component name the daemon reads out of a bill of
// materials.
const DaemonComponent = "daemon"

// DesktopComponent is the component name the desktop app is published under.
//
// Nothing in the daemon reads this component: the app resolves its own entry out
// of the bill of materials of the release the daemon it talks to is running. The
// name lives here because the publisher and the app must spell it identically,
// and the publisher is built from this package.
const DesktopComponent = "desktop"

// signatureSuffix names the detached signature beside a document: the channel
// head is `<base>/channels/<channel>.json` and its signature is that same path
// with this appended.
const signatureSuffix = ".sig"

// maxBinaryBytes bounds a download. The binary is ~20 MB; anything serving
// orders of magnitude more is not a release.
const maxBinaryBytes = 512 << 20

// maxBOMBytes bounds a bill of materials, which is a few dozen lines of JSON.
// The document is read whole in order to verify a signature over its exact
// bytes, so the cap is what keeps an unauthenticated host from choosing how
// much memory the daemon allocates.
const maxBOMBytes = 1 << 20

// maxSignatureBytes bounds a detached signature: base64 of an ASN.1 P-256
// signature is under a hundred bytes.
const maxSignatureBytes = 4 << 10

// Errors callers distinguish.
var (
	// ErrNoRelease means the channel offers nothing: no document is published
	// at that path. It is a NORMAL state for a project before its first
	// release, not a failure worth waking anyone about.
	ErrNoRelease = errors.New("no release found")
	// ErrChecksumMismatch means the download is not what the bill of materials
	// describes. Never recoverable by retrying the same asset.
	ErrChecksumMismatch = errors.New("downloaded binary does not match the published checksum")
	// ErrNoAsset means the release has nothing built for this platform.
	ErrNoAsset = errors.New("no release asset for this platform")
)

// errNotFound marks a 404 so each caller can say what a missing document
// means — a channel with nothing published, or a document served without its
// signature — rather than reporting both as the same transport failure.
var errNotFound = errors.New("not found")

// Source is what UpdateService consumes: what a channel currently offers, when
// a given release was published, and a download of an asset one of those
// documents names.
//
// Everything here comes from a verified bill of materials, so an implementation
// hands the updater a decision it can act on without checking the bytes again.
type Source interface {
	// Head is what the channel currently offers for this platform.
	Head(ctx context.Context, channel Channel) (Head, error)
	// ReleaseBOM is one release's own bill of materials, looked up by label.
	// It is how a daemon learns when its own release was published.
	ReleaseBOM(ctx context.Context, label string) (*BOM, error)
	// FetchAsset downloads an asset to dest, verifying it against the digest
	// the bill of materials publishes before returning.
	FetchAsset(ctx context.Context, asset Asset, dest string) error
}

// Head is what a channel currently offers, read from a verified bill of
// materials.
type Head struct {
	// Release is the label people read and no client compares.
	Release string
	// Channel is the channel this head was read from.
	Channel Channel
	// PublishedAt orders releases: recency, not the label, decides which
	// release a channel offers.
	PublishedAt time.Time
	// Version is the daemon component's semver in this release, without a
	// leading "v".
	Version string
	// Asset is the daemon binary published for the platform this source
	// targets.
	Asset Asset
}

// GitHub fetches releases published under a base URL.
type GitHub struct {
	baseURL string
	client  *http.Client
	// keys verifies a bill of materials. Nil means the keys compiled into this
	// binary, which is what a daemon uses; a test signs with its own.
	keys []*ecdsa.PublicKey
	// goos and goarch name the asset. Fields rather than runtime constants so a
	// test can ask for a platform it is not running on.
	goos, goarch string
}

// Option configures the source.
type Option func(*GitHub)

// WithBaseURL points at a different release host, for tests.
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

// WithKeys replaces the keys a bill of materials is verified against.
//
// A daemon verifies against the keys compiled into it. A test signs its
// fixtures with a key it generates, because the private half of a release key
// does not exist in this repository. An empty list verifies nothing.
func WithKeys(keys []*ecdsa.PublicKey) Option {
	return func(g *GitHub) { g.keys = keys }
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

// Head reads the channel's current offer.
//
// The channel name is validated before it reaches a URL, and the verified
// document must agree about which channel it is the head of — a stable
// document served from the edge path is refused rather than followed.
func (g *GitHub) Head(ctx context.Context, channel Channel) (Head, error) {
	if err := ValidateChannel(channel); err != nil {
		return Head{}, err
	}

	docURL := g.baseURL + "/channels/" + string(channel) + ".json"
	bom, err := g.bom(ctx, docURL)
	if err != nil {
		return Head{}, err
	}
	if bom.Channel != channel {
		return Head{}, fmt.Errorf("%w: %s is the head of channel %q", ErrMalformedBOM, docURL, bom.Channel)
	}

	component, ok := bom.Components[DaemonComponent]
	if !ok {
		return Head{}, fmt.Errorf("%w: release %s ships no %s", ErrNoAsset, bom.Release, DaemonComponent)
	}
	asset, ok := bom.Asset(DaemonComponent, g.goos, g.goarch)
	if !ok {
		// The release exists but has nothing for this machine — a dropped build
		// target, or a platform that was never published.
		return Head{}, fmt.Errorf("%w: release %s publishes no %s for %s",
			ErrNoAsset, bom.Release, DaemonComponent, PlatformKey(g.goos, g.goarch))
	}

	return Head{
		Release:     bom.Release,
		Channel:     bom.Channel,
		PublishedAt: bom.PublishedAt,
		Version:     strings.TrimPrefix(component.Version, "v"),
		Asset:       asset,
	}, nil
}

// ReleaseBOM reads one release's own bill of materials.
//
// It is how a daemon learns when its own release was published, and how a
// component version is resolved to the version of another component that
// belongs with it. The label is validated BEFORE it is placed in the URL: it
// comes from a build stamp or from another process, so a label that could
// escape a path segment must never reach the host at all.
func (g *GitHub) ReleaseBOM(ctx context.Context, label string) (*BOM, error) {
	if err := ValidateReleaseLabel(label); err != nil {
		return nil, err
	}

	docURL := g.baseURL + "/releases/" + label + ".json"
	bom, err := g.bom(ctx, docURL)
	if err != nil {
		return nil, err
	}
	if bom.Release != label {
		return nil, fmt.Errorf("%w: %s describes release %q", ErrMalformedBOM, docURL, bom.Release)
	}
	return bom, nil
}

// FetchAsset downloads an asset a bill of materials names and verifies it
// before returning.
//
// dest is written only if the digest matches: the caller gets a file that is
// either correct or absent, never a partially-written binary it might go on to
// execute.
func (g *GitHub) FetchAsset(ctx context.Context, asset Asset, dest string) error {
	// Sweep anything a previous attempt left behind.
	//
	// The deferred cleanup below only covers THIS call: a kill or a power cut
	// mid-download leaves a partial file in the live binary's directory, and
	// nothing else ever removes it. On the Pi + SD card this is written for,
	// repeated failed updates accumulate ~20 MB each until the card is full.
	sweepStagingFiles(filepath.Dir(dest))

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

	got, err := g.download(ctx, asset.URL, tmp)
	if err != nil {
		return err
	}
	if !strings.EqualFold(got, asset.SHA256) {
		return fmt.Errorf("%w: %s hashes to %s, the bill of materials says %s",
			ErrChecksumMismatch, asset.URL, got, asset.SHA256)
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

// bom reads a document and its detached signature, and returns it only if the
// signature verifies.
//
// A document with no signature beside it is refused exactly as a tampered one
// is: the bill of materials names the bytes the daemon will execute, so an
// unsigned document is not a weaker answer, it is no answer.
func (g *GitHub) bom(ctx context.Context, docURL string) (*BOM, error) {
	body, err := g.document(ctx, docURL, maxBOMBytes)
	if err != nil {
		if errors.Is(err, errNotFound) {
			return nil, fmt.Errorf("%w at %s", ErrNoRelease, docURL)
		}
		return nil, err
	}

	signature, err := g.document(ctx, docURL+signatureSuffix, maxSignatureBytes)
	if err != nil {
		if errors.Is(err, errNotFound) {
			return nil, fmt.Errorf("%w: nothing is published at %s%s", ErrUnsignedBOM, docURL, signatureSuffix)
		}
		return nil, err
	}

	if g.keys != nil {
		return VerifyBOM(body, signature, g.keys)
	}
	return OpenBOM(body, signature)
}

// document reads a whole document, refusing one larger than max.
func (g *GitHub) document(ctx context.Context, url string, max int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build the request for %s: %w", url, err)
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%s: %w", url, errNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: %s", url, resp.Status)
	}

	// One byte past the cap, so an oversized document is refused rather than
	// silently truncated into a body whose signature could never verify.
	body, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", url, err)
	}
	if int64(len(body)) > max {
		return nil, fmt.Errorf("read %s: larger than the %d byte cap", url, max)
	}
	return body, nil
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

// sweepStagingFiles removes leftovers from interrupted downloads.
//
// Best-effort throughout: this runs before an update, and failing to tidy is
// never a reason to refuse one.
func sweepStagingFiles(dir string) {
	matches, err := filepath.Glob(filepath.Join(dir, ".tumika-update-*"))
	if err != nil {
		return
	}
	for _, path := range matches {
		_ = os.Remove(path)
	}
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

// Later reports whether candidate was published strictly after current.
//
// This is the comparison that orders releases, because a release label is never
// compared. A zero current is older than anything published: a daemon that
// cannot learn when its own release was published — its bill of materials is
// pruned, or it is a development build — is offered the head rather than pinned
// to nothing. A zero candidate is never later, because an undated document
// orders nothing.
func Later(candidate, current time.Time) bool {
	if candidate.IsZero() {
		return false
	}
	return candidate.After(current)
}
