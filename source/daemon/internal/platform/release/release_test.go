package release

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// published is the instant the fixtures date a release at. Tests that care
// about publication order offset it.
var published = time.Date(2026, 9, 20, 7, 28, 0, 0, time.UTC)

// host is a release host under the test's control: it serves documents,
// detached signatures and assets, and records every path asked for.
type host struct {
	t      *testing.T
	key    *ecdsa.PrivateKey
	mux    *http.ServeMux
	server *httptest.Server

	mu       sync.Mutex
	requests []string
}

func newHost(t *testing.T) *host {
	t.Helper()
	h := &host{t: t, key: newSigningKey(t), mux: http.NewServeMux()}
	h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.requests = append(h.requests, r.URL.Path)
		h.mu.Unlock()
		h.mux.ServeHTTP(w, r)
	}))
	t.Cleanup(h.server.Close)
	return h
}

// source points a daemon at this host, verifying against this host's key.
func (h *host) source(opts ...Option) *GitHub {
	h.t.Helper()
	base := []Option{
		WithBaseURL(h.server.URL),
		WithHTTPClient(h.server.Client()),
		WithPlatform("linux", "arm64"),
		WithKeys([]*ecdsa.PublicKey{&h.key.PublicKey}),
	}
	return NewGitHub(append(base, opts...)...)
}

func (h *host) paths() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.requests...)
}

func (h *host) serveBytes(path string, body []byte) {
	h.mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	})
}

// serveDocument publishes a document and the detached signature beside it.
func (h *host) serveDocument(path, body string) {
	h.serveBytes(path, []byte(body))
	h.serveBytes(path+signatureSuffix, signBOM(h.t, body, h.key))
}

// serveChannel publishes a channel head.
func (h *host) serveChannel(bom BOM) {
	h.serveDocument("/channels/"+string(bom.Channel)+".json", encodeBOM(h.t, bom))
}

// serveRelease publishes a release's own document.
func (h *host) serveRelease(bom BOM) {
	h.serveDocument("/releases/"+bom.Release+".json", encodeBOM(h.t, bom))
}

// serveBinary publishes an asset and returns the entry a bill of materials
// would carry for it.
func (h *host) serveBinary(name, body string) Asset {
	h.serveBytes("/download/"+name, []byte(body))
	sum := sha256.Sum256([]byte(body))
	return Asset{URL: h.server.URL + "/download/" + name, SHA256: hex.EncodeToString(sum[:])}
}

func encodeBOM(t *testing.T, bom BOM) string {
	t.Helper()
	data, err := json.MarshalIndent(bom, "", "  ")
	if err != nil {
		t.Fatalf("encode the bill of materials: %v", err)
	}
	return string(data)
}

// daemonBOM is a release shipping one daemon asset for linux/arm64.
func daemonBOM(release string, channel Channel, at time.Time, version string, asset Asset) BOM {
	return BOM{
		Release:     release,
		Channel:     channel,
		PublishedAt: at,
		Components: map[string]Component{
			DaemonComponent: {
				Version: version,
				Assets:  map[string]Asset{PlatformKey("linux", "arm64"): asset},
			},
		},
	}
}

// A daemon follows the channel it is set to, and only that one. The three
// documents differ, so reading the wrong one is visible rather than harmless.
func TestHeadReadsTheChannelItIsAsked(t *testing.T) {
	h := newHost(t)
	asset := h.serveBinary("tumika", "the new binary")
	h.serveChannel(daemonBOM("2026.09.00", ChannelStable, published, "0.0.1", asset))
	h.serveChannel(daemonBOM("2026.09.01-beta.1", ChannelBeta, published.Add(time.Hour), "0.0.2", asset))
	h.serveChannel(daemonBOM("edge.147", ChannelEdge, published.Add(2*time.Hour), "0.0.2-edge.147", asset))
	src := h.source()

	for _, tc := range []struct {
		channel Channel
		release string
		version string
		at      time.Time
	}{
		{ChannelStable, "2026.09.00", "0.0.1", published},
		{ChannelBeta, "2026.09.01-beta.1", "0.0.2", published.Add(time.Hour)},
		{ChannelEdge, "edge.147", "0.0.2-edge.147", published.Add(2 * time.Hour)},
	} {
		head, err := src.Head(context.Background(), tc.channel)
		if err != nil {
			t.Fatalf("Head(%s): %v", tc.channel, err)
		}
		if head.Release != tc.release || head.Version != tc.version {
			t.Errorf("Head(%s) = release %q version %q, want %q / %q",
				tc.channel, head.Release, head.Version, tc.release, tc.version)
		}
		if !head.PublishedAt.Equal(tc.at) {
			t.Errorf("Head(%s) published_at = %s, want %s", tc.channel, head.PublishedAt, tc.at)
		}
		if head.Asset != asset {
			t.Errorf("Head(%s) asset = %+v, want %+v", tc.channel, head.Asset, asset)
		}
	}
}

// A channel with nothing published is a NORMAL state before the first release,
// not a failure — the runner logs it at debug and carries on.
func TestHeadOnAChannelWithNothingPublished(t *testing.T) {
	h := newHost(t)
	src := h.source()

	if _, err := src.Head(context.Background(), ChannelStable); !errors.Is(err, ErrNoRelease) {
		t.Fatalf("= %v, want ErrNoRelease", err)
	}
}

// Anything else is a real failure and must not be mistaken for "no release",
// which would silently stop a fleet from ever updating.
func TestHeadOnAServerError(t *testing.T) {
	h := newHost(t)
	h.mux.HandleFunc("/channels/stable.json", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	src := h.source()

	_, err := src.Head(context.Background(), ChannelStable)
	if err == nil {
		t.Fatal("a 500 was accepted")
	}
	if errors.Is(err, ErrNoRelease) {
		t.Error("a server error was reported as 'no release', which would stop updates silently")
	}
}

// THE property of the trust chain: a document signed by a key the daemon does
// not carry names bytes the daemon must never execute.
func TestHeadRefusesADocumentSignedByAnUnknownKey(t *testing.T) {
	h := newHost(t)
	asset := h.serveBinary("tumika", "the new binary")
	body := encodeBOM(t, daemonBOM("2026.09.00", ChannelStable, published, "0.0.1", asset))
	h.serveBytes("/channels/stable.json", []byte(body))
	h.serveBytes("/channels/stable.json"+signatureSuffix, signBOM(t, body, newSigningKey(t)))
	src := h.source()

	if _, err := src.Head(context.Background(), ChannelStable); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("= %v, want ErrBadSignature", err)
	}
}

// A document served without its signature is refused exactly as a tampered one
// is. Falling back to "unsigned is better than nothing" is the whole attack.
func TestHeadRefusesAnUnsignedDocument(t *testing.T) {
	h := newHost(t)
	asset := h.serveBinary("tumika", "the new binary")
	h.serveBytes("/channels/stable.json",
		[]byte(encodeBOM(t, daemonBOM("2026.09.00", ChannelStable, published, "0.0.1", asset))))
	src := h.source()

	if _, err := src.Head(context.Background(), ChannelStable); !errors.Is(err, ErrUnsignedBOM) {
		t.Fatalf("= %v, want ErrUnsignedBOM", err)
	}
}

// A stable document copied to the edge path would put an edge follower on a
// release its channel never offered, and the signature over it is perfectly
// valid — only the channel inside the document says so.
func TestHeadRefusesADocumentFromAnotherChannelsPath(t *testing.T) {
	h := newHost(t)
	asset := h.serveBinary("tumika", "the new binary")
	h.serveDocument("/channels/edge.json",
		encodeBOM(t, daemonBOM("2026.09.00", ChannelStable, published, "0.0.1", asset)))
	src := h.source()

	if _, err := src.Head(context.Background(), ChannelEdge); !errors.Is(err, ErrMalformedBOM) {
		t.Fatalf("= %v, want ErrMalformedBOM", err)
	}
}

// A release that published nothing for this platform is a clear error, not a
// mystery. It happens when a build target is dropped.
func TestHeadWithNoAssetForThisPlatform(t *testing.T) {
	h := newHost(t)
	asset := h.serveBinary("tumika", "the new binary")
	bom := daemonBOM("2026.09.00", ChannelStable, published, "0.0.1", asset)
	bom.Components[DaemonComponent] = Component{
		Version: "0.0.1",
		Assets:  map[string]Asset{PlatformKey("darwin", "arm64"): asset},
	}
	h.serveChannel(bom)
	src := h.source()

	if _, err := src.Head(context.Background(), ChannelStable); !errors.Is(err, ErrNoAsset) {
		t.Fatalf("= %v, want ErrNoAsset", err)
	}
}

// A release that ships no daemon at all — a desktop-only hotfix — is the same
// answer: there is nothing here for this binary to install.
func TestHeadOnAReleaseWithoutTheDaemon(t *testing.T) {
	h := newHost(t)
	asset := h.serveBinary("tumika-desktop", "the app")
	bom := daemonBOM("2026.09.00", ChannelStable, published, "0.0.1", asset)
	bom.Components = map[string]Component{
		"desktop": {Version: "0.0.1", Assets: map[string]Asset{PlatformKey("linux", "arm64"): asset}},
	}
	h.serveChannel(bom)
	src := h.source()

	if _, err := src.Head(context.Background(), ChannelStable); !errors.Is(err, ErrNoAsset) {
		t.Fatalf("= %v, want ErrNoAsset", err)
	}
}

// The channel reaches a URL path, so a name outside the set is refused before
// anything is requested.
func TestHeadRefusesAnUnknownChannel(t *testing.T) {
	h := newHost(t)
	src := h.source()

	if _, err := src.Head(context.Background(), Channel("../releases/edge")); !errors.Is(err, ErrInvalidChannel) {
		t.Fatalf("= %v, want ErrInvalidChannel", err)
	}
	if paths := h.paths(); len(paths) != 0 {
		t.Errorf("an unknown channel reached the host: %v", paths)
	}
}

// The document is read whole in order to verify a signature over its exact
// bytes, so the host would otherwise choose how much memory the daemon
// allocates.
func TestHeadRefusesAnOversizeDocument(t *testing.T) {
	h := newHost(t)
	h.serveBytes("/channels/stable.json", make([]byte, maxBOMBytes+1))
	h.serveBytes("/channels/stable.json"+signatureSuffix, []byte("ignored"))
	src := h.source()

	_, err := src.Head(context.Background(), ChannelStable)
	if err == nil {
		t.Fatal("an oversize document was read")
	}
	if !strings.Contains(err.Error(), "cap") {
		t.Fatalf("= %v, want the size cap to be the reason", err)
	}
	if got := len(h.paths()); got != 1 {
		t.Errorf("%d requests, want only the document itself: the signature must not be fetched", got)
	}
}

// The daemon learns when its own release was published by reading that
// release's document.
func TestReleaseBOMReadsOneRelease(t *testing.T) {
	h := newHost(t)
	asset := h.serveBinary("tumika", "the new binary")
	h.serveRelease(daemonBOM("2026.09.01", ChannelStable, published, "0.0.2", asset))
	src := h.source()

	bom, err := src.ReleaseBOM(context.Background(), "2026.09.01")
	if err != nil {
		t.Fatalf("ReleaseBOM: %v", err)
	}
	if !bom.PublishedAt.Equal(published) {
		t.Errorf("published_at = %s, want %s", bom.PublishedAt, published)
	}
	if bom.Components[DaemonComponent].Version != "0.0.2" {
		t.Errorf("daemon version = %q", bom.Components[DaemonComponent].Version)
	}
}

// A pruned edge release is a missing document, and the caller treats that as
// "older than anything published" rather than as a failure.
func TestReleaseBOMOnAPrunedRelease(t *testing.T) {
	h := newHost(t)
	src := h.source()

	if _, err := src.ReleaseBOM(context.Background(), "edge.12"); !errors.Is(err, ErrNoRelease) {
		t.Fatalf("= %v, want ErrNoRelease", err)
	}
}

// The label comes from a build stamp, so it is attacker-influenced input that
// ends up in a URL path. Every one of these must be refused before a request is
// made — an escaped segment would fetch a document from somewhere else on the
// host entirely.
func TestReleaseBOMRefusesAHostileLabel(t *testing.T) {
	h := newHost(t)
	src := h.source()

	labels := []string{
		"../x",
		"a/b",
		"%2e%2e",
		"..%2fchannels%2fedge",
		"",
		"2026.09.00\n",
		"2026.09.00/../edge.1",
		"dev",
		"2026.9.0",
		"http://elsewhere.example/x",
	}
	for _, label := range labels {
		if _, err := src.ReleaseBOM(context.Background(), label); !errors.Is(err, ErrInvalidReleaseLabel) {
			t.Errorf("ReleaseBOM(%q) = %v, want ErrInvalidReleaseLabel", label, err)
		}
	}
	if paths := h.paths(); len(paths) != 0 {
		t.Errorf("a hostile label reached the host: %v", paths)
	}
}

// A document that verifies but describes a different release means the host
// answered the wrong path, and following it would date the running build by
// somebody else's release.
func TestReleaseBOMRefusesALabelItDoesNotDescribe(t *testing.T) {
	h := newHost(t)
	asset := h.serveBinary("tumika", "the new binary")
	h.serveDocument("/releases/2026.09.01.json",
		encodeBOM(t, daemonBOM("2026.09.00", ChannelStable, published, "0.0.1", asset)))
	src := h.source()

	if _, err := src.ReleaseBOM(context.Background(), "2026.09.01"); !errors.Is(err, ErrMalformedBOM) {
		t.Fatalf("= %v, want ErrMalformedBOM", err)
	}
}

// A daemon carrying only the compiled-in release keys cannot verify a document
// a test signs, and it refuses rather than accepting one.
func TestAReleaseKeyedDaemonRefusesATestSignature(t *testing.T) {
	h := newHost(t)
	asset := h.serveBinary("tumika", "the new binary")
	h.serveChannel(daemonBOM("2026.09.00", ChannelStable, published, "0.0.1", asset))
	src := NewGitHub(
		WithBaseURL(h.server.URL),
		WithHTTPClient(h.server.Client()),
		WithPlatform("linux", "arm64"),
	)

	if _, err := src.Head(context.Background(), ChannelStable); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("= %v, want ErrBadSignature", err)
	}
}

func TestLatestIsTheStableHead(t *testing.T) {
	h := newHost(t)
	asset := h.serveBinary("tumika", "the new binary")
	h.serveChannel(daemonBOM("2026.09.00", ChannelStable, published, "0.0.1", asset))
	h.serveChannel(daemonBOM("edge.147", ChannelEdge, published.Add(time.Hour), "0.0.2-edge.147", asset))
	src := h.source()

	got, err := src.Latest(context.Background())
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if got != "0.0.1" {
		t.Errorf("Latest = %q, want the stable head 0.0.1 without a leading v", got)
	}
}

func TestFetchVerifiesAndInstalls(t *testing.T) {
	h := newHost(t)
	asset := h.serveBinary("tumika", "the new binary")
	h.serveChannel(daemonBOM("2026.09.00", ChannelStable, published, "0.0.1", asset))
	src := h.source()

	dest := filepath.Join(t.TempDir(), "tumika")
	if err := src.Fetch(context.Background(), "0.0.1", dest); err != nil {
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

// The head can move between a check and an apply, and installing whatever the
// channel now offers would install a version nothing approved.
func TestFetchRefusesAVersionTheHeadDoesNotShip(t *testing.T) {
	h := newHost(t)
	asset := h.serveBinary("tumika", "the new binary")
	h.serveChannel(daemonBOM("2026.09.00", ChannelStable, published, "0.0.1", asset))
	src := h.source()

	dest := filepath.Join(t.TempDir(), "tumika")
	if err := src.Fetch(context.Background(), "0.0.2", dest); err == nil {
		t.Fatal("a version the head does not ship was installed")
	}
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Error("something was written for a version the head does not ship")
	}
}

// THE property. A substituted or corrupted download must never reach the binary
// path — this is the only thing standing between a compromised mirror and a
// binary the daemon will execute as a service account.
func TestFetchRefusesASHA256Mismatch(t *testing.T) {
	h := newHost(t)
	asset := h.serveBinary("tumika", "a binary nobody published")
	asset.SHA256 = strings.Repeat("0", 64)
	h.serveChannel(daemonBOM("2026.09.00", ChannelStable, published, "0.0.1", asset))
	src := h.source()

	dest := filepath.Join(t.TempDir(), "tumika")
	if err := src.Fetch(context.Background(), "0.0.1", dest); !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("= %v, want ErrChecksumMismatch", err)
	}
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Error("a binary that failed verification was written to the destination")
	}
}

// A truncated download hashes differently, so the digest catches it — but the
// point is what is left behind, because the caller is about to exec this path.
func TestFetchLeavesNothingAfterATruncatedDownload(t *testing.T) {
	h := newHost(t)
	full := "the new binary, in full"
	sum := sha256.Sum256([]byte(full))
	h.serveBytes("/download/tumika", []byte(full[:6]))
	asset := Asset{URL: h.server.URL + "/download/tumika", SHA256: hex.EncodeToString(sum[:])}
	h.serveChannel(daemonBOM("2026.09.00", ChannelStable, published, "0.0.1", asset))
	src := h.source()

	dir := t.TempDir()
	dest := filepath.Join(dir, "tumika")
	if err := src.Fetch(context.Background(), "0.0.1", dest); !errors.Is(err, ErrChecksumMismatch) {
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

// A 5xx on the binary itself, with a valid bill of materials — a CDN failing
// mid-release. Nothing must be installed.
func TestFetchWithAServerErrorOnTheBinary(t *testing.T) {
	h := newHost(t)
	h.mux.HandleFunc("/download/tumika", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	asset := Asset{URL: h.server.URL + "/download/tumika", SHA256: strings.Repeat("a", 64)}
	h.serveChannel(daemonBOM("2026.09.00", ChannelStable, published, "0.0.1", asset))
	src := h.source()

	dest := filepath.Join(t.TempDir(), "tumika")
	if err := src.Fetch(context.Background(), "0.0.1", dest); err == nil {
		t.Fatal("a 502 on the binary was accepted")
	}
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Error("something was installed despite the download failing")
	}
}

// An unwritable destination directory is an error, not a silent no-op.
func TestFetchIntoAnUnwritableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the permission bits this relies on")
	}

	h := newHost(t)
	asset := h.serveBinary("tumika", "the new binary")
	h.serveChannel(daemonBOM("2026.09.00", ChannelStable, published, "0.0.1", asset))
	src := h.source()

	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	if err := src.Fetch(context.Background(), "0.0.1", filepath.Join(dir, "tumika")); err == nil {
		t.Fatal("staging into an unwritable directory reported success")
	}
}

// A cancelled context stops the download rather than running to completion.
func TestFetchRespectsCancellation(t *testing.T) {
	h := newHost(t)
	asset := h.serveBinary("tumika", "the new binary")
	h.serveChannel(daemonBOM("2026.09.00", ChannelStable, published, "0.0.1", asset))
	src := h.source()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	dest := filepath.Join(t.TempDir(), "tumika")
	if err := src.Fetch(ctx, "0.0.1", dest); err == nil {
		t.Fatal("a cancelled fetch reported success")
	}
}

// A kill or power cut mid-download leaves a partial file in the LIVE binary's
// directory, and the deferred cleanup only covers the call that made it. On the
// Pi + SD card this targets, repeated failed updates accumulate ~20 MB each
// until the card is full.
func TestFetchSweepsLeftoversFromInterruptedDownloads(t *testing.T) {
	h := newHost(t)
	asset := h.serveBinary("tumika", "the new binary")
	h.serveChannel(daemonBOM("2026.09.00", ChannelStable, published, "0.0.1", asset))
	src := h.source()

	dir := t.TempDir()
	stale := []string{
		filepath.Join(dir, ".tumika-update-111"),
		filepath.Join(dir, ".tumika-update-222"),
	}
	for _, path := range stale {
		if err := os.WriteFile(path, []byte("half a binary"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	// Something that is NOT ours must survive.
	keep := filepath.Join(dir, "unrelated.txt")
	if err := os.WriteFile(keep, []byte("not ours"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := src.Fetch(context.Background(), "0.0.1", filepath.Join(dir, "tumika")); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	for _, path := range stale {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s survived; leftovers accumulate until the disk fills", filepath.Base(path))
		}
	}
	if _, err := os.Stat(keep); err != nil {
		t.Error("the sweep removed a file that was not ours")
	}
}

// An unreachable host is an error, not an empty answer.
func TestHeadWithAnUnreachableHost(t *testing.T) {
	src := NewGitHub(WithBaseURL("http://127.0.0.1:1"))
	if _, err := src.Head(context.Background(), ChannelStable); err == nil {
		t.Fatal("an unreachable host reported a release")
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

// An edge component version is a prerelease of the version it was cut from, so
// semver ranks it below that version. Edge follows publication order for
// exactly this reason.
func TestNewerTreatsAPrereleaseAsOlderThanItsRelease(t *testing.T) {
	if Newer("1.0.0-rc1", "1.0.0") {
		t.Error("an RC was reported as newer than its own release")
	}
	if Newer("0.0.2-edge.147", "0.0.2") {
		t.Error("an edge build was reported as newer than the version it was cut from")
	}
}

// Publication order is what ranks releases, because a release label is never
// compared.
func TestLater(t *testing.T) {
	zero := time.Time{}

	if !Later(published, published.Add(-time.Second)) {
		t.Error("a release published one second later was not reported as later")
	}
	if Later(published, published) {
		t.Error("a release is later than itself, which would reinstall it forever")
	}
	if Later(published, published.Add(time.Second)) {
		t.Error("an earlier release was reported as later")
	}
	// A daemon whose own release cannot be dated — a pruned edge document, or a
	// development build — is offered the head rather than pinned to nothing.
	if !Later(published, zero) {
		t.Error("a published release was not later than an undated one")
	}
	if Later(zero, published) {
		t.Error("an undated document was reported as later")
	}
	if Later(zero, zero) {
		t.Error("two undated documents were ordered")
	}
}
