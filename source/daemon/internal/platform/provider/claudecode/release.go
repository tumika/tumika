// Package claudecode drives the vendored Claude Code CLI.
//
// tumika installs its own copy rather than using whatever is on PATH, at an
// exact pinned version, verified against Anthropic's signed manifest. The pin is
// the contract: the interactive login flow parses a specific version's terminal
// output, so a silent change of build is a silent break (ADR-0001).
package claudecode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"time"

	_ "embed"

	"github.com/ProtonMail/go-crypto/openpgp"
)

// DefaultBaseURL is Anthropic's release bucket.
const DefaultBaseURL = "https://downloads.claude.ai/claude-code-releases"

// SigningKeyFingerprint is the key tumika will accept a manifest from, and
// nothing else.
//
// Verified by hand against the real signature for 2.1.233: the detached
// signature's issuer fingerprint is this value, and the key carries the UID
// "Anthropic Claude Code Release Signing <security@anthropic.com>".
const SigningKeyFingerprint = "31DDDE24DDFAB679F42D7BD2BAA929FF1A7ECACE"

// signingKey is Anthropic's release signing key, embedded rather than fetched.
//
// Embedding is the point: a key downloaded at install time from the same place
// as the artifact proves nothing, because whoever could tamper with one could
// tamper with the other. This file is the trust anchor, and changing it is a
// deliberate act visible in a diff. It was obtained from keyserver.ubuntu.com by
// fingerprint and confirmed to produce a good signature over the real manifest,
// and a bad one over a manifest altered by a single byte.
//
//go:embed anthropic-release-key.asc
var signingKey []byte

// maxManifestBytes bounds the manifest read. It is a small JSON document; a
// bucket serving something enormous is not something to stream into memory.
const maxManifestBytes = 1 << 20 // 1 MiB

// maxBinaryBytes bounds the binary download. The linux-arm64 build is ~320 MB,
// so this leaves generous headroom while still refusing an unbounded stream.
const maxBinaryBytes = 1 << 30 // 1 GiB

var (
	// ErrUnsignedManifest means the manifest was not signed by the expected key.
	// Never recoverable by retrying, and never worth ignoring.
	ErrUnsignedManifest = errors.New("manifest signature is not from Anthropic's release key")

	// ErrChecksumMismatch means the downloaded binary is not the one the signed
	// manifest describes.
	ErrChecksumMismatch = errors.New("downloaded binary does not match the signed checksum")

	// ErrPlatformUnsupported means the manifest has no build for this machine.
	ErrPlatformUnsupported = errors.New("no Claude Code build for this platform")
)

// Manifest is a release's signed description of its artifacts.
type Manifest struct {
	Version   string              `json:"version"`
	Commit    string              `json:"commit"`
	BuildDate string              `json:"buildDate"`
	Platforms map[string]Platform `json:"platforms"`
}

// Platform is one build within a manifest.
type Platform struct {
	Binary   string `json:"binary"`
	Checksum string `json:"checksum"`
	Size     int64  `json:"size"`
}

// PlatformKey is how the manifest names this machine's build.
//
// Anthropic uses the Node convention (x64, arm64) rather than Go's (amd64), so
// the two have to be translated rather than assumed equal — an assumption that
// would fail only on amd64, which is exactly where nobody would notice during
// development on Apple silicon.
func PlatformKey(goos, goarch string) (string, error) {
	arch, ok := map[string]string{"amd64": "x64", "arm64": "arm64"}[goarch]
	if !ok {
		return "", fmt.Errorf("%w: architecture %s", ErrPlatformUnsupported, goarch)
	}

	switch goos {
	case "darwin", "linux":
		return goos + "-" + arch, nil
	default:
		return "", fmt.Errorf("%w: %s", ErrPlatformUnsupported, goos)
	}
}

// releaseClient fetches and verifies release artifacts.
type releaseClient struct {
	baseURL string
	client  *http.Client
	keyring openpgp.EntityList
	// expectFingerprint is the signer Manifest will accept. A field rather than
	// the constant directly so a test can substitute a whole trust anchor —
	// keyring AND expected signer together. Substituting only the keyring would
	// leave the two halves disagreeing, which is not a configuration that can
	// exist in production.
	expectFingerprint string
	platform          string
}

func newReleaseClient(baseURL string, httpClient *http.Client) (*releaseClient, error) {
	keyring, err := openpgp.ReadArmoredKeyRing(strings.NewReader(string(signingKey)))
	if err != nil {
		return nil, fmt.Errorf("read the embedded signing key: %w", err)
	}
	if err := assertFingerprint(keyring); err != nil {
		return nil, err
	}

	platform, err := PlatformKey(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return nil, err
	}

	if httpClient == nil {
		// Generous: the linux-arm64 binary is ~320 MB, and a Pi on a domestic
		// connection is a supported deployment.
		httpClient = &http.Client{Timeout: 30 * time.Minute}
	}

	return &releaseClient{
		baseURL:           strings.TrimSuffix(baseURL, "/"),
		client:            httpClient,
		keyring:           keyring,
		expectFingerprint: SigningKeyFingerprint,
		platform:          platform,
	}, nil
}

// assertFingerprint checks the embedded keyring holds exactly the key we mean.
//
// The fingerprint is the constant an operator can check against Anthropic's
// published value; the key file is bytes nobody reads. Checking one against the
// other at startup is what stops swapping the file silently changing who tumika
// trusts.
//
// EXACTLY ONE entity, not "at least one that matches". A keyring with the right
// key plus an attacker's would have satisfied a contains-check while making the
// attacker a trusted signer — and the manifest verification would have accepted
// their signature. Manifest() also checks the signer's own fingerprint, so this
// is belt and braces on the property that matters most in the package.
func assertFingerprint(keyring openpgp.EntityList) error {
	if len(keyring) != 1 {
		return fmt.Errorf("%w: the embedded keyring holds %d keys, expected exactly 1",
			ErrUnsignedManifest, len(keyring))
	}

	got := strings.ToUpper(hex.EncodeToString(keyring[0].PrimaryKey.Fingerprint))
	if got != SigningKeyFingerprint {
		return fmt.Errorf("%w: the embedded key is %s, expected %s",
			ErrUnsignedManifest, got, SigningKeyFingerprint)
	}
	return nil
}

// Stable reports the version the bucket currently calls stable.
//
// Informational only: tumika installs its PINNED version, which may deliberately
// differ. At the time of writing stable was behind the pin.
func (c *releaseClient) Stable(ctx context.Context) (string, error) {
	body, err := c.get(ctx, "/stable", 64)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(body)), nil
}

// Manifest fetches a version's manifest and verifies its detached signature
// against the embedded key.
//
// Everything downstream trusts the checksums in this document, so this is the
// single point where that trust is established. A manifest that does not verify
// is not used to check anything.
func (c *releaseClient) Manifest(ctx context.Context, version string) (Manifest, error) {
	raw, err := c.get(ctx, "/"+version+"/manifest.json", maxManifestBytes)
	if err != nil {
		return Manifest{}, err
	}

	sig, err := c.get(ctx, "/"+version+"/manifest.json.sig", maxManifestBytes)
	if err != nil {
		return Manifest{}, fmt.Errorf("fetch the manifest signature: %w", err)
	}

	signer, err := openpgp.CheckArmoredDetachedSignature(
		c.keyring, strings.NewReader(string(raw)), strings.NewReader(string(sig)), nil)
	if err != nil {
		return Manifest{}, fmt.Errorf("%w: %w", ErrUnsignedManifest, err)
	}

	// Check WHO signed it, not merely that the keyring contains the right key.
	//
	// CheckArmoredDetachedSignature accepts a signature from ANY entity in the
	// keyring and returns which one; discarding that meant the guarantee rested
	// entirely on the keyring having exactly one member. Appending a second
	// armored key to the embedded file would have made it a fully trusted
	// signer while every existing check still passed — which is exactly the
	// "swapping the file cannot change who tumika trusts" property the embedded
	// key is supposed to provide.
	if signer == nil {
		return Manifest{}, fmt.Errorf("%w: the signature names no key", ErrUnsignedManifest)
	}
	got := strings.ToUpper(hex.EncodeToString(signer.PrimaryKey.Fingerprint))
	if got != c.expectFingerprint {
		return Manifest{}, fmt.Errorf("%w: signed by %s, expected %s",
			ErrUnsignedManifest, got, c.expectFingerprint)
	}

	var manifest Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("parse the manifest: %w", err)
	}

	// A manifest that is validly signed but describes a different version would
	// mean a replay of an old signed document to install the wrong build.
	if manifest.Version != version {
		return Manifest{}, fmt.Errorf("%w: asked for %s, the signed manifest describes %s",
			ErrUnsignedManifest, version, manifest.Version)
	}

	return manifest, nil
}

// Download fetches this platform's binary and verifies it against the manifest's
// checksum, writing it to w.
//
// The hash is computed while streaming, so the 320 MB binary is never held in
// memory — but nothing is installed until the digest matches, which is the
// caller's job to enforce by writing to a temporary file.
func (c *releaseClient) Download(ctx context.Context, version string, manifest Manifest, w io.Writer) error {
	platform, ok := manifest.Platforms[c.platform]
	if !ok {
		return fmt.Errorf("%w: %s is not in the manifest for %s", ErrPlatformUnsupported, c.platform, version)
	}

	url := c.baseURL + "/" + version + "/" + c.platform + "/" + platform.Binary
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build the download request: %w", err)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch %s: %s", url, resp.Status)
	}

	digest := sha256.New()
	written, err := io.Copy(io.MultiWriter(w, digest), io.LimitReader(resp.Body, maxBinaryBytes))
	if err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}

	if got := hex.EncodeToString(digest.Sum(nil)); !strings.EqualFold(got, platform.Checksum) {
		return fmt.Errorf("%w: got %s, the signed manifest says %s", ErrChecksumMismatch, got, platform.Checksum)
	}

	// A truncated download that happened to hash correctly is not possible, but
	// a size mismatch means the manifest and the artifact disagree about what
	// this release is, which is worth refusing on its own.
	if platform.Size > 0 && written != platform.Size {
		return fmt.Errorf("%w: downloaded %d bytes, the manifest says %d",
			ErrChecksumMismatch, written, platform.Size)
	}

	return nil
}

func (c *releaseClient) get(ctx context.Context, path string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("build a request for %s: %w", path, err)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: %s", path, resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return body, nil
}
