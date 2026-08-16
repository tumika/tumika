package claudecode

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"golang.org/x/mod/semver"

	"github.com/tumika/tumika/source/internal/domain"
)

// BinaryName is what the manifest calls the executable, and what tumika stores
// it as.
const BinaryName = "claude"

// DefaultRetention is how many installed versions to keep.
//
// Two, because the linux-arm64 build is ~307 MB and a Raspberry Pi's SD card is
// a supported deployment. Two rather than one so an operator can roll the pin
// back without a download.
const DefaultRetention = 2

// Installer downloads and manages vendored Claude Code binaries.
//
// Layout is one directory per version under the providers root:
//
//	<providers>/claude-code/<version>/claude
//
// A version is either completely present or absent — never half-written — which
// is what lets Installed() simply list directories.
type Installer struct {
	root string
	// staging is a sibling of root, deliberately outside the directory
	// Installed() enumerates, and on the same filesystem so the publishing
	// rename cannot cross a boundary.
	staging string
	release *releaseClient
}

// InstallerOption configures the installer.
type InstallerOption func(*installerConfig)

type installerConfig struct {
	baseURL string
	client  *http.Client
}

// WithBaseURL points the installer at a different release bucket, for tests.
func WithBaseURL(url string) InstallerOption {
	return func(c *installerConfig) { c.baseURL = url }
}

// WithHTTPClient replaces the HTTP client.
func WithHTTPClient(client *http.Client) InstallerOption {
	return func(c *installerConfig) { c.client = client }
}

// NewInstaller builds an installer rooted at the providers directory.
func NewInstaller(providersRoot string, opts ...InstallerOption) (*Installer, error) {
	cfg := installerConfig{baseURL: DefaultBaseURL}
	for _, opt := range opts {
		opt(&cfg)
	}

	release, err := newReleaseClient(cfg.baseURL, cfg.client)
	if err != nil {
		return nil, err
	}

	return &Installer{
		root:    filepath.Join(providersRoot, "claude-code"),
		staging: filepath.Join(providersRoot, ".claude-code-staging"),
		release: release,
	}, nil
}

// ErrInvalidVersion means a version string is not a bare version token.
var ErrInvalidVersion = errors.New("invalid version")

// validateVersion refuses anything that is not a bare version token.
//
// Path() joins this into a filesystem path and the release client interpolates
// it into a URL, so an unchecked value escapes both. "../../../../usr/local/bin"
// cleans to a path outside the providers root, where Install's fast path would
// find an existing `claude`, never download anything, and report it as an
// installed version — handing the daemon an unverified binary to execute. That
// is precisely the hijack this package exists to prevent.
func validateVersion(version string) error {
	if version == "" {
		return fmt.Errorf("%w: no version specified", ErrInvalidVersion)
	}
	if !semver.IsValid("v" + version) {
		return fmt.Errorf("%w: %q is not a version", ErrInvalidVersion, version)
	}
	// semver.IsValid already excludes separators, but the intent is worth
	// stating where a reader is looking for it.
	if strings.ContainsAny(version, `/\`) || version != filepath.Base(version) {
		return fmt.Errorf("%w: %q contains a path separator", ErrInvalidVersion, version)
	}
	return nil
}

// usable reports whether path is a binary the daemon could actually execute.
//
// Mere existence is not enough: a directory named `claude`, a zero-byte file, or
// one with the execute bit stripped all satisfy os.Stat and none can be run. An
// Install that accepted any of them would be a permanent no-op reporting
// success.
func usable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Size() > 0 && info.Mode().Perm()&0o100 != 0
}

// Path is where a version's binary lives once installed.
//
// Absolute, and executed as such: there is deliberately no launcher symlink to
// repoint and no PATH lookup to win, so the binary tumika runs is the one it
// verified. See
// .agents/rules/every-spawned-claude-process-is-credential-isolated.md.
func (i *Installer) Path(version string) string {
	return filepath.Join(i.root, version, BinaryName)
}

// Installed lists the versions present, newest first.
func (i *Installer) Installed(context.Context) ([]string, error) {
	entries, err := os.ReadDir(i.root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list installed versions: %w", err)
	}

	var versions []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		// Dot-prefixed directories are never versions. Staging lives elsewhere
		// now, but a leftover from an older build must not be reported as an
		// installed version — its binary is created before a single byte is
		// downloaded, let alone verified.
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		if validateVersion(entry.Name()) != nil {
			continue
		}
		// A directory without a runnable binary is debris from an interrupted
		// install, not a version anyone can use.
		if !usable(filepath.Join(i.root, entry.Name(), BinaryName)) {
			continue
		}
		versions = append(versions, entry.Name())
	}

	sortVersionsNewestFirst(versions)
	return versions, nil
}

// Install downloads and verifies a version, unless it is already present.
//
// The order is the security property: fetch the SIGNED manifest, verify its
// signature against the embedded key, and only then use its checksums to judge
// the binary. Verifying a download against a checksum from an unsigned document
// proves the bytes arrived intact, not that they are the right bytes.
func (i *Installer) Install(ctx context.Context, version string) (domain.InstallResult, error) {
	if err := validateVersion(version); err != nil {
		return domain.InstallResult{}, err
	}

	target := i.Path(version)
	if usable(target) {
		return domain.InstallResult{Version: version, Path: target, AlreadyPresent: true}, nil
	}

	manifest, err := i.release.Manifest(ctx, version)
	if err != nil {
		return domain.InstallResult{}, err
	}

	// Staged OUTSIDE the directory Installed() enumerates, and beside it so the
	// publishing rename stays on one filesystem.
	//
	// Staging inside that directory put a `claude` file — created before a
	// single byte was downloaded — where Installed() would list it as a version
	// and Path() would hand out a runnable path to unverified bytes. Worse,
	// Prune walked the same list and would delete the staging directory of an
	// install still in flight.
	if err := os.MkdirAll(i.root, 0o700); err != nil {
		return domain.InstallResult{}, fmt.Errorf("create %s: %w", i.root, err)
	}
	if err := os.MkdirAll(i.staging, 0o700); err != nil {
		return domain.InstallResult{}, fmt.Errorf("create %s: %w", i.staging, err)
	}
	staging, err := os.MkdirTemp(i.staging, version+"-")
	if err != nil {
		return domain.InstallResult{}, fmt.Errorf("create a staging directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()

	// Two gosec exceptions, both deliberate.
	//
	// G304 flags the variable path. It is not caller-influenced: staging is a
	// directory this function just created under its own root, and BinaryName is
	// a constant; the only variability is the temp suffix.
	//
	// G302 wants 0600 or less. This is an executable, so it needs its execute
	// bit — 0700 IS the minimum here, and it is owner-only, which is what the
	// rule is actually protecting.
	staged := filepath.Join(staging, BinaryName)
	file, err := os.OpenFile(staged, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700) // #nosec G304,G302 -- constructed path under our own root; 0700 is minimal for an executable
	if err != nil {
		return domain.InstallResult{}, fmt.Errorf("create %s: %w", staged, err)
	}

	if err := i.release.Download(ctx, version, manifest, file); err != nil {
		_ = file.Close()
		// The staged copy is removed with the staging directory. Nothing
		// unverified is ever left where Installed() would find it.
		return domain.InstallResult{}, err
	}
	// Flushed to the device before the rename publishes it. On the Pi + SD card
	// deployment this package is written for, a power cut can otherwise make the
	// rename durable while the contents are not — leaving a zero-length or
	// truncated `claude` that the fast path above will treat as installed
	// forever.
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return domain.InstallResult{}, fmt.Errorf("flush %s: %w", staged, err)
	}
	if err := file.Close(); err != nil {
		return domain.InstallResult{}, fmt.Errorf("close %s: %w", staged, err)
	}
	if err := syncDir(staging); err != nil {
		return domain.InstallResult{}, err
	}

	// Published by renaming the whole directory: a reader either sees no version
	// or sees a complete, verified one.
	if err := os.Rename(staging, filepath.Join(i.root, version)); err != nil {
		if !errors.Is(err, os.ErrExist) && !errors.Is(err, syscall.ENOTEMPTY) {
			return domain.InstallResult{}, fmt.Errorf("install %s: %w", version, err)
		}

		// Something already occupies the destination. If it is a usable binary,
		// another process won the race and its copy is verified too.
		//
		// If it is NOT — an empty directory left by an interrupted install, say
		// — then reporting success would be a lie: the verified download is
		// about to be discarded by the deferred cleanup, and Path() would point
		// at a file that does not exist. Every later attempt would repeat it, so
		// the daemon would be permanently stuck with a "present" version it
		// cannot execute.
		if usable(target) {
			return domain.InstallResult{Version: version, Path: target, AlreadyPresent: true}, nil
		}
		return domain.InstallResult{}, fmt.Errorf(
			"install %s: %s already exists but holds no usable binary; remove it and retry: %w",
			version, filepath.Join(i.root, version), err)
	}

	if err := syncDir(i.root); err != nil {
		return domain.InstallResult{}, err
	}

	return domain.InstallResult{Version: version, Path: target}, nil
}

// syncDir flushes a directory entry, so a rename survives a power cut.
func syncDir(path string) error {
	dir, err := os.Open(path) // #nosec G304 -- a directory this package created
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = dir.Close() }()

	if err := dir.Sync(); err != nil {
		return fmt.Errorf("flush %s: %w", path, err)
	}
	return nil
}

// Prune removes all but the newest keep versions, never removing protected ones.
//
// protected is the pinned version and anything else the caller must not lose.
// Retention is about disk, not about tidiness: 307 MB per version on a Pi's SD
// card is the constraint, and deleting the version the daemon is configured to
// run would trade a full card for a broken install.
func (i *Installer) Prune(ctx context.Context, keep int, protected ...string) error {
	if keep < 1 {
		keep = 1
	}

	installed, err := i.Installed(ctx)
	if err != nil {
		return err
	}

	// Keep the newest `keep`, PLUS any protected version not already among them.
	//
	// Expressed in that order deliberately. A combined budget of
	// keep+len(protected) let a protected version that was already among the
	// newest buy an extra slot it never used, so keep=2 retained three — roughly
	// 920 MB on the SD card the policy exists to protect — and duplicates in
	// protected made it worse.
	keepSet := make(map[string]struct{}, keep+len(protected))
	for _, v := range installed {
		if len(keepSet) >= keep {
			break
		}
		keepSet[v] = struct{}{}
	}

	// Protected versions survive whether or not they are recent. Only ones that
	// are actually installed matter; the rest are not ours to keep.
	installedSet := make(map[string]struct{}, len(installed))
	for _, v := range installed {
		installedSet[v] = struct{}{}
	}
	for _, v := range protected {
		if _, ok := installedSet[v]; ok {
			keepSet[v] = struct{}{}
		}
	}

	for _, v := range installed {
		if _, ok := keepSet[v]; ok {
			continue
		}
		if err := os.RemoveAll(filepath.Join(i.root, v)); err != nil {
			return fmt.Errorf("remove %s: %w", v, err)
		}
	}
	return nil
}

// sortVersionsNewestFirst orders semver-ish versions, newest first.
//
// The manifest uses bare versions ("2.1.233"); semver.Compare wants a leading
// "v". Anything unparseable sorts last rather than being dropped, so debris is
// visible to Prune rather than accumulating invisibly.
func sortVersionsNewestFirst(versions []string) {
	sort.SliceStable(versions, func(a, b int) bool {
		va, vb := "v"+versions[a], "v"+versions[b]
		switch {
		case semver.IsValid(va) && !semver.IsValid(vb):
			return true
		case !semver.IsValid(va) && semver.IsValid(vb):
			return false
		case !semver.IsValid(va) && !semver.IsValid(vb):
			return versions[a] > versions[b]
		default:
			return semver.Compare(va, vb) > 0
		}
	})
}
