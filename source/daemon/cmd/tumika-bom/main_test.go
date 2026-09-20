package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tumika/tumika/source/daemon/internal/bomgen"
	"github.com/tumika/tumika/source/daemon/internal/platform/release"
)

// stubSource stands in for the Releases API. The publishing half is what this
// command adds on top of bomgen, and it is testable without a network and
// without the real signing key.
type stubSource struct {
	releases []bomgen.Release
	err      error
}

func (s stubSource) Releases(context.Context) ([]bomgen.Release, error) {
	return s.releases, s.err
}

const daemonDigest = "1111111111111111111111111111111111111111111111111111111111111111"

func publishedRelease(t *testing.T, label, tag string, prerelease bool, at time.Time) bomgen.Release {
	t.Helper()
	return bomgen.Release{
		Tag:         tag,
		Prerelease:  prerelease,
		PublishedAt: at,
		ReleaseYAML: []byte("release: " + label + "\ncomponents:\n  daemon: 0.0.1\n"),
		Assets: []bomgen.Asset{{
			Name:   "tumika_0.0.1_linux_amd64",
			URL:    "https://github.com/tumika/tumika/releases/download/" + tag + "/tumika_0.0.1_linux_amd64",
			SHA256: daemonDigest,
		}},
	}
}

func newKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate a key: %v", err)
	}
	return key
}

func newOptions(t *testing.T, key *ecdsa.PrivateKey, source releaseSource, stderr *bytes.Buffer) options {
	t.Helper()
	return options{
		source:  source,
		key:     key,
		trusted: []*ecdsa.PublicKey{&key.PublicKey},
		outDir:  t.TempDir(),
		stderr:  stderr,
	}
}

func TestRunWritesEveryDocumentBesideAVerifyingSignature(t *testing.T) {
	key := newKey(t)
	var stderr bytes.Buffer
	opts := newOptions(t, key, stubSource{releases: []bomgen.Release{
		publishedRelease(t, "2026.09.00", "v2026.09.00", false, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)),
		publishedRelease(t, "2026.09.01", "v2026.09.01", false, time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)),
	}}, &stderr)

	if err := run(context.Background(), opts); err != nil {
		t.Fatalf("run: %v", err)
	}

	for _, path := range []string{
		"releases/2026.09.00.json",
		"releases/2026.09.01.json",
		"channels/stable.json",
		"channels/beta.json",
		"channels/edge.json",
	} {
		body, err := os.ReadFile(filepath.Join(opts.outDir, filepath.FromSlash(path)))
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		signature, err := os.ReadFile(filepath.Join(opts.outDir, filepath.FromSlash(path+".sig")))
		if err != nil {
			t.Fatalf("read %s.sig: %v", path, err)
		}
		// The bytes on disk are the bytes signed: verification runs over what
		// was written, not over anything still held in memory.
		bom, err := release.VerifyBOM(body, signature, []*ecdsa.PublicKey{&key.PublicKey})
		if err != nil {
			t.Fatalf("%s does not verify: %v", path, err)
		}
		if bom.Components[release.DaemonComponent].Assets["linux_amd64"].SHA256 != daemonDigest {
			t.Fatalf("%s lost the published digest", path)
		}

		other := newKey(t)
		if _, err := release.VerifyBOM(body, signature, []*ecdsa.PublicKey{&other.PublicKey}); !errors.Is(err, release.ErrBadSignature) {
			t.Fatalf("%s verified against an unrelated key: %v", path, err)
		}
	}

	// The stable head is the most recently published stable release.
	head, err := os.ReadFile(filepath.Join(opts.outDir, "channels", "stable.json"))
	if err != nil {
		t.Fatalf("read the stable head: %v", err)
	}
	if !strings.Contains(string(head), `"release": "2026.09.01"`) {
		t.Fatalf("the stable head does not name the newest release:\n%s", head)
	}
}

func TestRunRefusesASigningKeyNoDaemonTrusts(t *testing.T) {
	key := newKey(t)
	var stderr bytes.Buffer
	opts := newOptions(t, key, stubSource{releases: []bomgen.Release{
		publishedRelease(t, "2026.09.00", "v2026.09.00", false, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)),
	}}, &stderr)
	opts.trusted = []*ecdsa.PublicKey{&newKey(t).PublicKey}

	err := run(context.Background(), opts)
	if err == nil {
		t.Fatal("published with a key no daemon verifies with")
	}
	if !strings.Contains(err.Error(), "release key") {
		t.Fatalf("the error does not say what was refused: %v", err)
	}
	if entries, readErr := os.ReadDir(opts.outDir); readErr != nil || len(entries) != 0 {
		t.Fatalf("a refused run still wrote into the output tree: %v %v", entries, readErr)
	}
	assertNoKeyMaterial(t, key, stderr.String()+err.Error())
}

func TestRunFailsOnASkippedReleaseUnlessSkipsAreAllowed(t *testing.T) {
	good := publishedRelease(t, "2026.09.00", "v2026.09.00", false, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	bad := bomgen.Release{Tag: "not-a-release-tag", PublishedAt: time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)}

	key := newKey(t)
	var stderr bytes.Buffer
	opts := newOptions(t, key, stubSource{releases: []bomgen.Release{good, bad}}, &stderr)

	err := run(context.Background(), opts)
	if err == nil {
		t.Fatal("a skipped release did not fail the run")
	}
	if !strings.Contains(stderr.String(), "not-a-release-tag") {
		t.Fatalf("the skip reason was not reported:\n%s", stderr.String())
	}

	stderr.Reset()
	allowed := newOptions(t, key, stubSource{releases: []bomgen.Release{good, bad}}, &stderr)
	allowed.allowSkips = true
	if err := run(context.Background(), allowed); err != nil {
		t.Fatalf("run with -allow-skips: %v", err)
	}
	if _, err := os.Stat(filepath.Join(allowed.outDir, "releases", "2026.09.00.json")); err != nil {
		t.Fatalf("the releases that could be published were not: %v", err)
	}
	if !strings.Contains(stderr.String(), "not-a-release-tag") {
		t.Fatalf("an allowed skip was not reported:\n%s", stderr.String())
	}
}

func TestSigningKeyReadsBothPEMSpellings(t *testing.T) {
	key := newKey(t)
	sec1, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal SEC1: %v", err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal PKCS#8: %v", err)
	}

	for name, block := range map[string]*pem.Block{
		"EC PRIVATE KEY": {Type: "EC PRIVATE KEY", Bytes: sec1},
		"PRIVATE KEY":    {Type: "PRIVATE KEY", Bytes: pkcs8},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(signingKeyEnv, string(pem.EncodeToMemory(block)))
			read, err := signingKey()
			if err != nil {
				t.Fatalf("signingKey: %v", err)
			}
			if !read.PublicKey.Equal(&key.PublicKey) {
				t.Fatal("signingKey read a different key")
			}
		})
	}
}

func TestSigningKeyErrorsCarryNoKeyMaterial(t *testing.T) {
	key := newKey(t)
	sec1, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal SEC1: %v", err)
	}
	// A PEM block whose body is the real key but whose type is wrong, and a
	// value that is not PEM at all: both are reported without quoting what was
	// read, because a CI log keeps whatever an error printed.
	mistyped := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: sec1}))
	for name, value := range map[string]string{
		"mistyped block": mistyped,
		"not pem":        base64.StdEncoding.EncodeToString(sec1),
		"empty":          "  \n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(signingKeyEnv, value)
			read, err := signingKey()
			if err == nil {
				t.Fatalf("read a key out of %q", name)
			}
			if read != nil {
				t.Fatal("a failed read still returned a key")
			}
			assertNoKeyMaterial(t, key, err.Error())
			if strings.Contains(err.Error(), value) {
				t.Fatalf("the error quotes what it was given: %v", err)
			}
		})
	}
}

func TestCommandRefusesToRunWithoutASigningKey(t *testing.T) {
	t.Setenv(signingKeyEnv, "")
	var stderr bytes.Buffer
	err := command(context.Background(), []string{"-out", t.TempDir()}, &stderr)
	if err == nil || !strings.Contains(err.Error(), signingKeyEnv) {
		t.Fatalf("command without a key: %v", err)
	}
}

func TestNewGitHubSourceRefusesAnUnusableTarget(t *testing.T) {
	if _, err := newGitHubSource("https://api.github.com", "tumika", ""); err == nil {
		t.Fatal("a repository without an owner was accepted")
	}
	if _, err := newGitHubSource("api.github.com", "tumika/tumika", ""); err == nil {
		t.Fatal("a relative API base was accepted")
	}
	if _, err := newGitHubSource("https://api.github.com/", "tumika/tumika", ""); err != nil {
		t.Fatalf("newGitHubSource: %v", err)
	}
}

func TestDestinationRefusesAPathOutsideTheOutputTree(t *testing.T) {
	out := t.TempDir()
	if _, err := destination(out, "../escaped.json"); err == nil {
		t.Fatal("a path above the output tree was accepted")
	}
	got, err := destination(out, "releases/2026.09.00.json")
	if err != nil {
		t.Fatalf("destination: %v", err)
	}
	if want := filepath.Join(out, "releases", "2026.09.00.json"); got != want {
		t.Fatalf("destination = %q, want %q", got, want)
	}
}

func TestGitHubSourceResolvesDigestsFromChecksums(t *testing.T) {
	var (
		listed int
		base   string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/repos/tumika/tumika/releases"):
			listed++
			if r.URL.Query().Get("page") != "1" {
				_, _ = io.WriteString(w, `[]`)
				return
			}
			_, _ = io.WriteString(w, `[
			  {"tag_name":"v2026.09.00","draft":false,"prerelease":false,
			   "published_at":"2026-09-01T00:00:00Z",
			   "assets":[
			     {"name":"checksums.txt","browser_download_url":"`+base+`/checksums.txt"},
			     {"name":"release.yaml","browser_download_url":"`+base+`/release.yaml"},
			     {"name":"tumika_0.0.1_linux_amd64","browser_download_url":"https://example.test/bin"}]},
			  {"tag_name":"v2026.09.01","draft":true,"prerelease":false,
			   "published_at":"2026-09-08T00:00:00Z",
			   "assets":[{"name":"checksums.txt","browser_download_url":"https://unreachable.test/x"}]}
			]`)
		case r.URL.Path == "/checksums.txt":
			_, _ = io.WriteString(w, daemonDigest+"  tumika_0.0.1_linux_amd64\n")
		case r.URL.Path == "/release.yaml":
			_, _ = io.WriteString(w, "release: 2026.09.00\ncomponents:\n  daemon: 0.0.1\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	base = server.URL

	source, err := newGitHubSource(server.URL, "tumika/tumika", "test-token")
	if err != nil {
		t.Fatalf("newGitHubSource: %v", err)
	}
	source.client = server.Client()

	releases, err := source.Releases(context.Background())
	if err != nil {
		t.Fatalf("Releases: %v", err)
	}
	if len(releases) != 2 {
		t.Fatalf("Releases returned %d entries", len(releases))
	}
	// A short page is the last one, so the walk stops without asking for
	// another.
	if listed != 1 {
		t.Fatalf("the release list was requested %d times", listed)
	}
	first := releases[0]
	if first.Tag != "v2026.09.00" || len(first.ReleaseYAML) == 0 {
		t.Fatalf("release.yaml was not read: %+v", first)
	}
	var found bool
	for _, a := range first.Assets {
		if a.Name == "tumika_0.0.1_linux_amd64" {
			found = a.SHA256 == daemonDigest
		}
	}
	if !found {
		t.Fatalf("the binary's digest was not resolved from checksums.txt: %+v", first.Assets)
	}
	// A draft's assets are never fetched: its checksums.txt URL resolves
	// nowhere, so reaching for it would have failed the run.
	if !releases[1].Draft || len(releases[1].Assets) != 0 {
		t.Fatalf("a draft was read as publishable: %+v", releases[1])
	}
}

func TestParseChecksumsReadsBothDigestSeparators(t *testing.T) {
	digests := parseChecksums([]byte(
		"abc  tumika_0.0.1_linux_amd64\n" +
			"def *tumika_0.0.1_darwin_arm64\n" +
			"\n" +
			"no-separator\n"))
	if digests["tumika_0.0.1_linux_amd64"] != "abc" || digests["tumika_0.0.1_darwin_arm64"] != "def" {
		t.Fatalf("parseChecksums = %v", digests)
	}
	if len(digests) != 2 {
		t.Fatalf("parseChecksums read a line it should have ignored: %v", digests)
	}
}

// assertNoKeyMaterial fails when text carries the private key in any shape this
// command could have leaked it in: the PEM body, its base64, or the raw scalar.
func assertNoKeyMaterial(t *testing.T, key *ecdsa.PrivateKey, text string) {
	t.Helper()
	sec1, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal SEC1: %v", err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal PKCS#8: %v", err)
	}
	raw, err := key.Bytes()
	if err != nil {
		t.Fatalf("read the raw key: %v", err)
	}
	for name, material := range map[string]string{
		"sec1 base64":  base64.StdEncoding.EncodeToString(sec1),
		"pkcs8 base64": base64.StdEncoding.EncodeToString(pkcs8),
		"sec1 bytes":   string(sec1),
		"raw scalar":   string(raw),
		"raw base64":   base64.StdEncoding.EncodeToString(raw),
		"raw hex":      hex.EncodeToString(raw),
	} {
		if strings.Contains(text, material) {
			t.Fatalf("output carries the signing key (%s):\n%s", name, text)
		}
	}
}
