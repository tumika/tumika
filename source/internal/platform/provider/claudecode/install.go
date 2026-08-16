package claudecode

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"

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
	root    string
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
		release: release,
	}, nil
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
		// A directory without the binary is debris from an interrupted install
		// and is not a version anyone can run.
		if _, err := os.Stat(filepath.Join(i.root, entry.Name(), BinaryName)); err != nil {
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
	if version == "" {
		return domain.InstallResult{}, errors.New("no version specified")
	}

	target := i.Path(version)
	if _, err := os.Stat(target); err == nil {
		return domain.InstallResult{Version: version, Path: target, AlreadyPresent: true}, nil
	}

	manifest, err := i.release.Manifest(ctx, version)
	if err != nil {
		return domain.InstallResult{}, err
	}

	// Staged in a sibling of the final directory, so the rename that publishes
	// it cannot cross a filesystem boundary.
	if err := os.MkdirAll(i.root, 0o700); err != nil {
		return domain.InstallResult{}, fmt.Errorf("create %s: %w", i.root, err)
	}
	staging, err := os.MkdirTemp(i.root, ".staging-"+version+"-")
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
	if err := file.Close(); err != nil {
		return domain.InstallResult{}, fmt.Errorf("close %s: %w", staged, err)
	}

	// Published by renaming the whole directory: a reader either sees no version
	// or sees a complete, verified one.
	if err := os.Rename(staging, filepath.Join(i.root, version)); err != nil {
		if errors.Is(err, os.ErrExist) {
			// Another process installed it while we were downloading. Theirs is
			// verified too; nothing to do.
			return domain.InstallResult{Version: version, Path: target, AlreadyPresent: true}, nil
		}
		return domain.InstallResult{}, fmt.Errorf("install %s: %w", version, err)
	}

	return domain.InstallResult{Version: version, Path: target}, nil
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

	keepSet := make(map[string]struct{}, keep+len(protected))
	for _, v := range protected {
		keepSet[v] = struct{}{}
	}
	for _, v := range installed {
		if len(keepSet) >= keep+len(protected) {
			break
		}
		keepSet[v] = struct{}{}
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
